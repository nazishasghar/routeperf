package analyze

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/plan"
)

// Term is coef · Π sym^Vars[sym] · Π log^Logs[sym](sym). Symbols are "k" or "n_<table>".
type Term struct {
	Vars map[string]float64 `json:"vars,omitempty"`
	Logs map[string]float64 `json:"logs,omitempty"`
}

type Expr struct {
	Terms []Term `json:"terms"`
}

func T(vars map[string]float64, logs map[string]float64) Term {
	t := Term{Vars: map[string]float64{}, Logs: map[string]float64{}}
	for k, v := range vars {
		if v > 0 {
			t.Vars[k] = v
		}
	}
	for k, v := range logs {
		if v > 0 {
			t.Logs[k] = v
		}
	}
	return t
}

func (t Term) Mul(o Term) Term {
	r := T(t.Vars, t.Logs)
	for k, v := range o.Vars {
		r.Vars[k] += v
	}
	for k, v := range o.Logs {
		r.Logs[k] += v
	}
	return r
}

func (t Term) dims() (nDeg, nLog, kDeg, kLog float64) {
	for s, v := range t.Vars {
		if s == "k" {
			kDeg += v
		} else {
			nDeg += v
		}
	}
	for s, v := range t.Logs {
		if s == "k" {
			kLog += v
		} else {
			nLog += v
		}
	}
	return
}

