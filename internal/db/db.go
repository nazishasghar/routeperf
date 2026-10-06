// Package db implements the dialect layer: catalog, statement capture from the
// server log, EXPLAIN ANALYZE replay, nested data subsets and snapshots.
package db

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/plan"
	"github.com/nazishasghar/routeperf/internal/sqlutil"
)

// Table is a catalog relation. Tables are keyed by Key: the lowercase name
// when it resolves unqualified (search_path / current database), otherwise
// "schema.name".
type Table struct {
	Schema  string
	Name    string
	Key     string
	SQL     string // quoted, schema-qualified name
	Kind    string // table | partitioned | view | matview
	PK      string // first primary-key column
	PKCols  []string
	Rows    float64
	Cols    []string            // insertable columns, in order
	NotNull map[string]bool     // NOT NULL columns (lowercase)
	Indexed map[string]bool     // leading index columns
	Indexes map[string][]string // index name → ordered columns (lowercase)
	ViewDef string              // views: defining SELECT
	PartKey string              // Postgres partitioned tables: partition key definition
	Parts   []Partition         // Postgres partitions, parents first
}

// Partition is one Postgres partition (possibly itself partitioned).
type Partition struct {
	Name, Schema, Parent, Bound, PartKey string
}

// NSName is the table's name inside routeperf namespaces (_rp_*).
func (t *Table) NSName() string { return strings.ReplaceAll(t.Key, ".", "__") }

// HasData reports whether the relation stores rows (not a plain view).
func (t *Table) HasData() bool { return t.Kind != "view" }

// FK is a foreign key between catalog keys. ChildCol/ParentCol are the first
// columns; ChildCols/ParentCols hold all of them (composite keys).
type FK struct {
	Child, ChildCol, Parent, ParentCol string
	ChildCols, ParentCols              []string
	Declared, Indexed                  bool
	OnDelete                           string
	// Deferred marks an FK that closes a cycle (self-reference, or tables
	// referencing each other): no load order satisfies it, so subsets sample
	// the child without it. See MarkCycles.
	Deferred bool
}

type Mark struct {
	Offset int64
	Time   time.Time
	Timer  int64 // performance_schema timer (MySQL), picoseconds
}

type Info struct {
	Dialect, Version, Database, Host string
	Super                            bool
	Major                            int
}

// Target says where a replay reads: live tables, or copies in namespace NS
// (for the keys in Only, or every catalog table when Only is nil).
type Target struct {
	NS   string
	Only map[string]bool
}

// Capture collects the SQL the application issues.
type Capture interface {
	StartCapture(ctx context.Context) error
	Mark(ctx context.Context) (Mark, error)
	Window(ctx context.Context, from, to Mark) ([]analyze.Stmt, error)
	StopCapture(ctx context.Context) error
	// Probe issues a tagged query from a foreign session and checks that the
	// capture saw it (capture must be started).
	Probe(ctx context.Context) error
	LogSource() string
}

var ErrUnsupported = errors.New("not supported on this database")

type DB interface {
	Capture
	Info() Info
	Close()
	Tables(ctx context.Context) (map[string]*Table, error)
	FKs(ctx context.Context, tables map[string]*Table) ([]FK, error)
	HashPred(expr string, frac float64) string
	// KeyExpr is the expression subsets hash on: the primary key (all
	// columns) or the whole row.
	KeyExpr(t *Table, alias string) string
	Quote(ident string) string
	FirstValues(ctx context.Context, sql string) ([]string, error)
	Explain(ctx context.Context, st analyze.Stmt, tgt Target) (*plan.Plan, error)
	// BuildSubset copies structure from each table's schema and data from src
	// (a snapshot namespace) or the live table when src is "".
	BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error)
	// BuildShrunk copies one table into ns at frac (by key hash) plus the
	// rows whose first primary-key value is in keep, without foreign keys.
	BuildShrunk(ctx context.Context, ns string, frac float64, t *Table, keep []string, src string) (float64, error)
	DropNamespace(ctx context.Context, ns string) error
	Snapshot(ctx context.Context, tables []*Table) error
	Counters(ctx context.Context) (map[string]float64, error)
	Restore(ctx context.Context, tables []*Table) error
	HasSnapshot(ctx context.Context) bool
	// HypoIndex plans st with and without a hypothetical index (HypoPG).
	HypoIndex(ctx context.Context, st analyze.Stmt, createIndex string) (Hypo, error)
	// Evict drops the given relations (and their indexes) from the DB cache.
	Evict(ctx context.Context, tables []*Table) error
	// LoadSample samples server-side activity during load runs.
	LoadSample(ctx context.Context) (LoadSample, error)
}

