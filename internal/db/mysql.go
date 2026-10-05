package db

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
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
	prevGL    string
	prevOut   string
	truncate  bool
	capturing bool
	probeDSN  string
}

func openMySQL(ctx context.Context, u *url.URL) (DB, error) {
	user := u.User.Username()
	pass, _ := u.User.Password()
	host := u.Host
	if u.Port() == "" {
		host += ":3306"
	}
	dbname := strings.TrimPrefix(u.Path, "/")
	q := u.Query()
	q.Set("parseTime", "true")
	q.Set("sql_log_off", "1")
	q.Set("time_zone", "'+00:00'")
	q.Set("information_schema_stats_expiry", "0")
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
		return nil, fmt.Errorf("connect mysql (routeperf needs SYSTEM_VARIABLES_ADMIN for sql_log_off): %w", err)
	}
	ex, err := d.Conn(ctx)
	if err != nil {
		return nil, err
	}
	m := &my{db: d, ex: ex, probeDSN: probeDSN}
	var ver string
	if err := d.QueryRowContext(ctx, "SELECT VERSION(), DATABASE()").Scan(&ver, &m.info.Database); err != nil {
		return nil, err
	}
	var super int
	_ = d.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.USER_PRIVILEGES WHERE PRIVILEGE_TYPE IN ('SUPER') AND GRANTEE = CONCAT(\"'\", SUBSTRING_INDEX(CURRENT_USER(), '@', 1), \"'@'\", SUBSTRING_INDEX(CURRENT_USER(), '@', -1), \"'\")").Scan(&super)
	m.info.Dialect, m.info.Version, m.info.Super, m.info.Host = "mysql", ver, super > 0, u.Hostname()
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

