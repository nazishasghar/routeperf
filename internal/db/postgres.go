package db

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/plan"
	"github.com/nazishasghar/routeperf/internal/sqlutil"
)

type pg struct {
	pool    *pgxpool.Pool
	ex      *pgx.Conn
	info    Info
	opt     Options
	logPath string
	prev    map[string]*string // auto.conf values before capture (nil = unset)
	path    string             // search_path
	raw     string
	stream  *exec.Cmd // docker logs -f, when the server runs in a container
	port    string
	cat     map[string]*Table
	parts   map[string]string // partition name → top-level table key
}

var pgCaptureParams = map[string]string{
	"log_min_duration_statement": "0",
	"log_parameter_max_length":   "-1",
	"log_line_prefix":            "%m [%p] rp:%a: ",
}

func openPG(ctx context.Context, raw string, u *url.URL, opt Options) (DB, error) {
	cfg, err := pgxpool.ParseConfig(raw)
	if err != nil {
		return nil, err
	}
	cfg.ConnConfig.RuntimeParams["application_name"] = "routeperf"
	cfg.MaxConns = 4
	if !opt.NoSilence {
		cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
			_, err := c.Exec(ctx, "SET log_min_duration_statement = -1")
			return err
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	cc, _ := pgx.ParseConfig(raw)
	cc.RuntimeParams["application_name"] = "routeperf"
	ex, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("connect postgres: %w", err)
	}
	if !opt.NoSilence {
		if _, err := ex.Exec(ctx, "SET log_min_duration_statement = -1"); err != nil {
			return nil, fmt.Errorf("routeperf needs a superuser (to silence its own statements in the log); use --capture proxy without one: %w", err)
		}
	}
	p := &pg{pool: pool, ex: ex, opt: opt, raw: raw, port: u.Port(), cat: map[string]*Table{}, parts: map[string]string{}}
	if p.port == "" {
		p.port = "5432"
	}
	var ver string
	var super bool
	err = pool.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int/10000, r.rolsuper, current_database(), current_setting('search_path')
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&ver, &p.info.Major, &super, &p.info.Database, &p.path)
	if err != nil {
		return nil, err
	}
	p.info.Dialect, p.info.Version, p.info.Super, p.info.Host = "postgres", ver, super, u.Hostname()
	return p, nil
}

func (p *pg) Info() Info { return p.info }

func (p *pg) Close() {
	p.stopStream()
	p.ex.Close(context.Background())
	p.pool.Close()
}