// Hypo compares the planner's estimate with and without a hypothetical index.
type Hypo struct {
	CostBefore, CostAfter float64
	RowsBefore, RowsAfter float64 // estimated rows examined by scans
	Used                  bool    // the new plan uses the hypothetical index
	ScanBefore, ScanAfter string  // how the indexed table is read, e.g. "Seq Scan" → "Index Scan"
	PlanAfter             string
}

// LoadSample is one snapshot of DB activity (connections, lock waits).
type LoadSample struct {
	Active, Connections, MaxConnections int
	LockWaits                           int
	Waits                               map[string]int // wait event → sessions
	Blocked                             map[string]int // relation → sessions waiting on a lock
}

type Options struct {
	PGLogFile string
	Schemas   []string // MySQL: extra databases to include in the catalog
	NoSilence bool     // proxy capture: don't touch log settings (no admin rights needed)
}

func Open(ctx context.Context, rawURL string, opt Options) (DB, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("db url: %w", err)
	}
	switch u.Scheme {
	case "postgres", "postgresql":
		return openPG(ctx, rawURL, u, opt)
	case "mysql":
		return openMySQL(ctx, u, opt)
	}
	return nil, fmt.Errorf("unsupported db scheme %q (use postgres:// or mysql://)", u.Scheme)
}

// IsLocalHost reports whether the DB URL points at this machine.
func IsLocalHost(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	h := u.Hostname()
	if h == "" || h == "localhost" || strings.HasPrefix(h, "/") {
		return true
	}
	if q := u.Query().Get("host"); strings.HasPrefix(q, "/") {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate() && strings.HasPrefix(h, "172.17."))
}

// Resolve maps a reference (schema may be "") to a catalog table.
func Resolve(tables map[string]*Table, schema, name string) *Table {
	if schema == "" {
		return tables[strings.ToLower(name)]
	}
	if t := tables[strings.ToLower(schema+"."+name)]; t != nil {
		return t
	}
	if t := tables[strings.ToLower(name)]; t != nil && strings.EqualFold(t.Schema, schema) {
		return t
	}
	return nil
}

// byNSName finds a table by its namespace name (for relations in _rp_*).
func byNSName(tables map[string]*Table, n string) *Table {
	n = strings.ToLower(n)
	if t := tables[n]; t != nil && t.NSName() == n {
		return t
	}
	for _, t := range tables {
		if t.NSName() == n {
			return t
		}
	}
	return nil
}

// MakeStmt parses a captured statement and resolves its tables to keys.
func MakeStmt(dialect, sql string, cat map[string]*Table) analyze.Stmt {
	p := sqlutil.Parse(dialect, sql)
	st := analyze.Stmt{SQL: sql, Kind: p.Kind, Fingerprint: p.Fingerprint, TraceID: sqlutil.TraceID(sql)}
	seen := map[string]bool{}
	for _, r := range p.Refs {
		k := strings.ToLower(r.Name)
		if t := Resolve(cat, r.Schema, r.Name); t != nil {
			k = t.Key
		}
		if !seen[k] {
			seen[k] = true
			st.Tables = append(st.Tables, k)
		}
	}
	if p.Target.Name != "" {
		st.Target = strings.ToLower(p.Target.Name)
		if t := Resolve(cat, p.Target.Schema, p.Target.Name); t != nil {
			st.Target = t.Key
		}
	}
	return st
}

