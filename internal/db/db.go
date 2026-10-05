// Package db implements the dialect layer: catalog, statement capture from the
// server log, EXPLAIN ANALYZE replay, nested data subsets and snapshots.
package db

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/plan"
)

type Table struct {
	Schema  string
	Name    string
	PK      string
	Rows    float64
	Cols    []string            // insertable columns, in order
	Indexed map[string]bool     // leading index columns
	Indexes map[string][]string // index name → ordered columns (lowercase)
}

type FK struct {
	Child, ChildCol, Parent, ParentCol string
	Declared, Indexed                  bool
	OnDelete                           string
}

type Mark struct {
	Offset int64
	Time   time.Time
}

type Info struct {
	Dialect, Version, Database, Host string
	Super                            bool
}

type DB interface {
	Info() Info
	Close()
	Tables(ctx context.Context) (map[string]*Table, error)
	FKs(ctx context.Context, tables map[string]*Table) ([]FK, error)
	HashPred(expr string, frac float64) string
	FirstValues(ctx context.Context, sql string) ([]string, error)
	StartCapture(ctx context.Context) error
	Mark(ctx context.Context) (Mark, error)
	Window(ctx context.Context, from, to Mark) ([]analyze.Stmt, error)
	StopCapture(ctx context.Context) error
	Explain(ctx context.Context, st analyze.Stmt, ns string, tables map[string]*Table) (*plan.Plan, error)
	// BuildSubset copies structure from each table's schema and data from src
	// (a snapshot namespace) or the live table when src is "".
	BuildSubset(ctx context.Context, ns string, frac float64, order []*Table, fks []FK, src string) (map[string]float64, error)
	DropNamespace(ctx context.Context, ns string) error
	Snapshot(ctx context.Context, tables []*Table) error
	Counters(ctx context.Context) (map[string]float64, error)
	Restore(ctx context.Context, tables []*Table) error
	HasSnapshot(ctx context.Context) bool
	// Probe issues a tagged query from a foreign session and checks that the
	// statement log captured it (capture must be started).
	Probe(ctx context.Context) error
	LogSource() string
}

type Options struct {
	PGLogFile string
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
		return openMySQL(ctx, u)
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

var idCol = regexp.MustCompile(`(?i)^(\w+?)_?id$`)

// InferFKs adds naming-convention FKs (user_id → users.id) where none are
// declared, so subsets stay join-consistent for ORMs/MySQL without FKs.
func InferFKs(tables map[string]*Table, declared []FK) []FK {
	have := map[string]bool{}
	for _, f := range declared {
		have[f.Child+"."+f.ChildCol] = true
	}
	out := append([]FK(nil), declared...)
	for _, t := range tables {
		for _, c := range t.Cols {
			if strings.EqualFold(c, t.PK) || have[t.Name+"."+c] {
				continue
			}
			m := idCol.FindStringSubmatch(c)
			if m == nil || strings.EqualFold(m[1], "") {
				continue
			}
			base := strings.ToLower(m[1])
			for _, cand := range []string{base + "s", base, base + "es", strings.TrimSuffix(base, "y") + "ies"} {
				if p, ok := tables[cand]; ok && p.PK != "" && p.Name != t.Name {
					out = append(out, FK{Child: t.Name, ChildCol: c, Parent: p.Name, ParentCol: p.PK, Indexed: t.Indexed[strings.ToLower(c)]})
					break
				}
			}
		}
	}
	return out
}

// TopoOrder returns tables parents-first within scope.
func TopoOrder(scope []*Table, fks []FK) []*Table {
	in := map[string]*Table{}
	for _, t := range scope {
		in[t.Name] = t
	}
	deps := map[string][]string{}
	for _, f := range fks {
		if in[f.Child] != nil && in[f.Parent] != nil && f.Child != f.Parent {
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
	return out
}

// ParentClosure expands a set of tables with their FK parents.
func ParentClosure(names []string, tables map[string]*Table, fks []FK) []*Table {
	set := map[string]bool{}
	var add func(n string)
	add = func(n string) {
		if set[n] || tables[n] == nil {
			return
		}
		set[n] = true
		for _, f := range fks {
			if f.Child == n {
				add(f.Parent)
			}
		}
	}
	for _, n := range names {
		add(strings.ToLower(n))
	}
	var out []*Table
	for n := range set {
		out = append(out, tables[n])
	}
	return out
}

// subsetPred builds the FK-consistent sampling predicate for table t.
func subsetPred(d DB, t *Table, ns string, frac float64, fks []FK, inScope map[string]bool, quote func(string) string) string {
	var conds []string
	for _, f := range fks {
		if f.Child == t.Name && f.Parent != t.Name && inScope[f.Parent] {
			conds = append(conds, fmt.Sprintf("(s.%s IS NULL OR EXISTS (SELECT 1 FROM %s.%s p WHERE p.%s = s.%s))",
				quote(f.ChildCol), quote(ns), quote(f.Parent), quote(f.ParentCol), quote(f.ChildCol)))
		}
	}
	if len(conds) > 0 {
		return strings.Join(conds, " AND ")
	}
	key := "s." + quote(t.PK)
	if t.PK == "" {
		key = rowKey(d, t, quote)
	}
	return d.HashPred(key, frac)
}

func rowKey(d DB, t *Table, quote func(string) string) string {
	if d.Info().Dialect == "postgres" {
		return "s::text"
	}
	var cs []string
	for _, c := range t.Cols {
		cs = append(cs, "s."+quote(c))
	}
	return "CONCAT_WS(','," + strings.Join(cs, ",") + ")"
}

// NSName gives the namespace for a subset step, e.g. 0.03 → _rp_s03.
func NSName(frac float64) string {
	return fmt.Sprintf("_rp_s%02d", int(frac*100+0.5))
}

const SnapNS = "_rp_snap"

const nullMarker = "\x00NULL"

func IsNull(s string) bool { return s == nullMarker }