func qpg(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func (p *pg) Quote(s string) string { return qpg(s) }

// searchPath returns the schemas of the search_path in order.
func (p *pg) searchPath(ctx context.Context) []string {
	var user string
	_ = p.pool.QueryRow(ctx, "SELECT current_user").Scan(&user)
	var out []string
	for _, s := range strings.Split(p.path, ",") {
		s = strings.Trim(strings.TrimSpace(s), `"`)
		if s == "$user" {
			s = user
		}
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

func (p *pg) Tables(ctx context.Context) (map[string]*Table, error) {
	rows, err := p.pool.Query(ctx, `
SELECT c.relname, n.nspname, c.relkind::text, GREATEST(c.reltuples, 0)::float8,
  COALESCE((SELECT array_agg(a.attname ORDER BY k.ord) FROM pg_index i
            CROSS JOIN LATERAL unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, ord)
            JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum
            WHERE i.indrelid = c.oid AND i.indisprimary), '{}'),
  COALESCE((SELECT array_agg(a.attname ORDER BY a.attnum) FROM pg_attribute a
            WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''), '{}'),
  COALESCE((SELECT array_agg(a.attname) FROM pg_attribute a
            WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attnotnull), '{}'),
  COALESCE((SELECT array_agg(DISTINCT a.attname) FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
            WHERE i.indrelid = c.oid), '{}'),
  CASE WHEN c.relkind = 'v' THEN pg_get_viewdef(c.oid, true) ELSE '' END,
  CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) ELSE '' END
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','v','m') AND NOT c.relispartition
  AND n.nspname NOT IN ('pg_catalog','information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE 'pg\_temp%' AND n.nspname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	var all []*Table
	for rows.Next() {
		t := &Table{Indexed: map[string]bool{}, Indexes: map[string][]string{}, NotNull: map[string]bool{}}
		var kind string
		var idx, notNull []string
		if err := rows.Scan(&t.Name, &t.Schema, &kind, &t.Rows, &t.PKCols, &t.Cols, &notNull, &idx, &t.ViewDef, &t.PartKey); err != nil {
			rows.Close()
			return nil, err
		}
		t.Kind = map[string]string{"r": "table", "p": "partitioned", "v": "view", "m": "matview"}[kind]
		if len(t.PKCols) > 0 {
			t.PK = t.PKCols[0]
		}
		for _, c := range idx {
			t.Indexed[strings.ToLower(c)] = true
		}
		for _, c := range notNull {
			t.NotNull[strings.ToLower(c)] = true
		}
		t.SQL = qpg(t.Schema) + "." + qpg(t.Name)
		all = append(all, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := assignKeys(all, p.searchPath(ctx))
	ir, err := p.pool.Query(ctx, `
SELECT n.nspname, c.relname, ic.relname,
  ARRAY(SELECT a.attname FROM unnest(i.indkey::int2[]) WITH ORDINALITY k(attnum, ord)
        JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = k.attnum ORDER BY k.ord)
FROM pg_index i JOIN pg_class c ON c.oid = i.indrelid JOIN pg_class ic ON ic.oid = i.indexrelid
JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname NOT IN ('pg_catalog','information_schema') AND n.nspname NOT LIKE '\_rp\_%' AND NOT c.relispartition`)
	if err != nil {
		return nil, err
	}
	for ir.Next() {
		var sn, tn, in string
		var cols []string
		if err := ir.Scan(&sn, &tn, &in, &cols); err != nil {
			ir.Close()
			return nil, err
		}
		if t := Resolve(out, sn, tn); t != nil {
			for i := range cols {
				cols[i] = strings.ToLower(cols[i])
			}
			t.Indexes[in] = cols
		}
	}
	ir.Close()
	// partitions (any depth), parents first
	pr, err := p.pool.Query(ctx, `
WITH RECURSIVE tree AS (
  SELECT i.inhrelid AS oid, i.inhparent AS parent, 1 AS depth FROM pg_inherits i
  JOIN pg_class c ON c.oid = i.inhrelid WHERE c.relispartition AND c.relkind IN ('r','p','f')
  UNION ALL
  SELECT t.oid, i.inhparent, t.depth + 1 FROM tree t JOIN pg_inherits i ON i.inhrelid = t.parent
  JOIN pg_class c ON c.oid = t.parent WHERE c.relispartition)
SELECT c.relname, n.nspname, pc.relname, top.relname, tn.nspname, COALESCE(pg_get_expr(c.relpartbound, c.oid), 'DEFAULT'),
  CASE WHEN c.relkind = 'p' THEN pg_get_partkeydef(c.oid) ELSE '' END, t.depth
FROM (SELECT oid, max(depth) AS depth FROM tree GROUP BY oid) d
JOIN tree t ON t.oid = d.oid AND t.depth = d.depth
JOIN pg_class c ON c.oid = t.oid JOIN pg_namespace n ON n.oid = c.relnamespace
JOIN pg_inherits pi ON pi.inhrelid = c.oid JOIN pg_class pc ON pc.oid = pi.inhparent
JOIN pg_class top ON top.oid = t.parent JOIN pg_namespace tn ON tn.oid = top.relnamespace
ORDER BY t.depth, c.relname`)
	if err == nil {
		type prow struct {
			part     Partition
			top, tsn string
			depth    int
		}
		var prs []prow
		for pr.Next() {
			var x prow
			if err := pr.Scan(&x.part.Name, &x.part.Schema, &x.part.Parent, &x.top, &x.tsn, &x.part.Bound, &x.part.PartKey, &x.depth); err != nil {
				pr.Close()
				return nil, fmt.Errorf("partitions: %w", err)
			}
			prs = append(prs, x)
		}
		pr.Close()
		for _, x := range prs {
			if t := Resolve(out, x.tsn, x.top); t != nil {
				t.Parts = append(t.Parts, x.part)
				p.parts[strings.ToLower(x.part.Name)] = t.Key
			}
		}
	}
	for _, t := range out {
		if t.Rows == 0 && t.HasData() {
			_ = p.pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*)::float8 FROM %s", t.SQL)).Scan(&t.Rows)
		}
	}
	p.cat = out
	return out, nil
}

// assignKeys gives each relation its catalog key: the bare name for the copy
// that wins search_path resolution, schema.name for the others.
func assignKeys(all []*Table, path []string) map[string]*Table {
	byName := map[string][]*Table{}
	for _, t := range all {
		n := strings.ToLower(t.Name)
		byName[n] = append(byName[n], t)
	}
	rank := func(s string) int {
		for i, x := range path {
			if x == s {
				return i
			}
		}
		return len(path) + 1
	}
	out := map[string]*Table{}
	for n, ts := range byName {
		sort.Slice(ts, func(i, j int) bool { return rank(ts[i].Schema) < rank(ts[j].Schema) })
		for i, t := range ts {
			if i == 0 && (len(ts) == 1 || rank(t.Schema) <= len(path)) {
				t.Key = n
			} else {
				t.Key = strings.ToLower(t.Schema + "." + t.Name)
			}
			out[t.Key] = t
		}
	}
	return out
}

func (p *pg) FKs(ctx context.Context, tables map[string]*Table) ([]FK, error) {
	rows, err := p.pool.Query(ctx, `
SELECT cn.nspname, cl.relname, pn.nspname, pcl.relname,
  ARRAY(SELECT a.attname FROM unnest(c.conkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.n ORDER BY k.o),
  ARRAY(SELECT a.attname FROM unnest(c.confkey) WITH ORDINALITY k(n, o) JOIN pg_attribute a ON a.attrelid = c.confrelid AND a.attnum = k.n ORDER BY k.o),
  c.confdeltype::text
FROM pg_constraint c
JOIN pg_class cl ON cl.oid = c.conrelid JOIN pg_namespace cn ON cn.oid = cl.relnamespace
JOIN pg_class pcl ON pcl.oid = c.confrelid JOIN pg_namespace pn ON pn.oid = pcl.relnamespace
WHERE c.contype = 'f' AND c.conparentid = 0 AND NOT cl.relispartition AND cn.nspname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FK
	for rows.Next() {
		var cs, ct, ps, pt string
		f := FK{Declared: true}
		if err := rows.Scan(&cs, &ct, &ps, &pt, &f.ChildCols, &f.ParentCols, &f.OnDelete); err != nil {
			return nil, err
		}
		child, parent := Resolve(tables, cs, ct), Resolve(tables, ps, pt)
		if child == nil || parent == nil || len(f.ChildCols) == 0 {
			continue
		}
		f.Child, f.Parent, f.ChildCol, f.ParentCol = child.Key, parent.Key, f.ChildCols[0], f.ParentCols[0]
		f.Indexed = fkIndexed(child, f.ChildCols)
		out = append(out, f)
	}
	return MarkCycles(tables, InferFKs(tables, out)), rows.Err()
}

func (p *pg) HashPred(expr string, frac float64) string {
	return fmt.Sprintf("((hashtextextended((%s)::text, 0) %% 10000 + 10000) %% 10000) < %d", expr, int(frac*10000+0.5))
}

func (p *pg) KeyExpr(t *Table, alias string) string {
	switch len(t.PKCols) {
	case 0:
		return alias + "::text"
	case 1:
		return alias + "." + qpg(t.PKCols[0])
	}
	var cs []string
	for _, c := range t.PKCols {
		cs = append(cs, alias+"."+qpg(c))
	}
	return "ROW(" + strings.Join(cs, ", ") + ")"
}

func (p *pg) FirstValues(ctx context.Context, sql string) ([]string, error) {
	rows, err := p.pool.Query(ctx, sql, pgx.QueryExecModeSimpleProtocol)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		if len(vals) > 0 {
			out = append(out, pgText(vals[0]))
		}
	}
	return out, rows.Err()
}

// pgText renders a scanned value the way Postgres would print it as text.
func pgText(v any) string {
	switch x := v.(type) {
	case [16]byte: // uuid
		return fmt.Sprintf("%x-%x-%x-%x-%x", x[0:4], x[4:6], x[6:8], x[8:10], x[10:16])
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	case nil:
		return ""
	}
	return fmt.Sprint(v)
}

// ---------------------------------------------------------------- capture

// dockerContainer finds a running container publishing the DB port.
func (p *pg) dockerContainer() string {
	out, err := exec.Command("docker", "ps", "--format", "{{.Names}}\t{{.Ports}}").Output()
	if err != nil {
		return ""
	}
	for _, ln := range strings.Split(string(out), "\n") {
		name, ports, ok := strings.Cut(ln, "\t")
		if ok && (strings.Contains(ports, ":"+p.port+"->5432/tcp") || strings.Contains(ports, ":"+p.port+"->"+p.port+"/tcp")) {
			return name
		}
	}
	return ""
}

// streamDocker follows `docker logs` of the container into a local file so the
// same offset-based windows work as for a plain log file.
func (p *pg) streamDocker(container string) (string, error) {
	f, err := os.CreateTemp("", "routeperf-pglog-*.log")
	if err != nil {
		return "", err
	}
	now := time.Now()
	cmd := exec.Command("docker", "logs", "-f", "--since", fmt.Sprintf("%d.%09d", now.Unix(), now.Nanosecond()), container)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		f.Close()
		os.Remove(f.Name())
		return "", fmt.Errorf("docker logs %s: %w", container, err)
	}
	p.stream = cmd
	return f.Name(), nil
}

func (p *pg) findLog(ctx context.Context) (string, error) {
	if c, ok := strings.CutPrefix(p.opt.PGLogFile, "docker:"); ok {
		return p.streamDocker(c)
	}
	if p.opt.PGLogFile != "" {
		return p.opt.PGLogFile, nil
	}
	var collector, dataDir string
	_ = p.pool.QueryRow(ctx, "SELECT current_setting('logging_collector'), current_setting('data_directory')").Scan(&collector, &dataDir)
	if collector == "on" {
		var f *string
		_ = p.pool.QueryRow(ctx, "SELECT pg_current_logfile('stderr')").Scan(&f)
		if f != nil {
			path := *f
			if !filepath.IsAbs(path) {
				path = filepath.Join(dataDir, path)
			}
			return path, nil
		}
	}
	for _, c := range []string{
		fmt.Sprintf("/opt/homebrew/var/log/postgresql@%d.log", p.info.Major),
		fmt.Sprintf("/usr/local/var/log/postgresql@%d.log", p.info.Major),
		"/opt/homebrew/var/log/postgres.log", "/usr/local/var/log/postgres.log",
		fmt.Sprintf("/var/log/postgresql/postgresql-%d-main.log", p.info.Major),
	} {
		if st, err := os.Stat(c); err == nil && time.Since(st.ModTime()) < 72*time.Hour {
			return c, nil
		}
	}
	if c := p.dockerContainer(); c != "" {
		return p.streamDocker(c)
	}
	return "", errors.New("cannot locate the Postgres server log; pass --pg-log-file <path> (or docker:<container>), enable logging_collector, or use --capture proxy")
}

func (p *pg) StartCapture(ctx context.Context) error {
	path, err := p.findLog(ctx)
	if err != nil {
		return err
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("postgres log %s not readable: %w", path, err)
	}
	p.logPath = path
	p.prev = map[string]*string{}
	for name := range pgCaptureParams {
		var v *string
		_ = p.pool.QueryRow(ctx, `SELECT setting FROM pg_file_settings WHERE name = $1 AND sourcefile LIKE '%postgresql.auto.conf' ORDER BY seqno DESC LIMIT 1`, name).Scan(&v)
		p.prev[name] = v
	}
	for name, val := range pgCaptureParams {
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", name, val)); err != nil {
			return fmt.Errorf("enable statement logging: %w", err)
		}
	}
	if _, err := p.pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		return err
	}
	time.Sleep(400 * time.Millisecond)
	return nil
}

func (p *pg) StopCapture(ctx context.Context) error {
	if p.prev == nil {
		return nil
	}
	for name := range pgCaptureParams {
		var err error
		if v := p.prev[name]; v != nil {
			_, err = p.pool.Exec(ctx, fmt.Sprintf("ALTER SYSTEM SET %s = '%s'", name, strings.ReplaceAll(*v, "'", "''")))
		} else {
			_, err = p.pool.Exec(ctx, "ALTER SYSTEM RESET "+name)
		}
		if err != nil {
			return err
		}
	}
	_, err := p.pool.Exec(ctx, "SELECT pg_reload_conf()")
	p.prev = nil
	p.stopStream()
	return err
}

func (p *pg) stopStream() {
	if p.stream != nil && p.stream.Process != nil {
		_ = p.stream.Process.Kill()
		_ = p.stream.Wait()
		if f, ok := p.stream.Stdout.(*os.File); ok {
			f.Close()
			os.Remove(f.Name())
		}
		p.stream = nil
	}
}

func (p *pg) Mark(ctx context.Context) (Mark, error) {
	st, err := os.Stat(p.logPath)
	if err != nil {
		return Mark{}, err
	}
	return Mark{Offset: st.Size(), Time: time.Now()}, nil
}

var (
	pgLine   = regexp.MustCompile(`^\d{4}-\d\d-\d\d \d\d:\d\d:\d\d(?:\.\d+)? \S+ \[(\d+)\] rp:([^:]*): ([A-Z]+):  (.*)$`)
	pgDur    = regexp.MustCompile(`(?s)^duration: ([\d.]+) ms  (statement|execute [^:]*): (.*)$`)
	pgParams = regexp.MustCompile(`\$(\d+) = (NULL|'(?:[^']|'')*')`)
)

func (p *pg) Window(ctx context.Context, from, to Mark) ([]analyze.Stmt, error) {
	if to.Offset <= from.Offset {
		return nil, nil
	}
	f, err := os.Open(p.logPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if _, err := f.Seek(from.Offset, io.SeekStart); err != nil {
		return nil, err
	}
	sc := bufio.NewScanner(io.LimitReader(f, to.Offset-from.Offset))
	sc.Buffer(make([]byte, 1<<20), 64<<20)
	type entry struct {
		pid, app, level, msg string
	}
	var entries []*entry
	for sc.Scan() {
		ln := sc.Text()
		if m := pgLine.FindStringSubmatch(ln); m != nil {
			entries = append(entries, &entry{m[1], m[2], m[3], m[4]})
		} else if len(entries) > 0 {
			entries[len(entries)-1].msg += "\n" + ln
		}
	}
	var out []analyze.Stmt
	lastByPid := map[string]int{}
	for _, e := range entries {
		if e.app == "routeperf" {
			continue
		}
		switch e.level {
		case "LOG":
			m := pgDur.FindStringSubmatch(e.msg)
			if m == nil {
				continue
			}
			d, _ := strconv.ParseFloat(m[1], 64)
			st := MakeStmt("postgres", strings.TrimSpace(m[3]), p.cat)
			st.DurMs, st.Conn = d, e.pid
			out = append(out, st)
			lastByPid[e.pid] = len(out) - 1
		case "DETAIL":
			i, ok := lastByPid[e.pid]
			if !ok || !strings.HasPrefix(e.msg, "Parameters:") || out[i].Params != nil {
				continue
			}
			out[i].Params = ParsePGParams(e.msg)
		}
	}
	return out, nil
}

// ParsePGParams parses a "Parameters: $1 = '…', $2 = NULL" log detail.
func ParsePGParams(msg string) []string {
	ps := map[int]string{}
	maxI := 0
	for _, pm := range pgParams.FindAllStringSubmatch(msg, -1) {
		n, _ := strconv.Atoi(pm[1])
		v := pm[2]
		if v == "NULL" {
			v = nullMarker
		} else {
			v = strings.ReplaceAll(v[1:len(v)-1], "''", "'")
		}
		ps[n] = v
		if n > maxI {
			maxI = n
		}
	}
	params := make([]string, maxI)
	for n, v := range ps {
		params[n-1] = v
	}
	return params
}

// NullParam is the marker for a NULL bind parameter.
func NullParam() string { return nullMarker }

// ---------------------------------------------------------------- explain

func pgInline(st analyze.Stmt) string {
	nulls := map[int]bool{}
	for i, v := range st.Params {
		if IsNull(v) {
			nulls[i] = true
		}
	}
	return sqlutil.InlinePG(st.SQL, st.Params, nulls)
}

func (p *pg) Explain(ctx context.Context, st analyze.Stmt, tgt Target) (*plan.Plan, error) {
	sql := pgInline(st)
	tx, err := p.ex.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '120s'; SET LOCAL max_parallel_workers_per_gather = 0"); err != nil {
		return nil, err
	}
	if tgt.NS != "" {
		rw, err := rewriteTo("postgres", sql, p.cat, tgt)
		switch {
		case err == nil:
			sql = rw
		case tgt.Only != nil:
			return nil, fmt.Errorf("per-table replay needs a parsable statement: %w", err)
		default: // unparsable: unqualified names via search_path, qualified ones by regex
			schemas := map[string][]string{}
			for _, k := range st.Tables {
				if tb := p.cat[k]; tb != nil {
					schemas[tb.Schema] = append(schemas[tb.Schema], tb.Name)
				}
			}
			for s, ts := range schemas {
				sql = sqlutil.RewriteSchema(sql, s, tgt.NS, ts)
			}
			if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL search_path = %s, %s", qpg(tgt.NS), p.path)); err != nil {
				return nil, err
			}
		}
	}
	var out string
	if st.Generic && len(st.Params) > 0 {
		// Prepared statements in the app switch to a generic plan after 5
		// executions; replay the same way so plans and timings match.
		raw, err := rewriteTo("postgres", st.SQL, p.cat, tgt)
		if err != nil {
			raw = st.SQL
		}
		name := fmt.Sprintf("rp_g%d", time.Now().UnixNano())
		var args []string
		for _, v := range st.Params {
			if IsNull(v) {
				args = append(args, "NULL")
			} else {
				args = append(args, "'"+strings.ReplaceAll(v, "'", "''")+"'")
			}
		}
		if _, err := tx.Exec(ctx, "SET LOCAL plan_cache_mode = force_generic_plan"); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, "PREPARE "+name+" AS "+raw); err == nil {
			defer p.ex.Exec(context.Background(), "DEALLOCATE "+name)
			if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT JSON) EXECUTE "+name+"("+strings.Join(args, ", ")+")", pgx.QueryExecModeSimpleProtocol).Scan(&out); err != nil {
				return nil, err
			}
			pl, err := plan.ParsePostgresJSON([]byte(out))
			if err == nil {
				normalizeRelations(pl, p.cat, p.parts)
			}
			return pl, err
		}
		// parameter types not inferable → fall back to a custom plan in a fresh tx
		tx.Rollback(ctx)
		st.Generic = false
		return p.Explain(ctx, st, tgt)
	}
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, VERBOSE, FORMAT JSON) "+sql, pgx.QueryExecModeSimpleProtocol).Scan(&out); err != nil {
		return nil, err
	}
	pl, err := plan.ParsePostgresJSON([]byte(out))
	if err == nil {
		normalizeRelations(pl, p.cat, p.parts)
	}
	return pl, err
}