// rewriteTo redirects table references into tgt.NS (nil error, unchanged SQL
// when tgt.NS is empty).
func rewriteTo(dialect, sql string, cat map[string]*Table, tgt Target) (string, error) {
	if tgt.NS == "" {
		return sql, nil
	}
	return sqlutil.Rewrite(dialect, sql, func(r sqlutil.Ref) (string, string, bool) {
		t := Resolve(cat, r.Schema, r.Name)
		if t == nil || tgt.Only != nil && !tgt.Only[t.Key] {
			return "", "", false
		}
		return tgt.NS, t.NSName(), true
	})
}

// normalizeRelations maps plan relation names (partitions, namespace copies,
// other schemas) back to catalog keys.
func normalizeRelations(p *plan.Plan, cat map[string]*Table, parts map[string]string) {
	p.Walk(func(n *plan.Node, _ int) {
		if n.Relation == "" || strings.HasPrefix(n.Relation, "<") {
			return
		}
		rel, schema := n.Relation, n.Schema
		if k, ok := parts[strings.ToLower(rel)]; ok {
			n.Relation = k
			return
		}
		if strings.HasPrefix(schema, "_rp_") || schema == "" {
			if t := byNSName(cat, rel); t != nil {
				n.Relation = t.Key
				return
			}
		}
		if t := Resolve(cat, schema, rel); t != nil {
			n.Relation = t.Key
		}
	})
	plan.Reindex(p) // node paths must match across data sizes
}

var idCol = regexp.MustCompile(`(?i)^(\w+?)_?id$`)

// InferFKs adds naming-convention FKs (user_id → users.id) where none are
// declared, so subsets stay join-consistent for ORMs/MySQL without FKs.
func InferFKs(tables map[string]*Table, declared []FK) []FK {
	have := map[string]bool{}
	for _, f := range declared {
		for _, c := range f.ChildCols {
			have[f.Child+"."+strings.ToLower(c)] = true
		}
	}
	out := append([]FK(nil), declared...)
	keys := make([]string, 0, len(tables))
	for k := range tables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t := tables[k]
		if !t.HasData() {
			continue
		}
		for _, c := range t.Cols {
			if strings.EqualFold(c, t.PK) || have[t.Key+"."+strings.ToLower(c)] {
				continue
			}
			m := idCol.FindStringSubmatch(c)
			if m == nil || m[1] == "" {
				continue
			}
			base := strings.ToLower(m[1])
			for _, cand := range []string{base + "s", base, base + "es", strings.TrimSuffix(base, "y") + "ies"} {
				if p, ok := tables[cand]; ok && p.PK != "" && len(p.PKCols) == 1 && p.Key != t.Key && p.HasData() {
					out = append(out, FK{Child: t.Key, ChildCol: c, ChildCols: []string{c}, Parent: p.Key, ParentCol: p.PK, ParentCols: []string{p.PK},
						Indexed: t.Indexed[strings.ToLower(c)]})
					break
				}
			}
		}
	}
	return out
}

