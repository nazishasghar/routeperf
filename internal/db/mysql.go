package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/plan"
	"github.com/nazishasghar/routeperf/internal/sqlutil"
)

type my struct {
	db        *sql.DB
	ex        *sql.Conn
	info      Info
	opt       Options
	prevGL    string
	prevOut   string
	truncate  bool
	swapped   bool // pre-existing general_log rows were set aside for the run
	capturing bool
	probeDSN  string
	cat       map[string]*Table
	ps        bool              // performance_schema statement history is usable
	prevCons  map[string]string // consumer → ENABLED before capture
}

func openMySQL(ctx context.Context, u *url.URL, opt Options) (DB, error) {
	user := u.User.Username()
	pass, _ := u.User.Password()
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	dbname := strings.TrimPrefix(u.Path, "/")
	q := u.Query()
	q.Set("parseTime", "true")
	if !opt.NoSilence {
		q.Set("sql_log_off", "1")
	}
	q.Set("time_zone", "'+00:00'")
	q.Set("information_schema_stats_expiry", "0")
	q.Set("connectionAttributes", "program_name:routeperf") // lets load sampling exclude routeperf's own sessions
	auth := user
	if pass != "" {
		auth += ":" + pass
	}
	dsn := fmt.Sprintf("%s@tcp(%s)/%s?%s", auth, host, dbname, q.Encode())
	pq := u.Query()
	pq.Set("parseTime", "true")
	probeDSN := fmt.Sprintf("%s@tcp(%s)/%s?%s", auth, host, dbname, pq.Encode())
	d, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(4)
	if err := d.PingContext(ctx); err != nil {
		hint := "routeperf needs SYSTEM_VARIABLES_ADMIN for sql_log_off; use --capture proxy without it"
		if opt.NoSilence {
			hint = "check the URL and credentials"
		}
		return nil, fmt.Errorf("connect mysql (%s): %w", hint, err)
	}
	ex, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	m := &my{db: d, ex: ex, probeDSN: probeDSN, opt: opt, cat: map[string]*Table{}}
	var ver string
	if err := d.QueryRowContext(ctx, "SELECT VERSION(), DATABASE()").Scan(&ver, &m.info.Database); err != nil {
		return nil, err
	}
	var super int
	_ = d.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE IN ('SUPER') AND GRANTEE = CONCAT(\"'\", SUBSTRING_INDEX(CURRENT_USER(), '@', 1), \"'@'\", SUBSTRING_INDEX(CURRENT_USER(), '@', -1), \"'\")").Scan(&super)
	m.info.Dialect, m.info.Version, m.info.Super, m.info.Host = "mysql", ver, super > 0, u.Hostname()
	m.info.Major, _ = strconv.Atoi(strings.SplitN(ver, ".", 2)[0])
	if m.info.Database == "" {
		return nil, fmt.Errorf("mysql url must include a database name")
	}
	return m, nil
}

func (m *my) Info() Info { return m.info }

func (m *my) Close() {
	m.ex.Close()
	m.db.Close()
}

func qmy(s string) string { return "`" + strings.ReplaceAll(s, "`", "``") + "`" }

func (m *my) Quote(s string) string { return qmy(s) }

func (m *my) schemas() []string { return append([]string{m.info.Database}, m.opt.Schemas...) }

func (m *my) inList() (string, []any) {
	var ph []string
	var args []any
	for _, s := range m.schemas() {
		ph = append(ph, "?")
		args = append(args, s)
	}
	return "(" + strings.Join(ph, ",") + ")", args
}

func (m *my) key(schema, name string) string {
	if strings.EqualFold(schema, m.info.Database) {
		return strings.ToLower(name)
	}
	return strings.ToLower(schema + "." + name)
}