// DuplicateValue extracts the conflicting value from a unique violation.
func DuplicateValue(err error) (string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		if m := regexp.MustCompile(`\)=\((.*)\) already exists`).FindStringSubmatch(pe.Detail); m != nil {
			return m[1], true
		}
		return "", true
	}
	if m := regexp.MustCompile(`Duplicate entry '(.*)' for key`).FindStringSubmatch(fmt.Sprint(err)); m != nil {
		return m[1], true
	}
	return "", false
}

// FKViolation extracts (column, value, parent table) from a foreign-key
// violation (value is empty for MySQL, which doesn't report it).
func FKViolation(err error) (string, string, string, bool) {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23503" {
		if m := regexp.MustCompile(`Key \((\w+)\)=\((.*)\) is not present in table "(\w+)"`).FindStringSubmatch(pe.Detail); m != nil {
			return m[1], m[2], m[3], true
		}
	}
	if m := regexp.MustCompile("FOREIGN KEY \\(`(\\w+)`\\) REFERENCES `(\\w+)`").FindStringSubmatch(fmt.Sprint(err)); m != nil && strings.Contains(fmt.Sprint(err), "1452") {
		return m[1], "", m[2], true
	}
	return "", "", "", false
}

// ---------------------------------------------------------------- subsets

// createLike creates ns.<t> with t's structure; partitioned tables keep their
// partitions so the planner can prune them as it does on the real table.
func (p *pg) createLike(ctx context.Context, ns string, t *Table) error {
	dst := qpg(ns) + "." + qpg(t.NSName())
	stmts := []string{fmt.Sprintf("DROP TABLE IF EXISTS %s CASCADE", dst)}
	switch {
	case t.Kind == "partitioned" && t.PartKey != "":
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL) PARTITION BY %s", dst, t.SQL, t.PartKey))
		names := map[string]string{} // real partition → copy name
		for i, pt := range t.Parts {
			n := fmt.Sprintf("%s__rp_part%d", t.NSName(), i)
			names[pt.Name] = n
			p.parts[strings.ToLower(n)] = t.Key // plans on the copy map back to the table
			parent := dst
			if c, ok := names[pt.Parent]; ok {
				parent = qpg(ns) + "." + qpg(c)
			}
			s := fmt.Sprintf("CREATE TABLE %s.%s PARTITION OF %s %s", qpg(ns), qpg(n), parent, pt.Bound)
			if pt.PartKey != "" {
				s += " PARTITION BY " + pt.PartKey
			}
			stmts = append(stmts, s)
		}
	case t.Kind == "matview":
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s (LIKE %s)", dst, t.SQL))
	default:
		stmts = append(stmts, fmt.Sprintf("CREATE TABLE %s (LIKE %s INCLUDING ALL)", dst, t.SQL))
	}
	for _, s := range stmts {
		if _, err := p.pool.Exec(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s, err)
		}
	}
	if t.Kind == "matview" { // copy its indexes so plans match
		var defs []string
		rows, err := p.pool.Query(ctx, "SELECT pg_get_indexdef(indexrelid) FROM pg_index WHERE indrelid = $1::regclass", t.SQL)
		if err == nil {
			for rows.Next() {
				var d string
				if rows.Scan(&d) == nil {
					defs = append(defs, d)
				}
			}
			rows.Close()
		}
		for i, d := range defs {
			if _, on, ok := strings.Cut(d, " USING "); ok {
				_, _ = p.pool.Exec(ctx, fmt.Sprintf("CREATE INDEX %s ON %s USING %s", qpg(fmt.Sprintf("%s_i%d", t.NSName(), i)), dst, on))
			}
		}
	}
	return nil
}