// fkIndexed reports whether some index of t starts with exactly cols (any order).
func fkIndexed(t *Table, cols []string) bool {
	if t == nil {
		return false
	}
	want := map[string]bool{}
	for _, c := range cols {
		want[strings.ToLower(c)] = true
	}
	for _, ic := range t.Indexes {
		if len(ic) < len(cols) {
			continue
		}
		ok := true
		for _, c := range ic[:len(cols)] {
			if !want[c] {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}

// MarkCycles flags the FKs that close a cycle: self-references
// (category.parent_id) and tables that reference each other
// (customer.primary_member_id ⇄ member.customer_id). No load order satisfies
// them, so subsets sample the child without them and keep its values as they
// are (possibly dangling). Within a cycle, tables are ordered so NOT NULL
// references hold (usually member.customer_id: members follow their
// customer into a subset) and the nullable one pointing back is deferred;
// otherwise the smaller table loads first. The choice depends only on the
// catalog, so the subset builder and the anchor sampler agree on it.
func MarkCycles(tables map[string]*Table, fks []FK) []FK {
	adj := map[string][]string{}
	var nodes []string
	seen := map[string]bool{}
	for _, f := range fks {
		for _, n := range []string{f.Child, f.Parent} {
			if !seen[n] {
				seen[n] = true
				nodes = append(nodes, n)
			}
		}
		if f.Child != f.Parent {
			adj[f.Child] = append(adj[f.Child], f.Parent)
		}
	}
	sort.Strings(nodes)
	// Tarjan's strongly connected components
	comp := map[string]int{}
	index, low, onStack := map[string]int{}, map[string]int{}, map[string]bool{}
	var stack []string
	var members [][]string
	var strong func(n string)
	strong = func(n string) {
		index[n], low[n] = len(index), len(index)
		stack = append(stack, n)
		onStack[n] = true
		for _, p := range adj[n] {
			if _, ok := index[p]; !ok {
				strong(p)
				low[n] = min(low[n], low[p])
			} else if onStack[p] {
				low[n] = min(low[n], index[p])
			}
		}
		if low[n] == index[n] {
			var c []string
			for {
				m := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[m] = false
				comp[m] = len(members)
				c = append(c, m)
				if m == n {
					break
				}
			}
			members = append(members, c)
		}
	}
	for _, n := range nodes {
		if _, ok := index[n]; !ok {
			strong(n)
		}
	}
	required := func(f FK) bool {
		t := tables[f.Child]
		if t == nil {
			return false
		}
		for _, c := range f.ChildCols {
			if !t.NotNull[strings.ToLower(c)] {
				return false
			}
		}
		return true
	}
	rows := func(n string) float64 {
		if t := tables[n]; t != nil {
			return t.Rows
		}
		return 0
	}
	// order each cycle's tables: parents of NOT NULL references first
	pos := map[string]int{}
	for ci, c := range members {
		if len(c) < 2 {
			continue
		}
		waits := map[string]int{} // NOT NULL parents not placed yet
		for _, f := range fks {
			if f.Child != f.Parent && comp[f.Child] == ci && comp[f.Parent] == ci && required(f) {
				waits[f.Child]++
			}
		}
		left := append([]string(nil), c...)
		for len(left) > 0 {
			sort.Slice(left, func(i, j int) bool {
				a, b := left[i], left[j]
				if (waits[a] == 0) != (waits[b] == 0) {
					return waits[a] == 0
				}
				if rows(a) != rows(b) {
					return rows(a) < rows(b)
				}
				return a < b
			})
			n := left[0] // when NOT NULL references cycle too, one of them gives
			left = left[1:]
			pos[n] = len(c) - len(left)
			for _, f := range fks {
				if f.Parent == n && f.Child != n && comp[f.Child] == ci && required(f) {
					waits[f.Child]--
				}
			}
		}
	}
	out := append([]FK(nil), fks...)
	for i := range out {
		f := &out[i]
		f.Deferred = f.Child == f.Parent || comp[f.Child] == comp[f.Parent] && pos[f.Parent] > pos[f.Child]
	}
	return out
}

// TopoOrder returns tables parents-first within scope (FKs that close a
// cycle are ignored, see MarkCycles).
func TopoOrder(scope []*Table, fks []FK) []*Table {
	in := map[string]*Table{}
	for _, t := range scope {
		in[t.Key] = t
	}
	deps := map[string][]string{}
	for _, f := range fks {
		if in[f.Child] != nil && in[f.Parent] != nil && f.Child != f.Parent && !f.Deferred {
			deps[f.Child] = append(deps[f.Child], f.Parent)
		}
	}
	var out []*Table
	state := map[string]int{}
	var visit func(n string)
	visit = func(n string) {
		if state[n] != 0 {
			return
		}
		state[n] = 1
		for _, p := range deps[n] {
			if state[p] == 0 {
				visit(p)
			}
		}
		state[n] = 2
		out = append(out, in[n])
	}
	names := make([]string, 0, len(in))
	for n := range in {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		visit(n)
	}
	return out
}

// ChildClosure adds tables reachable through declared FKs from the given
// parents (cascade / RI-check targets of DELETE and UPDATE).
func ChildClosure(names []string, fks []FK) []string {
	set := map[string]bool{}
	var add func(n string)
	add = func(n string) {
		if set[n] {
			return
		}
		set[n] = true
		for _, f := range fks {
			if f.Declared && f.Parent == n {
				add(f.Child)
			}
		}
	}
	for _, n := range names {
		add(strings.ToLower(n))
	}
	var out []string
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParentClosure expands a set of tables with their FK parents and, for views,
// the relations they read.
func ParentClosure(dialect string, names []string, tables map[string]*Table, fks []FK) []*Table {
	set := map[string]bool{}
	var add func(n string)
	add = func(n string) {
		t := tables[n]
		if set[n] || t == nil {
			return
		}
		set[n] = true
		for _, f := range fks {
			if f.Child == n {
				add(f.Parent)
			}
		}
		if t.Kind == "view" {
			for _, d := range ViewDeps(dialect, t, tables) {
				add(d.Key)
			}
		}
	}
	for _, n := range names {
		add(strings.ToLower(n))
	}
	var out []*Table
	keys := make([]string, 0, len(set))
	for n := range set {
		keys = append(keys, n)
	}
	sort.Strings(keys)
	for _, n := range keys {
		out = append(out, tables[n])
	}
	return out
}

// ViewDeps returns the catalog relations a view reads.
func ViewDeps(dialect string, v *Table, tables map[string]*Table) []*Table {
	var out []*Table
	for _, r := range sqlutil.Parse(dialect, v.ViewDef).Refs {
		if t := Resolve(tables, r.Schema, r.Name); t != nil && t.Key != v.Key {
			out = append(out, t)
		}
	}
	return out
}

// subsetPred builds the FK-consistent sampling predicate for table t.
func subsetPred(d DB, t *Table, ns string, frac float64, fks []FK, tables map[string]*Table, inScope map[string]bool) string {
	q := d.Quote
	var conds []string
	for _, f := range fks {
		if f.Child == t.Key && f.Parent != t.Key && !f.Deferred && inScope[f.Parent] {
			p := tables[f.Parent]
			var nulls, eqs []string
			for i := range f.ChildCols {
				nulls = append(nulls, fmt.Sprintf("s.%s IS NULL", q(f.ChildCols[i])))
				eqs = append(eqs, fmt.Sprintf("p.%s = s.%s", q(f.ParentCols[i]), q(f.ChildCols[i])))
			}
			conds = append(conds, fmt.Sprintf("(%s OR EXISTS (SELECT 1 FROM %s.%s p WHERE %s))",
				strings.Join(nulls, " OR "), q(ns), q(p.NSName()), strings.Join(eqs, " AND ")))
		}
	}
	if len(conds) > 0 {
		return strings.Join(conds, " AND ")
	}
	return d.HashPred(d.KeyExpr(t, "s"), frac)
}

// NSName gives the namespace for a subset step, e.g. 0.03 → _rp_s03.
func NSName(frac float64) string {
	return fmt.Sprintf("_rp_s%02d", int(frac*100+0.5))
}

// PTName gives the per-table shrink namespace for a step, e.g. 0.1 → _rp_p10.
func PTName(frac float64) string {
	return fmt.Sprintf("_rp_p%02d", int(frac*100+0.5))
}

const SnapNS = "_rp_snap"

const nullMarker = "\x00NULL"

func IsNull(s string) bool { return s == nullMarker }

// sortViews orders views so that a view comes after the views it reads.
func sortViews(dialect string, views []*Table, tables map[string]*Table) []*Table {
	in := map[string]bool{}
	for _, v := range views {
		in[v.Key] = true
	}
	var out []*Table
	done := map[string]bool{}
	var visit func(v *Table)
	visit = func(v *Table) {
		if done[v.Key] {
			return
		}
		done[v.Key] = true
		for _, d := range ViewDeps(dialect, v, tables) {
			if in[d.Key] {
				visit(d)
			}
		}
		out = append(out, v)
	}
	for _, v := range views {
		visit(v)
	}
	return out
}
