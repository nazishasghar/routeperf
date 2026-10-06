// Package plan holds a dialect-neutral query plan tree and parsers for
// Postgres EXPLAIN (FORMAT JSON) and MySQL EXPLAIN ANALYZE (TREE) output.
package plan

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type Op string

const (
	OpSeqScan       Op = "SeqScan"
	OpFullIndexScan Op = "FullIndexScan"
	OpIndexRange    Op = "IndexRange"
	OpIndexLookup   Op = "IndexLookup"
	OpUniqueLookup  Op = "UniqueLookup"
	OpBitmapHeap    Op = "BitmapHeap"
	OpSort          Op = "Sort"
	OpTopNSort      Op = "TopNSort"
	OpHashJoin      Op = "HashJoin"
	OpHash          Op = "Hash"
	OpNestedLoop    Op = "NestedLoop"
	OpMergeJoin     Op = "MergeJoin"
	OpAggregate     Op = "Aggregate"
	OpLimit         Op = "Limit"
	OpSubPlan       Op = "CorrelatedSubPlan"
	OpMaterialize   Op = "Materialize"
	OpMemoize       Op = "Memoize"
	OpModify        Op = "Modify"
	OpFilter        Op = "Filter"
	OpOther         Op = "Other"
)

type Node struct {
	Op          Op      `json:"op"`
	RawType     string  `json:"raw"`
	Relation    string  `json:"relation,omitempty"`
	Schema      string  `json:"-"`
	Alias       string  `json:"-"`
	Index       string  `json:"index,omitempty"`
	Rows        float64 `json:"rows"`         // total rows out, all loops
	Loops       float64 `json:"loops"`        // executions
	RowsRemoved float64 `json:"rows_removed"` // total, all loops
	EstRows     float64 `json:"est_rows"`     // planner estimate per loop
	Pages       float64 `json:"pages,omitempty"`
	Reads       float64 `json:"reads,omitempty"` // pages read from outside shared buffers (inclusive)
	TimeMs      float64 `json:"time_ms"`         // inclusive, all loops
	Blocking    bool    `json:"blocking,omitempty"`
	Scan        bool    `json:"scan,omitempty"` // reads table/index rows
	Filter      string  `json:"filter,omitempty"`
	IndexCond   string  `json:"index_cond,omitempty"`
	SortKey     string  `json:"sort_key,omitempty"`
	SortMethod  string  `json:"sort_method,omitempty"`
	Path        string  `json:"path"`
	Children    []*Node `json:"children,omitempty"`
}

type Trigger struct {
	Name     string  `json:"name"`
	Relation string  `json:"relation"`
	Calls    float64 `json:"calls"`
	TimeMs   float64 `json:"time_ms"`
}

type Plan struct {
	Dialect     string    `json:"dialect"`
	Root        *Node     `json:"root"`
	PlanningMs  float64   `json:"planning_ms"`
	ExecutionMs float64   `json:"execution_ms"`
	Triggers    []Trigger `json:"triggers,omitempty"`
	Text        string    `json:"text,omitempty"` // human-readable tree
	// Handler-based rows examined (MySQL fallback for DML); 0 = use plan.
	HandlerRows float64 `json:"handler_rows,omitempty"`
}

func (p *Plan) Walk(fn func(n *Node, depth int)) {
	var rec func(n *Node, d int)
	rec = func(n *Node, d int) {
		if n == nil {
			return
		}
		fn(n, d)
		for _, c := range n.Children {
			rec(c, d+1)
		}
	}
	rec(p.Root, 0)
}

// RowsExamined = Σ over scan nodes of (rows out + rows removed), all loops.
func (p *Plan) RowsExamined() float64 {
	if p.HandlerRows > 0 {
		return p.HandlerRows
	}
	var t float64
	p.Walk(func(n *Node, _ int) {
		if n.Scan {
			t += n.Rows + n.RowsRemoved
		}
	})
	return t
}

func (p *Plan) TotalMs() float64 {
	if p.ExecutionMs > 0 {
		return p.ExecutionMs
	}
	if p.Root != nil {
		return p.Root.TimeMs
	}
	return 0
}