func (p *pg) srcName(src string, t *Table) string {
	if src != "" {
		return qpg(src) + "." + qpg(t.NSName())
	}
	return t.SQL
}

func (p *pg) BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error) {
	if _, err := p.pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+qpg(ns)); err != nil {
		return nil, err
	}
	inScope := map[string]bool{}
	for _, t := range order {
		inScope[t.Key] = true
	}
	counts := map[string]float64{}
	var views []*Table
	for _, t := range order {
		if !t.HasData() {
			views = append(views, t)
			continue
		}
		if err := p.createLike(ctx, ns, t); err != nil {
			return nil, fmt.Errorf("subset %s.%s: %w", ns, t.Key, err)
		}
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = qpg(c)
		}
		cl := strings.Join(cols, ", ")
		scl := "s." + strings.Join(cols, ", s.")
		pred := subsetPred(p, t, ns, frac, fks, p.cat, inScope)
		dst := qpg(ns) + "." + qpg(t.NSName())
		srcT := p.srcName(src, t)
		if src != "" && !t.HasData() {
			srcT = t.SQL
		}
		for _, s := range []string{
			fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s s WHERE %s", dst, cl, scl, srcT, pred),
			fmt.Sprintf("ANALYZE %s", dst),
		} {
			if _, err := p.pool.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("subset %s.%s: %w", ns, t.Key, err)
			}
		}
		var n float64
		_ = p.pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*)::float8 FROM %s", dst)).Scan(&n)
		counts[t.Key] = n
	}
	// declared FKs make cascades/RI checks cost the same as in the real schema;
	// ones that close a cycle weren't sampled by, so existing rows aren't checked
	rules := map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}
	for _, f := range fks {
		if f.Declared && inScope[f.Child] && inScope[f.Parent] {
			c, pt := p.cat[f.Child], p.cat[f.Parent]
			var cc, pc []string
			for i := range f.ChildCols {
				cc = append(cc, qpg(f.ChildCols[i]))
				pc = append(pc, qpg(f.ParentCols[i]))
			}
			notValid := ""
			if f.Deferred {
				notValid = " NOT VALID"
			}
			_, err := p.pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s ADD FOREIGN KEY (%s) REFERENCES %s.%s (%s) ON DELETE %s%s",
				qpg(ns), qpg(c.NSName()), strings.Join(cc, ", "), qpg(ns), qpg(pt.NSName()), strings.Join(pc, ", "), rules[f.OnDelete], notValid))
			if err != nil && !f.Deferred { // partitioned tables take no NOT VALID FKs: go without
				return nil, fmt.Errorf("subset fk %s.%s: %w", f.Child, f.ChildCol, err)
			}
		}
	}
	for _, v := range sortViews("postgres", views, p.cat) {
		def, err := rewriteTo("postgres", v.ViewDef, p.cat, Target{NS: ns, Only: inScope})
		if err != nil {
			return nil, fmt.Errorf("subset view %s: %w", v.Key, err)
		}
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS %s", qpg(ns), qpg(v.NSName()), def)); err != nil {
			return nil, fmt.Errorf("subset view %s: %w", v.Key, err)
		}
	}
	return counts, nil
}