func (t Term) key() string {
	var parts []string
	for s, v := range t.Vars {
		parts = append(parts, fmt.Sprintf("v%s=%g", s, v))
	}
	for s, v := range t.Logs {
		parts = append(parts, fmt.Sprintf("l%s=%g", s, v))
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

func ge(a1, a2, b1, b2 float64) bool { // (a1,a2) >=lex (b1,b2)
	if a1 != b1 {
		return a1 > b1
	}
	return a2 >= b2
}

// dominates: all n_* symbols grow together under the growth model, so the n
// dimension compares total degree; k is an independent dimension.
func (t Term) dominates(o Term) bool {
	an, al, ak, akl := t.dims()
	bn, bl, bk, bkl := o.dims()
	return ge(an, al, bn, bl) && ge(ak, akl, bk, bkl)
}

func (e Expr) Add(o Expr) Expr { return Expr{Terms: append(append([]Term{}, e.Terms...), o.Terms...)} }

func (e Expr) MulTerm(m Term) Expr {
	var out Expr
	for _, t := range e.Terms {
		out.Terms = append(out.Terms, t.Mul(m))
	}
	return out
}

// Simplify merges duplicates and drops dominated terms.
func (e Expr) Simplify() Expr {
	uniq := map[string]Term{}
	var order []string
	for _, t := range e.Terms {
		k := t.key()
		if _, ok := uniq[k]; !ok {
			uniq[k] = t
			order = append(order, k)
		}
	}
	var out Expr
	for _, k := range order {
		t := uniq[k]
		dominated := false
		for _, k2 := range order {
			if k2 == k {
				continue
			}
			o := uniq[k2]
			if o.dominates(t) && !t.dominates(o) {
				dominated = true
				break
			}
		}
		if !dominated {
			out.Terms = append(out.Terms, t)
		}
	}
	sort.SliceStable(out.Terms, func(i, j int) bool {
		an, al, ak, _ := out.Terms[i].dims()
		bn, bl, bk, _ := out.Terms[j].dims()
		if an+ak != bn+bk {
			return an+ak > bn+bk
		}
		return al > bl
	})
	return out
}

func fmtExp(v float64) string {
	if v == 1 {
		return ""
	}
	return "^" + trimFloat(v)
}

func trimFloat(v float64) string {
	if v == math.Trunc(v) {
		return fmt.Sprintf("%d", int(v))
	}
	return fmt.Sprintf("%.1f", v)
}

func (t Term) String() string {
	var syms []string
	for s := range t.Vars {
		syms = append(syms, s)
	}
	sort.Slice(syms, func(i, j int) bool {
		if syms[i] == "k" {
			return true
		}
		if syms[j] == "k" {
			return false
		}
		return syms[i] < syms[j]
	})
	var f []string
	for _, s := range syms {
		f = append(f, s+fmtExp(t.Vars[s]))
	}
	var ls []string
	for s := range t.Logs {
		ls = append(ls, s)
	}
	sort.Strings(ls)
	for _, s := range ls {
		p := t.Logs[s]
		if p == 1 {
			f = append(f, "log "+s)
		} else {
			f = append(f, "log^"+trimFloat(p)+" "+s)
		}
	}
	if len(f) == 0 {
		return "1"
	}
	return strings.Join(f, "·")
}

func (e Expr) String() string {
	s := e.Simplify()
	if len(s.Terms) == 0 {
		return "O(1)"
	}
	var parts []string
	for _, t := range s.Terms {
		parts = append(parts, t.String())
	}
	return "O(" + strings.Join(parts, " + ") + ")"
}

// NDegree is the max total degree in table-size symbols.
func (e Expr) NDegree() float64 {
	var d float64
	for _, t := range e.Terms {
		n, _, _, _ := t.dims()
		d = math.Max(d, n)
	}
	return d
}

func (e Expr) KDegree() float64 {
	var d float64
	for _, t := range e.Terms {
		_, _, k, _ := t.dims()
		d = math.Max(d, k)
	}
	return d
}

// Dominant returns the table symbol of the highest-degree term ("" if none).
func (e Expr) Dominant() string {
	s := e.Simplify()
	for _, t := range s.Terms {
		best, bv := "", 0.0
		for sym, v := range t.Vars {
			if sym != "k" && v > bv {
				best, bv = sym, v
			}
		}
		if best != "" {
			return strings.TrimPrefix(best, "n_")
		}
	}
	return ""
}

// NodeGrowth holds exponents of a node's total rows and loops vs data scale.
type NodeGrowth struct {
	Rows    float64 `json:"rows"`  // output rows, all loops
	Loops   float64 `json:"loops"` // executions
	Exam    float64 `json:"exam"`  // rows examined (scans), all loops
	HasExam bool    `json:"-"`
}

func sym(rel string) string { return "n_" + rel }

func domRel(n *plan.Node) string {
	if n.Relation != "" && !strings.HasPrefix(n.Relation, "<") {
		return n.Relation
	}
	best, bv := "", -1.0
	for _, c := range n.Children {
		r := domRel(c)
		if r != "" && c.Rows+c.RowsRemoved > bv {
			best, bv = r, c.Rows+c.RowsRemoved
		}
	}
	return best
}

// FallbackGrowth estimates node growth from plan structure alone (used when
// tables are too small for data-scale experiments).
func FallbackGrowth(p *plan.Plan, tableRows map[string]float64) map[string]NodeGrowth {
	g := map[string]NodeGrowth{}
	var rows func(n *plan.Node) float64
	rows = func(n *plan.Node) float64 {
		var e float64
		switch n.Op {
		case plan.OpSeqScan, plan.OpFullIndexScan, plan.OpIndexRange:
			e = 1
			if tableRows[n.Relation] > 0 && tableRows[n.Relation] < 1000 {
				e = 0
			}
			if ex := n.Rows + n.RowsRemoved; ex > 1000 && n.Rows < 0.001*ex { // selective filter: bounded output
				e = 0
			}
		case plan.OpIndexLookup, plan.OpUniqueLookup, plan.OpLimit:
			e = 0
		default:
			for _, c := range n.Children {
				e = math.Max(e, rows(c))
			}
			if n.Op == plan.OpAggregate && n.Rows <= 1 {
				e = 0
			}
		}
		for _, c := range n.Children { // ensure children are recorded
			if _, ok := g[c.Path]; !ok {
				rows(c)
			}
		}
		ng := g[n.Path]
		ng.Rows = e
		g[n.Path] = ng
		return e
	}
	rows(p.Root)
	var limited func(n *plan.Node, under bool)
	limited = func(n *plan.Node, under bool) {
		if n.Op == plan.OpLimit {
			under = true
		}
		if n.Blocking {
			under = false
		}
		if under && (n.Op == plan.OpSeqScan || n.Op == plan.OpFullIndexScan) && n.RowsRemoved == 0 {
			ng := g[n.Path]
			ng.Exam, ng.HasExam = 0, true
			g[n.Path] = ng
		}
		for _, c := range n.Children {
			limited(c, under)
		}
	}
	limited(p.Root, false)
	var loops func(n *plan.Node, el float64)
	loops = func(n *plan.Node, el float64) {
		ng := g[n.Path]
		ng.Loops = el
		ng.Rows = math.Max(ng.Rows, el)
		if ng.HasExam {
			ng.Exam = math.Max(ng.Exam, el)
		}
		g[n.Path] = ng
		for i, c := range n.Children {
			ce := el
			if n.Op == plan.OpNestedLoop && i == 1 && len(n.Children) > 1 {
				ce = g[n.Children[0].Path].Rows
			}
			if c.Op == plan.OpSubPlan {
				ce = g[n.Path].Rows
			}
			loops(c, ce)
		}
	}
	loops(p.Root, 0)
	return g
}

// StaticCost turns a plan into a symbolic cost: Σ_nodes loops · unit cost,
// with growth exponents for rows and loops (measured or fallback).
func StaticCost(p *plan.Plan, g map[string]NodeGrowth) Expr {
	var e Expr
	if p == nil || p.Root == nil {
		return e
	}
	var rec func(n *plan.Node, driver string)
	rec = func(n *plan.Node, driver string) {
		ng := g[n.Path]
		rp := math.Max(0, RoundExp(ng.Rows)-RoundExp(ng.Loops)) // rows per loop growth
		dom := domRel(n)
		var unit []Term
		switch n.Op {
		case plan.OpSeqScan, plan.OpFullIndexScan:
			e := 1.0
			if ng.HasExam { // a scan cut short by LIMIT examines far fewer than n rows
				e = math.Max(0, RoundExp(ng.Exam)-RoundExp(ng.Loops))
			}
			if e > 0 {
				unit = append(unit, T(map[string]float64{sym(n.Relation): e}, nil))
			}
		case plan.OpIndexRange, plan.OpIndexLookup:
			unit = append(unit, T(nil, map[string]float64{sym(n.Relation): 1}))
			if rp > 0 {
				unit = append(unit, T(map[string]float64{sym(n.Relation): rp}, nil))
			}
		case plan.OpUniqueLookup:
			unit = append(unit, T(nil, map[string]float64{sym(n.Relation): 1}))
		case plan.OpSort:
			in := rp
			if len(n.Children) > 0 {
				cg := g[n.Children[0].Path]
				in = math.Max(0, RoundExp(cg.Rows)-RoundExp(cg.Loops))
			}
			if in > 0 && dom != "" {
				unit = append(unit, T(map[string]float64{sym(dom): in}, map[string]float64{sym(dom): 1}))
			}
		case plan.OpModify:
			rel := n.Relation
			if rel == "" {
				rel = dom
			}
			if rel != "" {
				unit = append(unit, T(nil, map[string]float64{sym(rel): 1}))
			}
		default:
			if rp > 0 && dom != "" {
				unit = append(unit, T(map[string]float64{sym(dom): rp}, nil))
			}
		}
		le := RoundExp(ng.Loops)
		for _, u := range unit {
			if le > 0 && driver != "" {
				u = u.Mul(T(map[string]float64{sym(driver): le}, nil))
			}
			e.Terms = append(e.Terms, u)
		}
		for i, c := range n.Children {
			d := driver
			if n.Op == plan.OpNestedLoop && i == 1 && len(n.Children) > 1 {
				if r := domRel(n.Children[0]); r != "" {
					d = r
				}
			}
			if c.Op == plan.OpSubPlan && dom != "" {
				d = dom
			}
			rec(c, d)
		}
	}
	rec(p.Root, "")
	return e.Simplify()
}