func (m *my) Tables(ctx context.Context) (map[string]*Table, error) {
	out := map[string]*Table{}
	in, args := m.inList()
	rows, err := m.db.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, TABLE_TYPE, COALESCE(TABLE_ROWS,0) FROM information_schema.TABLES
		WHERE TABLE_SCHEMA IN `+in+` AND TABLE_TYPE IN ('BASE TABLE','VIEW')`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &Table{Indexed: map[string]bool{}, Indexes: map[string][]string{}, NotNull: map[string]bool{}}
		var typ string
		if err := rows.Scan(&t.Schema, &t.Name, &typ, &t.Rows); err != nil {
			rows.Close()
			return nil, err
		}
		t.Kind = "table"
		if typ == "VIEW" {
			t.Kind, t.Rows = "view", 0
		}
		t.Key, t.SQL = m.key(t.Schema, t.Name), qmy(t.Schema)+"."+qmy(t.Name)
		out[t.Key] = t
	}
	rows.Close()
	cols, err := m.db.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, EXTRA, IS_NULLABLE FROM information_schema.COLUMNS
		WHERE TABLE_SCHEMA IN `+in+` ORDER BY TABLE_SCHEMA, TABLE_NAME, ORDINAL_POSITION`, args...)
	if err != nil {
		return nil, err
	}
	for cols.Next() {
		var sn, tn, cn, extra, nullable string
		if err := cols.Scan(&sn, &tn, &cn, &extra, &nullable); err != nil {
			cols.Close()
			return nil, err
		}
		t := out[m.key(sn, tn)]
		if t == nil {
			continue
		}
		if nullable == "NO" {
			t.NotNull[strings.ToLower(cn)] = true
		}
		if !strings.Contains(strings.ToUpper(extra), "GENERATED") {
			t.Cols = append(t.Cols, cn)
		}
	}
	cols.Close()
	idx, err := m.db.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, COLUMN_NAME, INDEX_NAME, SEQ_IN_INDEX FROM information_schema.STATISTICS
		WHERE TABLE_SCHEMA IN `+in+` AND COLUMN_NAME IS NOT NULL ORDER BY TABLE_SCHEMA, TABLE_NAME, INDEX_NAME, SEQ_IN_INDEX`, args...)
	if err != nil {
		return nil, err
	}
	for idx.Next() {
		var sn, tn, cn, iname string
		var seq int
		if err := idx.Scan(&sn, &tn, &cn, &iname, &seq); err != nil {
			idx.Close()
			return nil, err
		}
		if t := out[m.key(sn, tn)]; t != nil {
			t.Indexes[iname] = append(t.Indexes[iname], strings.ToLower(cn))
			if seq == 1 {
				t.Indexed[strings.ToLower(cn)] = true
			}
			if iname == "PRIMARY" {
				t.PKCols = append(t.PKCols, cn)
				t.PK = t.PKCols[0]
			}
		}
	}
	idx.Close()
	vr, err := m.db.QueryContext(ctx, `SELECT TABLE_SCHEMA, TABLE_NAME, VIEW_DEFINITION FROM information_schema.VIEWS WHERE TABLE_SCHEMA IN `+in, args...)
	if err == nil {
		for vr.Next() {
			var sn, tn, def string
			if vr.Scan(&sn, &tn, &def) == nil {
				if t := out[m.key(sn, tn)]; t != nil {
					t.ViewDef = def
				}
			}
		}
		vr.Close()
	}
	for _, t := range out {
		if t.Rows < 1000 && t.HasData() {
			_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+t.SQL).Scan(&t.Rows)
		}
	}
	m.cat = out
	return out, nil
}

func (m *my) FKs(ctx context.Context, tables map[string]*Table) ([]FK, error) {
	in, args := m.inList()
	rows, err := m.db.QueryContext(ctx, `
SELECT kcu.CONSTRAINT_SCHEMA, kcu.CONSTRAINT_NAME, kcu.TABLE_SCHEMA, kcu.TABLE_NAME, kcu.COLUMN_NAME,
  kcu.REFERENCED_TABLE_SCHEMA, kcu.REFERENCED_TABLE_NAME, kcu.REFERENCED_COLUMN_NAME, rc.DELETE_RULE
