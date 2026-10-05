package db

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	major   int
	path    string // search_path
	raw     string
	stream  *exec.Cmd // docker logs -f, when the server runs in a container
	port    string
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
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		_, err := c.Exec(ctx, "SET log_min_duration_statement = -1")
		return err
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
	if _, err := ex.Exec(ctx, "SET log_min_duration_statement = -1"); err != nil {
		return nil, fmt.Errorf("routeperf needs a superuser (to silence its own statements in the log): %w", err)
	}
	p := &pg{pool: pool, ex: ex, opt: opt, raw: raw, port: u.Port()}
	if p.port == "" {
		p.port = "5432"
	}
	var ver string
	var super bool
	err = pool.QueryRow(ctx, `SELECT current_setting('server_version'), current_setting('server_version_num')::int/10000, r.rolsuper, current_database(), current_setting('search_path')
		FROM pg_roles r WHERE r.rolname = current_user`).Scan(&ver, &p.major, &super, &p.info.Database, &p.path)
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

func (p *pg) Tables(ctx context.Context) (map[string]*Table, error) {
	rows, err := p.pool.Query(ctx, `
SELECT c.relname, n.nspname, GREATEST(c.reltuples, 0)::float8,
  COALESCE((SELECT a.attname FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
            WHERE i.indrelid = c.oid AND i.indisprimary LIMIT 1), ''),
  COALESCE((SELECT array_agg(a.attname ORDER BY a.attnum) FROM pg_attribute a
            WHERE a.attrelid = c.oid AND a.attnum > 0 AND NOT a.attisdropped AND a.attgenerated = ''), '{}'),
  COALESCE((SELECT array_agg(DISTINCT a.attname) FROM pg_index i JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
            WHERE i.indrelid = c.oid), '{}')
FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p') AND n.nspname NOT IN ('pg_catalog','information_schema')
  AND n.nspname NOT LIKE 'pg\_toast%' AND n.nspname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*Table{}
	for rows.Next() {
		t := &Table{Indexed: map[string]bool{}}
		var idx []string
		if err := rows.Scan(&t.Name, &t.Schema, &t.Rows, &t.PK, &t.Cols, &idx); err != nil {
			return nil, err
		}
		for _, c := range idx {
			t.Indexed[strings.ToLower(c)] = true
		}
		if prev, ok := out[strings.ToLower(t.Name)]; ok && prev.Schema == "public" {
			continue
		}
		out[strings.ToLower(t.Name)] = t
	}
	for _, t := range out {
		if t.Rows == 0 {
			_ = p.pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*)::float8 FROM %s.%s", qpg(t.Schema), qpg(t.Name))).Scan(&t.Rows)
		}
	}
	return out, rows.Err()
}

func (p *pg) FKs(ctx context.Context, tables map[string]*Table) ([]FK, error) {
	rows, err := p.pool.Query(ctx, `
SELECT cl.relname, a.attname, pcl.relname, pa.attname, c.confdeltype::text,
  EXISTS (SELECT 1 FROM pg_index i WHERE i.indrelid = c.conrelid AND i.indkey[0] = c.conkey[1])
FROM pg_constraint c
JOIN pg_class cl ON cl.oid = c.conrelid
JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = c.conkey[1]
JOIN pg_class pcl ON pcl.oid = c.confrelid
JOIN pg_attribute pa ON pa.attrelid = c.confrelid AND pa.attnum = c.confkey[1]
JOIN pg_namespace n ON n.oid = cl.relnamespace
WHERE c.contype = 'f' AND n.nspname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FK
	for rows.Next() {
		f := FK{Declared: true}
		if err := rows.Scan(&f.Child, &f.ChildCol, &f.Parent, &f.ParentCol, &f.OnDelete, &f.Indexed); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return InferFKs(tables, out), rows.Err()
}

func (p *pg) HashPred(expr string, frac float64) string {
	return fmt.Sprintf("((hashtextextended((%s)::text, 0) %% 10000 + 10000) %% 10000) < %d", expr, int(frac*10000+0.5))
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
			out = append(out, fmt.Sprint(vals[0]))
		}
	}
	return out, rows.Err()
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
		fmt.Sprintf("/opt/homebrew/var/log/postgresql@%d.log", p.major),
		fmt.Sprintf("/usr/local/var/log/postgresql@%d.log", p.major),
		"/opt/homebrew/var/log/postgres.log", "/usr/local/var/log/postgres.log",
		fmt.Sprintf("/var/log/postgresql/postgresql-%d-main.log", p.major),
	} {
		if st, err := os.Stat(c); err == nil && time.Since(st.ModTime()) < 72*time.Hour {
			return c, nil
		}
	}
	if c := p.dockerContainer(); c != "" {
		return p.streamDocker(c)
	}
	return "", errors.New("cannot locate the Postgres server log; pass --pg-log-file <path> (or docker:<container>), or enable logging_collector")
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
			sql := strings.TrimSpace(m[3])
			st := analyze.Stmt{SQL: sql, DurMs: d, Kind: sqlutil.Kind(sql), Fingerprint: sqlutil.Fingerprint(sql), Tables: sqlutil.Tables(sql)}
			out = append(out, st)
			lastByPid[e.pid] = len(out) - 1
		case "DETAIL":
			i, ok := lastByPid[e.pid]
			if !ok || !strings.HasPrefix(e.msg, "Parameters:") || out[i].Params != nil {
				continue
			}
			ps := map[int]string{}
			maxI := 0
			for _, pm := range pgParams.FindAllStringSubmatch(e.msg, -1) {
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
			out[i].Params = params
		}
	}
	return out, nil
}

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