func (m *my) Tables(ctx context.Context) (map[string]*Table, error) {
	out := map[string]*Table{}
	rows, err := m.db.QueryContext(ctx, `SELECT TABLE_NAME, COALESCE(TABLE_ROWS,0) FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE'`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t := &Table{Schema: m.info.Database, Indexed: map[string]bool{}}
		if err := rows.Scan(&t.Name, &t.Rows); err != nil {
			rows.Close()
			return nil, err
		}
		out[strings.ToLower(t.Name)] = t
	}
	rows.Close()
	cols, err := m.db.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME, EXTRA FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() ORDER BY TABLE_NAME, ORDINAL_POSITION`)
	if err != nil {
		return nil, err
	}
	for cols.Next() {
		var tn, cn, extra string
		if err := cols.Scan(&tn, &cn, &extra); err != nil {
			cols.Close()
			return nil, err
		}
		if t := out[strings.ToLower(tn)]; t != nil && !strings.Contains(strings.ToUpper(extra), "GENERATED") {
			t.Cols = append(t.Cols, cn)
		}
	}
	cols.Close()
	idx, err := m.db.QueryContext(ctx, `SELECT TABLE_NAME, COLUMN_NAME, INDEX_NAME FROM information_schema.STATISTICS WHERE TABLE_SCHEMA = DATABASE() AND SEQ_IN_INDEX = 1`)
	if err != nil {
		return nil, err
	}
	for idx.Next() {
		var tn, cn, in string
		if err := idx.Scan(&tn, &cn, &in); err != nil {
			idx.Close()
			return nil, err
		}
		if t := out[strings.ToLower(tn)]; t != nil {
			t.Indexed[strings.ToLower(cn)] = true
			if in == "PRIMARY" {
				t.PK = cn
			}
		}
	}
	idx.Close()
	for _, t := range out {
		if t.Rows < 1000 {
			_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+qmy(t.Name)).Scan(&t.Rows)
		}
	}
	return out, nil
}

func (m *my) FKs(ctx context.Context, tables map[string]*Table) ([]FK, error) {
	rows, err := m.db.QueryContext(ctx, `
SELECT kcu.TABLE_NAME, kcu.COLUMN_NAME, kcu.REFERENCED_TABLE_NAME, kcu.REFERENCED_COLUMN_NAME, rc.DELETE_RULE
FROM information_schema.KEY_COLUMN_USAGE kcu
JOIN information_schema.REFERENTIAL_CONSTRAINTS rc ON rc.CONSTRAINT_SCHEMA = kcu.CONSTRAINT_SCHEMA AND rc.CONSTRAINT_NAME = kcu.CONSTRAINT_NAME
WHERE kcu.TABLE_SCHEMA = DATABASE() AND kcu.REFERENCED_TABLE_NAME IS NOT NULL AND kcu.ORDINAL_POSITION = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FK
	for rows.Next() {
		f := FK{Declared: true, Indexed: true}
		if err := rows.Scan(&f.Child, &f.ChildCol, &f.Parent, &f.ParentCol, &f.OnDelete); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return InferFKs(tables, out), rows.Err()
}

func (m *my) HashPred(expr string, frac float64) string {
	return fmt.Sprintf("(CRC32(%s) %% 10000) < %d", expr, int(frac*10000+0.5))
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

func (m *my) StartCapture(ctx context.Context) error {
	if err := m.db.QueryRowContext(ctx, "SELECT @@GLOBAL.general_log, @@GLOBAL.log_output").Scan(&m.prevGL, &m.prevOut); err != nil {
		return err
	}
	var n int
	_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.general_log").Scan(&n)
	m.truncate = n == 0
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL log_output = 'TABLE'"); err != nil {
		return fmt.Errorf("enable general_log (needs SYSTEM_VARIABLES_ADMIN): %w", err)
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL general_log = 'ON'"); err != nil {
		return err
	}
	m.capturing = true
	return nil
}

func (m *my) StopCapture(ctx context.Context) error {
	if !m.capturing {
		return nil
	}
	gl := "OFF"
	if m.prevGL == "1" || strings.EqualFold(m.prevGL, "ON") {
		gl = "ON"
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL general_log = '"+gl+"'"); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx, "SET GLOBAL log_output = '"+strings.ReplaceAll(m.prevOut, "'", "")+"'"); err != nil {
		return err
	}
	if m.truncate {
		_, _ = m.db.ExecContext(ctx, "TRUNCATE TABLE mysql.general_log")
	}
	m.capturing = false
	return nil
}

// Mark is the general_log row count: the CSV table is append-only and scans in
// insertion order, and event_time is unusable (stored in each session's zone).
func (m *my) Mark(ctx context.Context) (Mark, error) {
	var n int64
	err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.general_log").Scan(&n)
	return Mark{Offset: n, Time: time.Now()}, err
}

func (m *my) Window(ctx context.Context, from, to Mark) ([]analyze.Stmt, error) {
	if to.Offset <= from.Offset {
		return nil, nil
	}
	rows, err := m.db.QueryContext(ctx, `SELECT command_type, CONVERT(argument USING utf8mb4) FROM mysql.general_log LIMIT ? OFFSET ?`,
		to.Offset-from.Offset, from.Offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []analyze.Stmt
	for rows.Next() {
		var ct, s string
		if err := rows.Scan(&ct, &s); err != nil {
			return nil, err
		}
		if ct != "Query" && ct != "Execute" {
			continue
		}
		s = strings.TrimSpace(s)
		out = append(out, analyze.Stmt{SQL: s, Kind: sqlutil.Kind(s), Fingerprint: sqlutil.Fingerprint(s), Tables: sqlutil.Tables(s)})
	}
	return out, rows.Err()
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

func (m *my) Explain(ctx context.Context, st analyze.Stmt, ns string, tables map[string]*Table) (*plan.Plan, error) {
	q := st.SQL
	if ns != "" {
		q = sqlutil.RewriteSchema(q, m.info.Database, ns, st.Tables)
		if _, err := m.ex.ExecContext(ctx, "USE "+qmy(ns)); err != nil {
			return nil, err
		}
		defer m.ex.ExecContext(context.Background(), "USE "+qmy(m.info.Database))
	}
	tx, err := m.ex.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	aliases := plan.MySQLAliases(q)
	if st.Kind == "select" {
		var text string
		if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+q).Scan(&text); err != nil {
			return nil, err
		}
		return plan.ParseMySQLTree(text, aliases)
	}
	// DML: EXPLAIN ANALYZE covers only multi-table UPDATE/DELETE.
	var text string
	if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+q).Scan(&text); err == nil {
		return plan.ParseMySQLTree(text, aliases)
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
	root := &plan.Node{Op: plan.OpModify, RawType: strings.ToUpper(st.Kind), Relation: sqlutil.TargetTable(q), Rows: 1, Loops: 1, TimeMs: elapsed}
	p := &plan.Plan{Dialect: "mysql", Root: root, ExecutionMs: elapsed, HandlerRows: after - base1 - overhead}
	if probe := sqlutil.WhereProbe(q); probe != "" {
		if err := tx.QueryRowContext(ctx, "EXPLAIN ANALYZE "+probe).Scan(&text); err == nil {
			if pp, err := plan.ParseMySQLTree(text, plan.MySQLAliases(probe)); err == nil {
				root.Children = []*plan.Node{pp.Root}
			}
		}
	}
	if p.HandlerRows < 1 {
		p.HandlerRows = 1
	}
	return plan.Finish(p), nil
}

// ---------------------------------------------------------------- subsets

func (m *my) BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error) {
	if _, err := m.db.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS "+qmy(ns)); err != nil {
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
			cols[i] = qmy(c)
		}
		cl := strings.Join(cols, ", ")
		scl := "s." + strings.Join(cols, ", s.")
		pred := subsetPred(m, t, ns, frac, fks, inScope, qmy)
		dst := qmy(ns) + "." + qmy(t.Name)
		for _, s := range []string{
			"DROP TABLE IF EXISTS " + dst,
			fmt.Sprintf("CREATE TABLE %s LIKE %s.%s", dst, qmy(t.Schema), qmy(t.Name)),
			fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s.%s s WHERE %s", dst, cl, scl, qmy(srcSchema(src, t)), qmy(t.Name), pred),
			"ANALYZE TABLE " + dst,
		} {
			if _, err := m.db.ExecContext(ctx, s); err != nil {
				return nil, fmt.Errorf("subset %s: %w", dst, err)
			}
		}
		var n float64
		_ = m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+dst).Scan(&n)
		counts[t.Name] = n
	}
	for _, f := range fks {
		if f.Declared && inScope[f.Child] && inScope[f.Parent] {
			rule := f.OnDelete
			if rule == "" {
				rule = "NO ACTION"
			}
			if _, err := m.db.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s.%s ADD FOREIGN KEY (%s) REFERENCES %s.%s (%s) ON DELETE %s",
				qmy(ns), qmy(f.Child), qmy(f.ChildCol), qmy(ns), qmy(f.Parent), qmy(f.ParentCol), rule)); err != nil {
				return nil, fmt.Errorf("subset fk %s.%s: %w", f.Child, f.ChildCol, err)
			}
		}
	}
	return counts, nil
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
		if _, err := m.db.ExecContext(ctx, fmt.Sprintf("CREATE TABLE %s.%s AS SELECT * FROM %s.%s", SnapNS, qmy(t.Name), qmy(t.Schema), qmy(t.Name))); err != nil {
			return fmt.Errorf("snapshot %s: %w", t.Name, err)
		}
	}
	_, err := m.db.ExecContext(ctx, "INSERT INTO "+SnapNS+"._rp_ai SELECT TABLE_NAME, AUTO_INCREMENT FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND AUTO_INCREMENT IS NOT NULL")
	return err
}