// opClass groups ops with the same complexity so access-path variants
// (index vs bitmap scan, sort vs incremental sort) don't count as plan flips.
func opClass(op Op) string {
	switch op {
	case OpSeqScan, OpFullIndexScan:
		return "full"
	case OpIndexRange, OpIndexLookup, OpUniqueLookup, OpBitmapHeap:
		return "index"
	case OpSort, OpTopNSort:
		return "sort"
	case OpNestedLoop:
		return "nl"
	case OpHashJoin, OpMergeJoin:
		return "join"
	case OpSubPlan:
		return "subplan"
	case OpModify:
		return "modify"
	}
	return ""
}

// ShapeHash identifies the plan's algorithmic shape.
func (p *Plan) ShapeHash() string {
	var b strings.Builder
	p.Walk(func(n *Node, d int) {
		if c := opClass(n.Op); c != "" && !(n.Op == OpIndexRange && n.Relation == "") {
			fmt.Fprintf(&b, "%s:%s;", c, n.Relation)
		}
	})
	h := sha1.Sum([]byte(b.String()))
	return hex.EncodeToString(h[:6])
}

func (p *Plan) Relations() []string {
	seen := map[string]bool{}
	var out []string
	p.Walk(func(n *Node, _ int) {
		if n.Relation != "" && !seen[n.Relation] && !strings.HasPrefix(n.Relation, "<") {
			seen[n.Relation] = true
			out = append(out, n.Relation)
		}
	})
	return out
}

// assignPaths gives each node a key that matches the "same" node across
// plans of different data sizes: op class + relation + occurrence.
func assignPaths(root *Node) {
	seen := map[string]int{}
	var rec func(n *Node)
	rec = func(n *Node) {
		c := opClass(n.Op)
		if c == "" {
			c = string(n.Op)
		}
		k := c + "@" + n.Relation
		n.Path = fmt.Sprintf("%s#%d", k, seen[k])
		seen[k]++
		for _, ch := range n.Children {
			rec(ch)
		}
	}
	rec(root)
}

func renderText(n *Node, d int, b *strings.Builder) {
	fmt.Fprintf(b, "%s-> %s", strings.Repeat("   ", d), n.RawType)
	if n.Relation != "" {
		fmt.Fprintf(b, " on %s", n.Relation)
	}
	if n.Index != "" {
		fmt.Fprintf(b, " using %s", n.Index)
	}
	fmt.Fprintf(b, "  (rows=%.0f loops=%.0f", n.Rows, n.Loops)
	if n.RowsRemoved > 0 {
		fmt.Fprintf(b, " removed=%.0f", n.RowsRemoved)
	}
	fmt.Fprintf(b, " time=%.3fms)", n.TimeMs)
	if n.Filter != "" {
		fmt.Fprintf(b, " filter: %s", n.Filter)
	}
	if n.IndexCond != "" {
		fmt.Fprintf(b, " cond: %s", n.IndexCond)
	}
	if n.SortKey != "" {
		fmt.Fprintf(b, " key: %s", n.SortKey)
	}
	b.WriteString("\n")
	for _, c := range n.Children {
		renderText(c, d+1, b)
	}
}

// Reindex reassigns node paths after relation names change (partitions
// and namespace copies mapped back to their table).
func Reindex(p *Plan) {
	if p.Root != nil {
		assignPaths(p.Root)
	}
}

// Finish assigns node paths and renders the text tree.
func Finish(p *Plan) *Plan { return finish(p) }

func finish(p *Plan) *Plan {
	if p.Root != nil {
		assignPaths(p.Root)
		var b strings.Builder
		renderText(p.Root, 0, &b)
		p.Text = b.String()
	}
	return p
}

// ---------------- Postgres ----------------

func ParsePostgresJSON(data []byte) (*Plan, error) {
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return nil, fmt.Errorf("pg explain json: %w", err)
	}
	if len(arr) == 0 {
		return nil, fmt.Errorf("pg explain json: empty")
	}
	top := arr[0]
	p := &Plan{Dialect: "postgres", PlanningMs: num(top["Planning Time"]), ExecutionMs: num(top["Execution Time"])}
	if pm, ok := top["Plan"].(map[string]any); ok {
		p.Root = pgNode(pm)
	}
	if trs, ok := top["Triggers"].([]any); ok {
		for _, t := range trs {
			if m, ok := t.(map[string]any); ok {
				p.Triggers = append(p.Triggers, Trigger{Name: str(m["Trigger Name"]), Relation: str(m["Relation"]), Calls: num(m["Calls"]), TimeMs: num(m["Time"])})
			}
		}
	}
	return finish(p), nil
}