FROM information_schema.KEY_COLUMN_USAGE kcu
JOIN information_schema.REFERENTIAL_CONSTRAINTS rc ON rc.CONSTRAINT_SCHEMA = kcu.CONSTRAINT_SCHEMA AND rc.CONSTRAINT_NAME = kcu.CONSTRAINT_NAME
WHERE kcu.TABLE_SCHEMA IN `+in+` AND kcu.REFERENCED_TABLE_NAME IS NOT NULL
ORDER BY kcu.CONSTRAINT_SCHEMA, kcu.CONSTRAINT_NAME, kcu.ORDINAL_POSITION`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*FK{}
	var order []string
	for rows.Next() {
		var cs, cn, ts, tn, col, rs, rt, rcol, rule string
		if err := rows.Scan(&cs, &cn, &ts, &tn, &col, &rs, &rt, &rcol, &rule); err != nil {
			return nil, err
		}
		child, parent := Resolve(tables, ts, tn), Resolve(tables, rs, rt)
		if child == nil || parent == nil {
			continue
		}
		k := cs + "." + cn
		f := byName[k]
		if f == nil {
			f = &FK{Declared: true, Child: child.Key, Parent: parent.Key, ChildCol: col, ParentCol: rcol, OnDelete: rule}
			byName[k] = f
			order = append(order, k)
		}
		f.ChildCols = append(f.ChildCols, col)
		f.ParentCols = append(f.ParentCols, rcol)
	}
	var out []FK
	for _, k := range order {
		f := byName[k]
		f.Indexed = fkIndexed(tables[f.Child], f.ChildCols)
		out = append(out, *f)
	}
	return MarkCycles(tables, InferFKs(tables, out)), rows.Err()
}

func (m *my) HashPred(expr string, frac float64) string {
	return fmt.Sprintf("(CRC32(%s) %% 10000) < %d", expr, int(frac*10000+0.5))
}

func (m *my) KeyExpr(t *Table, alias string) string {
	cols := t.PKCols
	if len(cols) == 1 {
		return alias + "." + qmy(cols[0])
	}
	if len(cols) == 0 {
		cols = t.Cols
	}
	var cs []string
	for _, c := range cols {
		cs = append(cs, alias+"."+qmy(c))
	}
	return "CONCAT_WS(','," + strings.Join(cs, ",") + ")"
}

func (m *my) FirstValues(ctx context.Context, q string) ([]string, error) {
	rows, err := m.db.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var v sql.RawBytes
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out = append(out, string(v))
	}
	return out, rows.Err()
}

// ---------------------------------------------------------------- capture

var psConsumers = []string{"events_statements_current", "events_statements_history_long"}

const savedLog = "mysql.rp_general_log_saved"

func (m *my) hasSavedLog(ctx context.Context) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'rp_general_log_saved'").Scan(&n)
	return n > 0
}

// restoreSavedLog puts back general_log rows set aside by a run (also after
// a crash). general_log must be off.
func (m *my) restoreSavedLog(ctx context.Context) error {
	if !m.hasSavedLog(ctx) {
		return nil
	}
	for _, s := range []string{"DROP TABLE IF EXISTS mysql.rp_general_log_run", "RENAME TABLE mysql.general_log TO mysql.rp_general_log_run, " + savedLog + " TO mysql.general_log", "DROP TABLE mysql.rp_general_log_run"} {
		if _, err := m.db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("restore general_log: %w", err)
		}
	}
	return nil
}

func on(v string) bool { return v == "1" || strings.EqualFold(v, "ON") }

func (m *my) StartCapture(ctx context.Context) error {
	if err := m.db.QueryRowContext(ctx, "SELECT @@GLOBAL.general_log, @@GLOBAL.log_output").Scan(&m.prevGL, &m.prevOut); err != nil {
		return err
	}
	if !on(m.prevGL) { // a crashed run may have left rows set aside
		if err := m.restoreSavedLog(ctx); err != nil {
			return err
		}
	}
	var n int
	_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.general_log").Scan(&n)
	m.truncate, m.swapped = n == 0, false
	if n > 0 && !on(m.prevGL) {
		// old rows make every window scan (the log table has no index): set
		// them aside for the run and put them back afterwards
		for _, s := range []string{"DROP TABLE IF EXISTS mysql.rp_general_log_new", "CREATE TABLE mysql.rp_general_log_new LIKE mysql.general_log",
			"RENAME TABLE mysql.general_log TO " + savedLog + ", mysql.rp_general_log_new TO mysql.general_log"} {
			if _, err := m.db.ExecContext(ctx, s); err != nil {
				break // keep the old behaviour: windows over the full table
			}
		}
		m.swapped = m.hasSavedLog(ctx)
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL log_output = 'TABLE'"); err != nil {
		return fmt.Errorf("enable general_log (needs SYSTEM_VARIABLES_ADMIN): %w", err)
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL general_log = 'ON'"); err != nil {
		return err
	}
	m.capturing = true
	m.startPS(ctx)
	return nil
}

// startPS enables the statement-history consumer so each captured statement
// gets its real server time and rows examined.
func (m *my) startPS(ctx context.Context) {
	m.ps = false
	var on int
	if m.db.QueryRowContext(ctx, "SELECT @@performance_schema").Scan(&on) != nil || on != 1 {
		return
	}
	m.prevCons = map[string]string{}
	for _, c := range psConsumers {
		var v string
		if m.db.QueryRowContext(ctx, "SELECT ENABLED FROM performance_schema.setup_consumers WHERE NAME = ?", c).Scan(&v) != nil {
			return
		}
		m.prevCons[c] = v
	}
	for _, c := range psConsumers {
		if _, err := m.db.ExecContext(ctx, "UPDATE performance_schema.setup_consumers SET ENABLED = 'YES' WHERE NAME = ?", c); err != nil {
			m.stopPS(ctx)
			return
		}
	}
	var t sql.NullInt64
	if m.db.QueryRowContext(ctx, "SELECT TIMER_START FROM performance_schema.events_statements_current WHERE THREAD_ID = PS_CURRENT_THREAD_ID()").Scan(&t) == nil && t.Valid {
		m.ps = true
	}
}

func (m *my) stopPS(ctx context.Context) {
	for c, v := range m.prevCons {
		_, _ = m.db.ExecContext(ctx, "UPDATE performance_schema.setup_consumers SET ENABLED = ? WHERE NAME = ?", v, c)
	}
	m.prevCons, m.ps = nil, false
}

// TimingSource says where per-statement DB time comes from.
func (m *my) TimingSource() string {
	if m.ps {
		return "performance_schema"
	}
	return "replay estimate"
}

func (m *my) StopCapture(ctx context.Context) error {
	if !m.capturing {
		return nil
	}
	m.stopPS(ctx)
	gl := "OFF"
	if on(m.prevGL) {
		gl = "ON"
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL general_log = '"+gl+"'"); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL log_output = '"+strings.ReplaceAll(m.prevOut, "'", "")+"'"); err != nil {
		return err
	}
	switch {
	case m.swapped:
		if err := m.restoreSavedLog(ctx); err != nil {
			return err
		}
	case m.truncate:
		_, _ = m.db.ExecContext(ctx, "TRUNCATE TABLE mysql.general_log")
	}
	m.swapped = false
	m.capturing = false
	return nil
}

// Mark is the general_log row count: the CSV table is append-only and scans in
// insertion order, and event_time is unusable (stored in each session's zone).
// With performance_schema it also records the server timer.
func (m *my) Mark(ctx context.Context) (Mark, error) {
	var n int64
	err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.general_log").Scan(&n)
	mk := Mark{Offset: n, Time: time.Now()}
	if m.ps {
		var t sql.NullInt64
		if m.db.QueryRowContext(ctx, "SELECT TIMER_START FROM performance_schema.events_statements_current WHERE THREAD_ID = PS_CURRENT_THREAD_ID()").Scan(&t) == nil {
			mk.Timer = t.Int64
		}
	}
	return mk, err
}

func (m *my) Window(ctx context.Context, from, to Mark) ([]analyze.Stmt, error) {
	if to.Offset <= from.Offset {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT thread_id, command_type, CONVERT(argument USING utf8mb4) FROM mysql.general_log LIMIT ? OFFSET ?`,
		to.Offset-from.Offset, from.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analyze.Stmt
	for rows.Next() {
		var tid int64
		var ct, s string
		if err := rows.Scan(&tid, &ct, &s); err != nil {
			return nil, err
		}
		if ct != "Query" && ct != "Execute" {
			continue
		}
		st := MakeStmt("mysql", strings.TrimSpace(s), m.cat)
		st.Conn = strconv.FormatInt(tid, 10)
		out = append(out, st)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if m.ps && from.Timer > 0 && to.Timer > from.Timer && len(out) > 0 {
		m.attachTimings(ctx, out, from.Timer, to.Timer)
	}
	return out, nil
}