func (m *my) Counters(ctx context.Context) (map[string]float64, error) {
	rows, err := m.db.QueryContext(ctx, `SELECT OBJECT_NAME, COUNT_INSERT + COUNT_UPDATE + COUNT_DELETE FROM performance_schema.table_io_waits_summary_by_table WHERE OBJECT_SCHEMA = DATABASE()`)
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
		cols := make([]string, len(t.Cols))
		for i, cn := range t.Cols {
			cols[i] = qmy(cn)
		}
		cl := strings.Join(cols, ", ")
		dst := qmy(t.Schema) + "." + qmy(t.Name)
		for _, s := range []string{"TRUNCATE TABLE " + dst,
			fmt.Sprintf("INSERT INTO %s (%s) SELECT %s FROM %s.%s", dst, cl, cl, SnapNS, qmy(t.Name))} {
			if _, err := c.ExecContext(ctx, s); err != nil {
				return fmt.Errorf("restore %s: %w", t.Name, err)
			}
		}
		var ai sql.NullInt64
		_ = c.QueryRowContext(ctx, "SELECT v FROM "+SnapNS+"._rp_ai WHERE t = ?", t.Name).Scan(&ai)
		if ai.Valid {
			_, _ = c.ExecContext(ctx, fmt.Sprintf("ALTER TABLE %s AUTO_INCREMENT = %d", dst, ai.Int64))
		}
		_, _ = c.ExecContext(ctx, "ANALYZE TABLE "+dst)
	}
	return nil
}

func (m *my) LogSource() string { return "mysql.general_log (TABLE)" }

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