var rangeOps = regexp.MustCompile(`[<>]|BETWEEN|~~|LIKE|ANY`)

func pgNode(m map[string]any) *Node {
	typ := str(m["Node Type"])
	loops := num(m["Actual Loops"])
	n := &Node{
		RawType:     typ,
		Relation:    str(m["Relation Name"]),
		Schema:      str(m["Schema"]),
		Alias:       str(m["Alias"]),
		Index:       str(m["Index Name"]),
		Loops:       loops,
		Rows:        num(m["Actual Rows"]) * loops,
		RowsRemoved: (num(m["Rows Removed by Filter"]) + num(m["Rows Removed by Index Recheck"]) + num(m["Rows Removed by Join Filter"])) * loops,
		EstRows:     num(m["Plan Rows"]),
		Pages:       num(m["Shared Hit Blocks"]) + num(m["Shared Read Blocks"]),
		Reads:       num(m["Shared Read Blocks"]) + num(m["Local Read Blocks"]),
		TimeMs:      num(m["Actual Total Time"]) * loops,
		Filter:      str(m["Filter"]),
		IndexCond:   firstNonEmpty(str(m["Index Cond"]), str(m["Recheck Cond"]), str(m["Hash Cond"]), str(m["Join Filter"])),
		SortMethod:  str(m["Sort Method"]),
	}
	if sk, ok := m["Sort Key"].([]any); ok {
		var ks []string
		for _, k := range sk {
			ks = append(ks, str(k))
		}
		n.SortKey = strings.Join(ks, ", ")
	}
	switch typ {
	case "Seq Scan", "Parallel Seq Scan", "Sample Scan":
		n.Op, n.Scan = OpSeqScan, true
	case "Index Scan", "Index Only Scan":
		n.Scan = true
		cond := str(m["Index Cond"])
		switch {
		case cond == "":
			n.Op = OpFullIndexScan
		case rangeOps.MatchString(cond):
			n.Op = OpIndexRange
		default:
			n.Op = OpIndexLookup
		}
	case "Bitmap Index Scan":
		n.Op = OpIndexRange
		if cond := str(m["Index Cond"]); cond != "" && !rangeOps.MatchString(cond) {
			n.Op = OpIndexLookup // equality: bounded matches per lookup
		}
	case "Bitmap Heap Scan":
		n.Op, n.Scan = OpBitmapHeap, true
	case "Tid Scan", "Tid Range Scan":
		n.Op, n.Scan = OpUniqueLookup, true
	case "Sort", "Incremental Sort":
		n.Op, n.Blocking = OpSort, true
		if strings.Contains(n.SortMethod, "top-N") {
			n.Op = OpTopNSort
		}
	case "Hash":
		n.Op, n.Blocking = OpHash, true
	case "Hash Join":
		n.Op = OpHashJoin
	case "Merge Join":
		n.Op = OpMergeJoin
	case "Nested Loop":
		n.Op = OpNestedLoop
	case "Aggregate", "Group", "WindowAgg", "GroupAggregate", "HashAggregate":
		n.Op = OpAggregate
		n.Blocking = str(m["Strategy"]) != "Sorted"
	case "Limit":
		n.Op = OpLimit
	case "Materialize":
		n.Op = OpMaterialize
	case "Memoize":
		n.Op = OpMemoize
	case "ModifyTable":
		n.Op = OpModify
		n.RawType = "ModifyTable " + str(m["Operation"])
	default:
		n.Op = OpOther
	}
	if (typ == "Gather" || typ == "Gather Merge") && len(n.Children) == 0 {
		if kids, ok := m["Plans"].([]any); ok && len(kids) == 1 {
			if km, ok := kids[0].(map[string]any); ok {
				return pgNode(km)
			}
		}
	}
	if str(m["Parent Relationship"]) == "SubPlan" {
		// Correlated subplan: executed per outer row; mark via wrapper op.
		w := &Node{Op: OpSubPlan, RawType: str(m["Subplan Name"]), Loops: n.Loops, Rows: n.Rows, TimeMs: n.TimeMs, Children: []*Node{n}}
		if kids, ok := m["Plans"].([]any); ok {
			for _, k := range kids {
				if km, ok := k.(map[string]any); ok {
					n.Children = append(n.Children, pgNode(km))
				}
			}
		}
		return w
	}
	if kids, ok := m["Plans"].([]any); ok {
		for _, k := range kids {
			if km, ok := k.(map[string]any); ok {
				n.Children = append(n.Children, pgNode(km))
			}
		}
	}
	if typ == "Bitmap Heap Scan" { // bitmap index scans read the heap scan's table
		var mark func(c *Node)
		mark = func(c *Node) {
			if c.Relation == "" && (c.Op == OpIndexRange || c.Op == OpIndexLookup) {
				c.Relation, c.Schema = n.Relation, n.Schema
			}
			for _, g := range c.Children {
				mark(g)
			}
		}
		for _, c := range n.Children {
			mark(c)
		}
	}
	return n
}