// attachTimings matches general_log statements to performance_schema events of
// the same connection (by normalized text, then by order) and copies the real
// execution time and rows examined.
func (m *my) attachTimings(ctx context.Context, stmts []analyze.Stmt, t0, t1 int64) {
	conns := map[string]bool{}
	var ids []any
	for _, st := range stmts {
		if !conns[st.Conn] {
			conns[st.Conn] = true
			ids = append(ids, st.Conn)
		}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := append([]any{t0, t1}, ids...)
	rows, err := m.db.QueryContext(ctx, `
SELECT t.PROCESSLIST_ID, COALESCE(e.SQL_TEXT, ''), e.TIMER_WAIT, e.ROWS_EXAMINED
FROM performance_schema.events_statements_history_long e
JOIN performance_schema.threads t ON t.THREAD_ID = e.THREAD_ID
WHERE e.TIMER_START >= ? AND e.TIMER_START <= ? AND t.PROCESSLIST_ID IN (`+ph+`)
  AND (e.EVENT_NAME LIKE 'statement/sql/%' OR e.EVENT_NAME = 'statement/com/Execute') AND e.NESTING_EVENT_ID IS NULL
ORDER BY e.THREAD_ID, e.EVENT_ID`, args...)
	if err != nil {
		return
	}
	defer rows.Close()
	type ev struct {
		norm     string
		ms, rows float64
		used     bool
	}
	byConn := map[string][]*ev{}
	for rows.Next() {
		var pid int64
		var text string
		var wait, exam float64
		if rows.Scan(&pid, &text, &wait, &exam) != nil {
			continue
		}
		c := strconv.FormatInt(pid, 10)
		byConn[c] = append(byConn[c], &ev{norm: sqlutil.Normalize(text), ms: wait / 1e9, rows: exam})
	}
	for i := range stmts {
		evs := byConn[stmts[i].Conn]
		norm := sqlutil.Normalize(stmts[i].SQL)
		var hit *ev
		for _, e := range evs {
			if !e.used && e.norm == norm {
				hit = e
				break
			}
		}
		if hit == nil {
			for _, e := range evs {
				if !e.used {
					hit = e
					break
				}
			}
		}
		if hit != nil {
			hit.used = true
			stmts[i].DurMs, stmts[i].RowsExam = hit.ms, hit.rows
		}
	}
}

// ---------------------------------------------------------------- explain

func (m *my) handlerReads(ctx context.Context, tx *sql.Tx) float64 {
	rows, err := tx.QueryContext(ctx, "SHOW SESSION STATUS LIKE 'Handler_read%'")
	if err != nil {
		return 0
	}
	defer rows.Close()
	var t float64
	for rows.Next() {
		var k, v string
		if rows.Scan(&k, &v) == nil {
			f, _ := strconv.ParseFloat(v, 64)
			t += f
		}
	}
	return t
}

func aliasesFor(q string) map[string]string {
	if a, ok := sqlutil.MySQLAliases(q); ok {
		return a
	}
	return plan.MySQLAliases(q)
}

func (m *my) Explain(ctx context.Context, st analyze.Stmt, tgt Target) (*plan.Plan, error) {
	q := st.SQL
	if tgt.NS != "" {
		rw, err := rewriteTo("mysql", q, m.cat, tgt)
		switch {
		case err == nil:
			q = rw
		case tgt.Only != nil:
			return nil, fmt.Errorf("per-table replay needs a parsable statement: %w", err)
		default:
			q = sqlutil.RewriteSchema(q, m.info.Database, tgt.NS, st.Tables)
			if _, err := m.ex.ExecContext(ctx, "USE "+qmy(tgt.NS)); err != nil {
				return nil, err
			}
			defer m.ex.ExecContext(context.Background(), "USE "+qmy(m.info.Database))
		}
	}
	tx, err := m.ex.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	finish := func(p *plan.Plan) *plan.Plan {
		normalizeRelations(p, m.cat, nil)
		return p
	}
	if st.Kind == "select" {
		var text string
		if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+q).Scan(&text); err != nil {
			return nil, err
		}
		p, err := plan.ParseMySQLTree(text, aliasesFor(q))
		if err != nil {
			return nil, err
		}
		if p.RowsExamined() < 1 { // work done while optimizing (const tables, materialized views): count handler reads
			h0 := m.handlerReads(ctx, tx)
			h1 := m.handlerReads(ctx, tx)
			t0 := time.Now()
			if rows, err := tx.QueryContext(ctx, q); err == nil {
				for rows.Next() {
				}
				rows.Close()
				elapsed := float64(time.Since(t0).Microseconds()) / 1000
				if d := m.handlerReads(ctx, tx) - h1 - (h1 - h0); d > 0 {
					p.HandlerRows = d
					p.ExecutionMs = math.Max(p.ExecutionMs, elapsed)
				}
			}
		}
		return finish(p), nil
	}
	// DML: EXPLAIN ANALYZE covers only multi-table UPDATE/DELETE.
	var text string
	if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+q).Scan(&text); err == nil {
		p, err := plan.ParseMySQLTree(text, aliasesFor(q))
		if err != nil {
			return nil, err
		}
		return finish(p), nil
	}
	base0 := m.handlerReads(ctx, tx)
	base1 := m.handlerReads(ctx, tx)
	overhead := base1 - base0
	t0 := time.Now()
	if _, err := tx.ExecContext(ctx, q); err != nil {
		return nil, err
	}
	elapsed := float64(time.Since(t0).Microseconds()) / 1000
	after := m.handlerReads(ctx, tx)
	target := sqlutil.Parse("mysql", q).Target.Name
	root := &plan.Node{Op: plan.OpModify, RawType: strings.ToUpper(st.Kind), Relation: target, Rows: 1, Loops: 1, TimeMs: elapsed}
	p := &plan.Plan{Dialect: "mysql", Root: root, ExecutionMs: elapsed, HandlerRows: after - base1 - overhead}
	if probe := sqlutil.MySQLWhereProbe(q); probe != "" {
		if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+probe).Scan(&text); err == nil {
			if pp, err := plan.ParseMySQLTree(text, aliasesFor(probe)); err == nil {
				root.Children = []*plan.Node{pp.Root}
			}
		}
	}
	if p.HandlerRows < 1 {
		p.HandlerRows = 1
	}
	return finish(plan.Finish(p)), nil
}