func (p *pg) BuildShrunk(ctx context.Context, ns string, frac float64, t *Table, keep []string, src string) (float64, error) {
	if _, err := p.pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+qpg(ns)); err != nil {
		return 0, err
	}
	if err := p.createLike(ctx, ns, t); err != nil {
		return 0, err
	}
	cols := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		cols[i] = qpg(c)
	}
	cl := strings.Join(cols, ", ")
	pred := p.HashPred(p.KeyExpr(t, "s"), frac)
	if len(keep) > 0 && t.PK != "" {
		var ks []string
		for _, k := range keep {
			ks = append(ks, "'"+strings.ReplaceAll(k, "'", "''")+"'")
		}
		pred = fmt.Sprintf("(%s OR s.%s::text IN (%s))", pred, qpg(t.PK), strings.Join(ks, ", "))
	}
	dst := qpg(ns) + "." + qpg(t.NSName())
	for _, s := range []string{
		fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT s.%s FROM %s s WHERE %s", dst, cl, strings.Join(cols, ", s."), p.srcName(src, t), pred),
		"ANALYZE " + dst,
	} {
		if _, err := p.pool.Exec(ctx, s); err != nil {
			return 0, fmt.Errorf("shrink %s: %w", t.Key, err)
		}
	}
	var n float64
	err := p.pool.QueryRow(ctx, "SELECT count(*)::float8 FROM "+dst).Scan(&n)
	return n, err
}