// ---------------- MySQL TREE ----------------

var (
	myActual = regexp.MustCompile(`\(actual time=([\d.e+-]+)\.\.([\d.e+-]+) rows=([\d.e+-]+) loops=([\d.e+-]+)\)`)
	myCost   = regexp.MustCompile(`\(cost=[^)]*rows=([\d.e+-]+)\)`)
	myOn     = regexp.MustCompile(`\bon (<[^>]+>|\S+)(?: using (\S+))?`)
	myLimitN = regexp.MustCompile(`limit input to (\d+) row`)
)

// ParseMySQLTree parses EXPLAIN ANALYZE (FORMAT=TREE) output. aliases maps
// SQL aliases to table names so "Table scan on o" resolves to "orders".
func ParseMySQLTree(text string, aliases map[string]string) (*Plan, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r", ""), "\n")
	type item struct {
		n     *Node
		depth int
	}
	var stack []item
	var root *Node
	for _, ln := range lines {
		idx := strings.Index(ln, "-> ")
		if idx < 0 {
			continue
		}
		depth := idx / 4
		body := ln[idx+3:]
		n := myNode(body, aliases)
		for len(stack) > 0 && stack[len(stack)-1].depth >= depth {
			stack = stack[:len(stack)-1]
		}
		if len(stack) == 0 {
			if root == nil {
				root = n
			} else {
				root.Children = append(root.Children, n)
			}
		} else {
			p := stack[len(stack)-1].n
			p.Children = append(p.Children, n)
		}
		stack = append(stack, item{n, depth})
	}
	if root == nil {
		return nil, fmt.Errorf("mysql tree: no nodes")
	}
	p := &Plan{Dialect: "mysql", Root: root, ExecutionMs: root.TimeMs / max1(root.Loops)}
	return finish(p), nil
}

func max1(v float64) float64 {
	if v < 1 {
		return 1
	}
	return v
}