// ---------------------------------------------------------------- subsets

func (m *my) srcName(src string, t *Table) string {
	if src != "" {
		return qmy(src) + "." + qmy(t.NSName())
	}
	return t.SQL
}

func (m *my) BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error) {
	if _, err := m.db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+qmy(ns)); err != nil {
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
		cols := make([]string, len(t.Cols))
		for i, c := range t.Cols {
			cols[i] = qmy(c)
		}
		cl := strings.Join(cols, ", ")
		scl := "s." + strings.Join(cols, ", s.")
		pred := subsetPred(m, t, ns, frac, fks, m.cat, inScope)
		dst := qmy(ns) + "." + qmy(t.NSName())
		for _, s := range []string{
			"DROP TABLE IF EXISTS " + dst,
			fmt.Sprintf("CREATE TABLE %s LIKE %s", dst, t.SQL),
			fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s s WHERE %s", dst, cl, scl, m.srcName(src, t), pred),
			"ANALYZE TABLE " + dst,
		} {
			if _, err := m.db.ExecContext(ctx, s); err != nil {
				return nil, fmt.Errorf("subset %s: %w", dst, err)
			}
		}
		var n float64
		_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dst).Scan(&n)
		counts[t.Key] = n
	}
	// FKs that close a cycle weren't sampled by: existing rows aren't checked
	c, err := m.db.Conn(ctx)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	defer c.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS = 1")
	for _, f := range fks {
		if f.Declared && inScope[f.Child] && inScope[f.Parent] {
			rule := f.OnDelete
			if rule == "" {
				rule = "NO ACTION"
			}
			var cc, pc []string
			for i := range f.ChildCols {
				cc = append(cc, qmy(f.ChildCols[i]))
				pc = append(pc, qmy(f.ParentCols[i]))
			}
			checks := "SET FOREIGN_KEY_CHECKS = 1"
			if f.Deferred {
				checks = "SET FOREIGN_KEY_CHECKS = 0"
			}
			if _, err := c.ExecContext(ctx, checks); err != nil {
				return nil, err
			}
			if _, err := c.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s.%s ADD FOREIGN KEY (%s) REFERENCES %s.%s (%s) ON DELETE %s",
				qmy(ns), qmy(m.cat[f.Child].NSName()), strings.Join(cc, ", "), qmy(ns), qmy(m.cat[f.Parent].NSName()), strings.Join(pc, ", "), rule)); err != nil && !f.Deferred {
				return nil, fmt.Errorf("subset fk %s.%s: %w", f.Child, f.ChildCol, err)
			}
		}
	}
	for _, v := range sortViews("mysql", views, m.cat) {
		def, err := rewriteTo("mysql", v.ViewDef, m.cat, Target{NS: ns, Only: inScope})
		if err != nil {
			return nil, fmt.Errorf("subset view %s: %w", v.Key, err)
		}
		if _, err := m.db.ExecContext(ctx, fmt.Sprintf("CREATE OR REPLACE VIEW %s.%s AS %s", qmy(ns), qmy(v.NSName()), def)); err != nil {
			return nil, fmt.Errorf("subset view %s: %w", v.Key, err)
		}
	}
	return counts, nil
}