func (p *pg) Explain(ctx context.Context, st analyze.Stmt, ns string, tables map[string]*Table) (*plan.Plan, error) {
	sql := pgInline(st)
	tx, err := p.ex.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '120s'; SET LOCAL max_parallel_workers_per_gather = 0"); err != nil {
		return nil, err
	}
	if ns != "" {
		schemas := map[string][]string{}
		for _, t := range st.Tables {
			if tb := tables[t]; tb != nil {
				schemas[tb.Schema] = append(schemas[tb.Schema], tb.Name)
			}
		}
		for s, ts := range schemas {
			sql = sqlutil.RewriteSchema(sql, s, ns, ts)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("SET LOCAL search_path = %s, %s", qpg(ns), p.path)); err != nil {
			return nil, err
		}
	}
	var out string
	if err := tx.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+sql, pgx.QueryExecModeSimpleProtocol).Scan(&out); err != nil {
		return nil, err
	}
	return plan.ParsePostgresJSON([]byte(out))
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

func (p *pg) BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error) {
	if _, err := p.pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+qpg(ns)); err != nil {
		return nil, err
	}
	inScope := map[string]bool{}
	for _, t := range order {
		inScope[t.Name] = true
	}
	counts := map[string]float64{}
	for _, t := range order {
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = qpg(c)
		}
		cl := strings.Join(cols, ", ")
		scl := "s." + strings.Join(cols, ", s.")
		pred := subsetPred(p, t, ns, frac, fks, inScope, qpg)
		stmts := []string{
			fmt.Sprintf("DROP TABLE IF EXISTS %s.%s CASCADE", qpg(ns), qpg(t.Name)),
			fmt.Sprintf("CREATE TABLE %s.%s (LIKE %s.%s INCLUDING ALL)", qpg(ns), qpg(t.Name), qpg(t.Schema), qpg(t.Name)),
			fmt.Sprintf("INSERT INTO %s.%s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s.%s s WHERE %s", qpg(ns), qpg(t.Name), cl, scl, qpg(srcSchema(src, t)), qpg(t.Name), pred),
			fmt.Sprintf("ANALYZE %s.%s", qpg(ns), qpg(t.Name)),
		}
		for _, s := range stmts {
			if _, err := p.pool.Exec(ctx, s); err != nil {
				return nil, fmt.Errorf("subset %s.%s: %w", ns, t.Name, err)
			}
		}
		var n float64
		_ = p.pool.QueryRow(ctx, fmt.Sprintf("SELECT count(*)::float8 FROM %s.%s", qpg(ns), qpg(t.Name))).Scan(&n)
		counts[t.Name] = n
	}
	// declared FKs make cascades/RI checks cost the same as in the real schema
	rules := map[string]string{"a": "NO ACTION", "r": "RESTRICT", "c": "CASCADE", "n": "SET NULL", "d": "SET DEFAULT"}
	for _, f := range fks {
		if f.Declared && inScope[f.Child] && inScope[f.Parent] {
			_, err := p.pool.Exec(ctx, fmt.Sprintf("ALTER TABLE %s.%s ADD FOREIGN KEY (%s) REFERENCES %s.%s (%s) ON DELETE %s",
				qpg(ns), qpg(f.Child), qpg(f.ChildCol), qpg(ns), qpg(f.Parent), qpg(f.ParentCol), rules[f.OnDelete]))
			if err != nil {
				return nil, fmt.Errorf("subset fk %s.%s: %w", f.Child, f.ChildCol, err)
			}
		}
	}
	return counts, nil
}

func srcSchema(src string, t *Table) string {
	if src != "" {
		return src
	}
	return t.Schema
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
		if _, err := p.pool.Exec(ctx, fmt.Sprintf("CREATE TABLE %s.%s AS TABLE %s.%s", SnapNS, qpg(t.Name), qpg(t.Schema), qpg(t.Name))); err != nil {
			return fmt.Errorf("snapshot %s: %w", t.Name, err)
		}
		for _, c := range t.Cols {
			var seq *string
			_ = p.pool.QueryRow(ctx, "SELECT pg_get_serial_sequence($1, $2)", qpg(t.Schema)+"."+qpg(t.Name), c).Scan(&seq)
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
	rows, err := p.pool.Query(ctx, `SELECT relname, (n_tup_ins + n_tup_upd + n_tup_del)::float8 FROM pg_stat_user_tables WHERE schemaname NOT LIKE '\_rp\_%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]float64{}
	for rows.Next() {
		var n string
		var v float64
		if err := rows.Scan(&n, &v); err != nil {
			return nil, err
		}
		out[strings.ToLower(n)] = v
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
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = qpg(c)
		}
		cl := strings.Join(cols, ", ")
		if _, err := tx.Exec(ctx, fmt.Sprintf("DELETE FROM %s.%s", qpg(t.Schema), qpg(t.Name))); err != nil {
			return fmt.Errorf("restore %s: %w", t.Name, err)
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf("INSERT INTO %s.%s (%s) OVERRIDING SYSTEM VALUE SELECT %s FROM %s.%s", qpg(t.Schema), qpg(t.Name), cl, cl, SnapNS, qpg(t.Name))); err != nil {
			return fmt.Errorf("restore %s: %w", t.Name, err)
		}
	}
	if _, err := tx.Exec(ctx, fmt.Sprintf("SELECT setval(seq, last_value, is_called) FROM %s._rp_seq", SnapNS)); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	for _, t := range tables {
		_, _ = p.pool.Exec(ctx, fmt.Sprintf("ANALYZE %s.%s", qpg(t.Schema), qpg(t.Name)))
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