func myNode(body string, aliases map[string]string) *Node {
	desc := body
	if i := strings.Index(body, "  ("); i >= 0 {
		desc = body[:i]
	} else if i := strings.Index(body, " (cost="); i >= 0 {
		desc = body[:i]
	} else if i := strings.Index(body, " (actual"); i >= 0 {
		desc = body[:i]
	}
	n := &Node{RawType: desc}
	if m := myActual.FindStringSubmatch(body); m != nil {
		last, _ := strconv.ParseFloat(m[2], 64)
		rows, _ := strconv.ParseFloat(m[3], 64)
		loops, _ := strconv.ParseFloat(m[4], 64)
		n.Loops, n.Rows, n.TimeMs = loops, rows*loops, last*loops
	}
	if m := myCost.FindStringSubmatch(body); m != nil {
		n.EstRows, _ = strconv.ParseFloat(m[1], 64)
	}
	resolve := func() {
		if m := myOn.FindStringSubmatch(desc); m != nil {
			rel := strings.Trim(m[1], "`")
			n.Alias = rel
			if t, ok := aliases[strings.ToLower(rel)]; ok {
				rel = t
			}
			n.Relation, n.Index = rel, strings.Trim(m[2], "`")
		}
	}
	d := strings.ToLower(desc)
	switch {
	case strings.HasPrefix(d, "insert into ") || strings.HasPrefix(d, "update ") || strings.HasPrefix(d, "delete from "):
		n.Op = OpModify
		f := strings.Fields(desc)
		rel := f[len(f)-1]
		if strings.HasPrefix(d, "insert into ") || strings.HasPrefix(d, "delete from ") {
			rel = f[2]
		} else {
			rel = f[1]
		}
		rel = strings.Trim(strings.TrimSuffix(rel, ","), "`")
		if t, ok := aliases[strings.ToLower(rel)]; ok {
			rel = t
		}
		n.Relation = rel
	case strings.HasPrefix(d, "table scan on <"):
		n.Op = OpOther
		resolve()
	case strings.HasPrefix(d, "table scan on"):
		n.Op, n.Scan = OpSeqScan, true
		resolve()
	case strings.Contains(d, "index range scan"), strings.Contains(d, "index skip scan"):
		n.Op, n.Scan = OpIndexRange, true
		resolve()
	case strings.HasPrefix(d, "single-row") || strings.HasPrefix(d, "constant row"):
		n.Op, n.Scan = OpUniqueLookup, true
		resolve()
	case strings.Contains(d, "index lookup on"):
		n.Op, n.Scan = OpIndexLookup, true
		resolve()
		if i := strings.Index(desc, "("); i >= 0 {
			n.IndexCond = desc[i:]
		}
	case strings.Contains(d, "index scan on"):
		n.Op, n.Scan = OpFullIndexScan, true
		resolve()
	case strings.HasPrefix(d, "sort"):
		n.Op, n.Blocking = OpSort, true
		n.SortKey = strings.TrimSpace(strings.TrimPrefix(desc, "Sort:"))
		if j := strings.Index(n.SortKey, ", limit input"); j >= 0 {
			n.SortKey = n.SortKey[:j]
		}
		if myLimitN.MatchString(d) {
			n.Op = OpTopNSort
		}
	case strings.HasPrefix(d, "limit"):
		n.Op = OpLimit
	case strings.HasPrefix(d, "filter"):
		n.Op = OpFilter
		n.Filter = strings.TrimSpace(strings.TrimPrefix(desc, "Filter:"))
	case strings.Contains(d, "hash join") || strings.HasPrefix(d, "hash semijoin") || strings.HasPrefix(d, "hash antijoin"):
		n.Op = OpHashJoin
	case strings.HasPrefix(d, "nested loop"):
		n.Op = OpNestedLoop
	case strings.Contains(d, "aggregate"), strings.HasPrefix(d, "group"):
		n.Op, n.Blocking = OpAggregate, true
	case strings.HasPrefix(d, "materialize"):
		n.Op, n.Blocking = OpMaterialize, true
	case strings.HasPrefix(d, "select #") && strings.Contains(d, "dependent"):
		n.Op = OpSubPlan
	default:
		n.Op = OpOther
	}
	return n
}

// MySQLAliases extracts "FROM t a" / "JOIN t AS a" alias → table mappings.
var aliasRe = regexp.MustCompile("(?i)\\b(?:from|join|update|into)\\s+`?(\\w+)`?(?:\\.`?(\\w+)`?)?(?:\\s+(?:as\\s+)?`?(\\w+)`?)?")

var sqlKeywords = map[string]bool{"where": true, "join": true, "inner": true, "left": true, "right": true, "on": true, "order": true, "group": true, "limit": true, "set": true, "values": true, "select": true, "union": true, "having": true, "for": true, "using": true, "cross": true, "natural": true, "straight_join": true, "outer": true, "offset": true, "window": true, "partition": true}

func MySQLAliases(sql string) map[string]string {
	out := map[string]string{}
	for _, m := range aliasRe.FindAllStringSubmatch(sql, -1) {
		table := m[1]
		if m[2] != "" {
			table = m[2]
		}
		out[strings.ToLower(table)] = table
		if a := m[3]; a != "" && !sqlKeywords[strings.ToLower(a)] {
			out[strings.ToLower(a)] = table
		}
	}
	return out
}

func num(v any) float64 {
	switch x := v.(type) {
	case float64:
		return x
	case json.Number:
		f, _ := x.Float64()
		return f
	case int:
		return float64(x)
	}
	return 0
}

func str(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

func firstNonEmpty(s ...string) string {
	for _, x := range s {
		if x != "" {
			return x
		}
	}
	return ""
}