func (p *pg) DropNamespace(ctx context.Context, ns string) error {
	_, err := p.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+qpg(ns)+" CASCADE")
	return err
}

// ---------------------------------------------------------------- snapshot

func (p *pg) HasSnapshot(ctx context.Context) bool {
	var ok bool
	_ = p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_namespace WHERE nspname = $1)", SnapNS).Scan(&ok)
	return ok
}

func (p *pg) Snapshot(ctx context.Context, tables []*Table) error {
	if _, err := p.pool.Exec(ctx, "DROP SCHEMA IF EXISTS "+SnapNS+" CASCADE; CREATE SCHEMA "+SnapNS+
		"; CREATE TABLE "+SnapNS+"._rp_seq (seq text, last_value bigint, is_called bool)"); err != nil {
		return err
	}
	for _, t := range tables {
		if t.Kind != "table" && t.Kind != "partitioned" {
			continue
		}
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s.%s AS TABLE %s", SnapNS, qpg(t.NSName()), t.SQL)); err != nil {
			return fmt.Errorf("snapshot %s: %w", t.Key, err)
		}
		for _, c := range t.Cols {
			var seq *string
			_ = p.pool.QueryRow(ctx, "SELECT pg_get_serial_sequence($1, $2)", t.SQL, c).Scan(&seq)
			if seq == nil {
				continue
			}
			if _, err := p.pool.Exec(ctx, fmt.Sprintf("INSERT INTO %s._rp_seq SELECT '%s', last_value, is_called FROM %s", SnapNS, strings.ReplaceAll(*seq, "'", "''"), *seq)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *pg) Counters(ctx context.Context) (map[string]float64, error) {
	rows, err := p.pool.Query(ctx, `SELECT schemaname, relname, (n_tup_ins + n_tup_upd + n_tup_del)::float8 FROM pg_stat_user_tables WHERE schemaname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var s, n string
		var v float64
		if err := rows.Scan(&s, &n, &v); err != nil {
			return nil, err
		}
		k := strings.ToLower(n)
		if top, ok := p.parts[k]; ok {
			k = top
		} else if t := Resolve(p.cat, s, n); t != nil {
			k = t.Key
		}
		out[k] += v
	}
	return out, rows.Err()
}

func (p *pg) Restore(ctx context.Context, tables []*Table) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		return err
	}
	for _, t := range tables {
		if t.Kind != "table" && t.Kind != "partitioned" {
			continue
		}
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = qpg(c)
		}
		cl := strings.Join(cols, ", ")
		if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s", t.SQL)); err != nil {
			return fmt.Errorf("restore %s: %w", t.Key, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s.%s", t.SQL, cl, cl, SnapNS, qpg(t.NSName()))); err != nil {
			return fmt.Errorf("restore %s: %w", t.Key, err)
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT setval(seq, last_value, is_called) FROM %s._rp_seq", SnapNS)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, t := range tables {
		if t.HasData() {
			_, _ = p.pool.Exec(ctx, "ANALYZE "+t.SQL)
		}
	}
	return nil
}

func (p *pg) LogSource() string {
	if p.stream != nil {
		return "docker logs " + p.stream.Args[len(p.stream.Args)-1]
	}
	return p.logPath
}

func (p *pg) Probe(ctx context.Context) error {
	token := fmt.Sprintf("rp_probe_%d", time.Now().UnixNano())
	m0, err := p.Mark(ctx)
	if err != nil {
		return err
	}
	cc, err := pgx.ParseConfig(p.raw)
	if err != nil {
		return err
	}
	cc.RuntimeParams["application_name"] = "rp-probe"
	c, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return err
	}
	var s string
	err = c.QueryRow(ctx, "SELECT $1::text", token).Scan(&s)
	c.Close(ctx)
	if err != nil {
		return err
	}
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		m1, _ := p.Mark(ctx)
		stmts, err := p.Window(ctx, m0, m1)
		if err != nil {
			return err
		}
		for _, st := range stmts {
			for _, v := range st.Params {
				if v == token {
					return nil
				}
			}
			if strings.Contains(st.SQL, token) {
				return nil
			}
		}
	}
	return fmt.Errorf("probe query not found in %s — wrong log file? (set capture.pg_log_file)", p.logPath)
}

// ---------------------------------------------------------------- HypoPG

func (p *pg) HypoIndex(ctx context.Context, st analyze.Stmt, createIndex string) (Hypo, error) {
	var h Hypo
	var avail bool
	if err := p.pool.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_available_extensions WHERE name = 'hypopg')").Scan(&avail); err != nil || !avail {
		return h, ErrUnsupported
	}
	tx, err := p.ex.Begin(ctx)
	if err != nil {
		return h, err
	}
	defer tx.Rollback(ctx) // also drops the extension if routeperf created it
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS hypopg"); err != nil {
		return h, fmt.Errorf("hypopg: %w", err)
	}
	defer tx.Exec(context.Background(), "SELECT hypopg_reset()")
	sql := pgInline(st)
	estimate := func() (float64, float64, *plan.Plan, error) {
		var out string
		if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, pgx.QueryExecModeSimpleProtocol).Scan(&out); err != nil {
			return 0, 0, nil, err
		}
		var arr []struct {
			Plan map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(out), &arr); err != nil || len(arr) == 0 {
			return 0, 0, nil, fmt.Errorf("explain json: %v", err)
		}
		cost, _ := arr[0].Plan["Total Cost"].(float64)
		pl, err := plan.ParsePostgresJSON([]byte(out))
		if err != nil {
			return 0, 0, nil, err
		}
		var rows float64
		pl.Walk(func(n *plan.Node, _ int) {
			if n.Scan {
				rows += n.EstRows
			}
		})
		return cost, rows, pl, nil
	}
	var p0 *plan.Plan
	if h.CostBefore, h.RowsBefore, p0, err = estimate(); err != nil {
		return h, err
	}
	var hypoName string
	if err := tx.QueryRow(ctx, "SELECT indexname FROM hypopg_create_index($1)", strings.TrimSuffix(strings.TrimSpace(createIndex), ";")).Scan(&hypoName); err != nil {
		return h, fmt.Errorf("hypopg_create_index: %w", err)
	}
	var p1 *plan.Plan
	if h.CostAfter, h.RowsAfter, p1, err = estimate(); err != nil {
		return h, err
	}
	p1.Walk(func(n *plan.Node, _ int) {
		if n.Index == hypoName {
			h.Used = true
		}
	})
	if m := regexp.MustCompile(`(?i)\bON\s+(?:\S+\.)?"?(\w+)"?\s*\(`).FindStringSubmatch(createIndex); m != nil {
		scanOf := func(pl *plan.Plan) string {
			out := ""
			pl.Walk(func(n *plan.Node, _ int) {
				if out == "" && strings.EqualFold(n.Relation, m[1]) && (n.Scan || n.Op == plan.OpBitmapHeap) {
					out = n.RawType
				}
			})
			return out
		}
		h.ScanBefore, h.ScanAfter = scanOf(p0), scanOf(p1)
	}
	h.PlanAfter = p1.Text
	return h, nil
}

// ---------------------------------------------------------------- cold cache

// Evict drops the relations' pages from shared_buffers (Postgres 17+,
// pg_buffercache). The extension is created inside a rolled-back
// transaction, so nothing is left behind.
func (p *pg) Evict(ctx context.Context, tables []*Table) error {
	if p.info.Major < 17 {
		return fmt.Errorf("%w: evicting shared_buffers needs Postgres 17+ (pg_buffercache_evict)", ErrUnsupported)
	}
	var names []string
	for _, t := range tables {
		if t.HasData() {
			names = append(names, t.SQL)
		}
	}
	if len(names) == 0 {
		return nil
	}
	tx, err := p.ex.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_buffercache"); err != nil {
		return fmt.Errorf("pg_buffercache: %w", err)
	}
	_, err = tx.Exec(ctx, `
WITH RECURSIVE rels(oid) AS (
  SELECT x::regclass::oid FROM unnest($1::text[]) x
  UNION SELECT i.inhrelid FROM pg_inherits i JOIN rels r ON i.inhparent = r.oid),
all_rels AS (
  SELECT oid FROM rels
  UNION SELECT indexrelid FROM pg_index WHERE indrelid IN (SELECT oid FROM rels)
  UNION SELECT reltoastrelid FROM pg_class WHERE oid IN (SELECT oid FROM rels) AND reltoastrelid <> 0)
SELECT count(*) FROM (
  SELECT pg_buffercache_evict(b.bufferid) FROM pg_buffercache b
  WHERE b.reldatabase = (SELECT oid FROM pg_database WHERE datname = current_database())
    AND b.relfilenode IN (SELECT pg_relation_filenode(oid) FROM all_rels)) e`, names)
	return err
}

// ---------------------------------------------------------------- load

func (p *pg) LoadSample(ctx context.Context) (LoadSample, error) {
	s := LoadSample{Waits: map[string]int{}, Blocked: map[string]int{}}
	err := p.pool.QueryRow(ctx, `
SELECT count(*) FILTER (WHERE state = 'active'), count(*), current_setting('max_connections')::int,
  count(*) FILTER (WHERE wait_event_type = 'Lock')
FROM pg_stat_activity WHERE datname = current_database() AND backend_type = 'client backend'
  AND pid <> pg_backend_pid() AND application_name NOT IN ('routeperf', 'rp-probe')`).Scan(&s.Active, &s.Connections, &s.MaxConnections, &s.LockWaits)
	if err != nil {
		return s, err
	}
	rows, err := p.pool.Query(ctx, `
SELECT COALESCE(wait_event_type || ':' || wait_event, 'CPU'), count(*)::int FROM pg_stat_activity
WHERE datname = current_database() AND state = 'active' AND backend_type = 'client backend'
  AND pid <> pg_backend_pid() AND application_name NOT IN ('routeperf', 'rp-probe') GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				s.Waits[k] = n
			}
		}
		rows.Close()
	}
	rows, err = p.pool.Query(ctx, `SELECT c.relname, count(*)::int FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
WHERE NOT l.granted AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database()) GROUP BY 1`)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				if top, ok := p.parts[strings.ToLower(k)]; ok {
					k = top
				}
				s.Blocked[k] += n
			}
		}
		rows.Close()
	}
	return s, nil
}