func (m *my) BuildShrunk(ctx context.Context, ns string, frac float64, t *Table, keep []string, src string) (float64, error) {
	if _, err := m.db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+qmy(ns)); err != nil {
		return 0, err
	}
	cols := make([]string, len(t.Cols))
	for i, c := range t.Cols {
		cols[i] = qmy(c)
	}
	pred := m.HashPred(m.KeyExpr(t, "s"), frac)
	if len(keep) > 0 && t.PK != "" {
		var ks []string
		for _, k := range keep {
			ks = append(ks, "'"+strings.ReplaceAll(strings.ReplaceAll(k, `\`, `\\`), "'", "''")+"'")
		}
		pred = fmt.Sprintf("(%s OR s.%s IN (%s))", pred, qmy(t.PK), strings.Join(ks, ", "))
	}
	dst := qmy(ns) + "." + qmy(t.NSName())
	for _, s := range []string{
		"DROP TABLE IF EXISTS " + dst,
		fmt.Sprintf("CREATE TABLE %s LIKE %s", dst, t.SQL),
		fmt.Sprintf("INSERT INTO %s (%s) SELECT s.%s FROM %s s WHERE %s", dst, strings.Join(cols, ", "), strings.Join(cols, ", s."), m.srcName(src, t), pred),
		"ANALYZE TABLE " + dst,
	} {
		if _, err := m.db.ExecContext(ctx, s); err != nil {
			return 0, fmt.Errorf("shrink %s: %w", t.Key, err)
		}
	}
	var n float64
	err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dst).Scan(&n)
	return n, err
}

func (m *my) DropNamespace(ctx context.Context, ns string) error {
	_, err := m.db.ExecContext(ctx, "DROP DATABASE IF EXISTS "+qmy(ns))
	return err
}

// ---------------------------------------------------------------- snapshot

func (m *my) HasSnapshot(ctx context.Context) bool {
	var n int
	_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", SnapNS).Scan(&n)
	return n > 0
}

func (m *my) Snapshot(ctx context.Context, tables []*Table) error {
	for _, s := range []string{"DROP DATABASE IF EXISTS " + SnapNS, "CREATE DATABASE " + SnapNS,
		"CREATE TABLE " + SnapNS + "._rp_ai (t varchar(255) PRIMARY KEY, v bigint)"} {
		if _, err := m.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	for _, t := range tables {
		if !t.HasData() {
			continue
		}
		if _, err := m.db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s.%s AS SELECT * FROM %s", SnapNS, qmy(t.NSName()), t.SQL)); err != nil {
			return fmt.Errorf("snapshot %s: %w", t.Key, err)
		}
		var ai sql.NullInt64
		_ = m.db.QueryRowContext(ctx, "SELECT AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_NAME = ?", t.Schema, t.Name).Scan(&ai)
		if ai.Valid {
			if _, err := m.db.ExecContext(ctx, "INSERT INTO "+SnapNS+"._rp_ai VALUES (?, ?)", t.NSName(), ai.Int64); err != nil {
				return err
			}
		}
	}
	return nil
}

func (m *my) Counters(ctx context.Context) (map[string]float64, error) {
	in, args := m.inList()
	rows, err := m.db.QueryContext(ctx, `SELECT OBJECT_SCHEMA, OBJECT_NAME, COUNT_INSERT + COUNT_UPDATE + COUNT_DELETE FROM performance_schema.table_io_waits_summary_by_table WHERE OBJECT_SCHEMA IN `+in, args...)
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
		out[m.key(s, n)] = v
	}
	return out, rows.Err()
}

func (m *my) Restore(ctx context.Context, tables []*Table) error {
	c, err := m.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := c.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		return err
	}
	defer c.ExecContext(context.Background(), "SET FOREIGN_KEY_CHECKS = 1")
	for _, t := range tables {
		if !t.HasData() {
			continue
		}
		cols := make([]string, len(t.Cols))
		for i, cn := range t.Cols {
			cols[i] = qmy(cn)
		}
		cl := strings.Join(cols, ", ")
		for _, s := range []string{"TRUNCATE TABLE " + t.SQL,
			fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s.%s", t.SQL, cl, cl, SnapNS, qmy(t.NSName()))} {
			if _, err := c.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("restore %s: %w", t.Key, err)
			}
		}
		var ai sql.NullInt64
		_ = c.QueryRowContext(ctx, "SELECT v FROM "+SnapNS+"._rp_ai WHERE t = ?", t.NSName()).Scan(&ai)
		if ai.Valid {
			_, _ = c.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", t.SQL, ai.Int64))
		}
		_, _ = c.ExecContext(ctx, "ANALYZE TABLE "+t.SQL)
	}
	return nil
}

func (m *my) LogSource() string {
	if m.ps {
		return "mysql.general_log (TABLE) + performance_schema timings"
	}
	return "mysql.general_log (TABLE)"
}

func (m *my) Probe(ctx context.Context) error {
	token := fmt.Sprintf("rp_probe_%d", time.Now().UnixNano())
	m0, err := m.Mark(ctx)
	if err != nil {
		return err
	}
	pd, err := sql.Open("mysql", m.probeDSN)
	if err != nil {
		return err
	}
	var s string
	err = pd.QueryRowContext(ctx, "SELECT ?", token).Scan(&s)
	pd.Close()
	if err != nil {
		return err
	}
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		m1, _ := m.Mark(ctx)
		stmts, err := m.Window(ctx, m0, m1)
		if err != nil {
			return err
		}
		for _, st := range stmts {
			if strings.Contains(st.SQL, token) {
				return nil
			}
		}
	}
	return fmt.Errorf("probe query not found in mysql.general_log")
}

// ---------------------------------------------------------------- unsupported

func (m *my) HypoIndex(context.Context, analyze.Stmt, string) (Hypo, error) {
	return Hypo{}, ErrUnsupported
}

func (m *my) Evict(context.Context, []*Table) error {
	return fmt.Errorf("%w: InnoDB can't evict single tables from the buffer pool; set cache.cold_cmd (e.g. a server restart) for cold numbers", ErrUnsupported)
}

// ---------------------------------------------------------------- load

func (m *my) LoadSample(ctx context.Context) (LoadSample, error) {
	s := LoadSample{Waits: map[string]int{}, Blocked: map[string]int{}}
	rows, err := m.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(STATE, ''), COMMAND), COUNT(*) FROM information_schema.PROCESSLIST
		WHERE DB = DATABASE() AND COMMAND NOT IN ('Sleep', 'Daemon') AND ID <> CONNECTION_ID()
		  AND ID NOT IN (SELECT PROCESSLIST_ID FROM performance_schema.session_connect_attrs WHERE ATTR_NAME = 'program_name' AND ATTR_VALUE = 'routeperf') GROUP BY 1`)
	if err != nil {
		return s, err
	}
	for rows.Next() {
		var k string
		var n int
		if rows.Scan(&k, &n) == nil {
			s.Waits[k] = n
			s.Active += n
		}
	}
	rows.Close()
	// the app's sessions on this database, without routeperf's own
	err = m.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM performance_schema.threads t WHERE t.TYPE = 'FOREGROUND' AND t.PROCESSLIST_DB = DATABASE()
		AND t.PROCESSLIST_ID NOT IN (SELECT PROCESSLIST_ID FROM performance_schema.session_connect_attrs WHERE ATTR_NAME = 'program_name' AND ATTR_VALUE = 'routeperf')`).Scan(&s.Connections)
	if err != nil {
		var v string
		_ = m.db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_status WHERE VARIABLE_NAME = 'Threads_connected'").Scan(&v)
		s.Connections, _ = strconv.Atoi(v)
	}
	_ = m.db.QueryRowContext(ctx, "SELECT @@max_connections").Scan(&s.MaxConnections)
	_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM performance_schema.data_lock_waits").Scan(&s.LockWaits)
	rows, err = m.db.QueryContext(ctx, `SELECT l.OBJECT_NAME, COUNT(*) FROM performance_schema.data_lock_waits w
		JOIN performance_schema.data_locks l ON l.ENGINE_LOCK_ID = w.REQUESTING_ENGINE_LOCK_ID GROUP BY l.OBJECT_NAME`)
	if err == nil {
		for rows.Next() {
			var k string
			var n int
			if rows.Scan(&k, &n) == nil {
				s.Blocked[strings.ToLower(k)] += n
			}
		}
		rows.Close()
	}
	return s, nil
}

// sortedKeys is a helper for deterministic iteration.
func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
