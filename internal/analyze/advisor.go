package analyze

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nazishasghar/routeperf/internal/plan"
)

var colRe = regexp.MustCompile("(?i)(?:\\b(\\w+)`?\\.)?[\"`]?(\\w+)[\"`]?\\s*(?:=|<>|<=|>=|<|>|\\bIN\\b|\\bLIKE\\b|~~|\\bBETWEEN\\b|= ANY)")

var (
	castedIdent = regexp.MustCompile(`\(([\w.]+)\)::[\w ]+(?:\[\])?`) // (status)::text → status
	castSuffix  = regexp.MustCompile(`::[a-z_][\w ]*(?:\[\])?`)       // 'paid'::text → 'paid'
)

func filterCols(f string) []string {
	f = castedIdent.ReplaceAllString(f, "$1")
	f = castSuffix.ReplaceAllString(f, "")
	seen := map[string]bool{}
	var out []string
	for _, m := range colRe.FindAllStringSubmatch(f, -1) {
		c := strings.ToLower(m[2])
		if c == "" || seen[c] || isNumber(c) || c == "and" || c == "or" || c == "not" {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	return out
}

func isNumber(s string) bool {
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return s != ""
}

func sortCols(k string) []string {
	var out []string
	for _, part := range strings.Split(k, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		f := strings.Fields(part)
		col := f[0]
		if i := strings.LastIndex(col, "."); i >= 0 {
			col = col[i+1:]
		}
		col = strings.Trim(col, "`\"()")
		if len(f) > 1 && strings.EqualFold(f[1], "DESC") {
			col += " DESC"
		}
		out = append(out, col)
	}
	return out
}

// AdviseCtx carries catalog facts the rules need.
type AdviseCtx struct {
	Dialect     string                         // postgres | mysql
	TableRows   map[string]float64             // table → rows
	UnindexedFK map[string][]string            // parent → ["child.col"]
	Cols        map[string]map[string]bool     // table → columns
	Indexes     map[string]map[string][]string // table → index name → ordered columns
}

var nonIdent = regexp.MustCompile(`[^a-z0-9_]+`)

// IndexName builds a deterministic, length-safe index name (PG 63, MySQL 64).
func IndexName(table string, cols []string) string {
	var parts []string
	for _, c := range cols {
		parts = append(parts, strings.TrimSuffix(strings.ToLower(c), " desc"))
	}
	n := nonIdent.ReplaceAllString("idx_"+strings.ToLower(table)+"_"+strings.Join(parts, "_"), "_")
	if len(n) > 60 {
		n = n[:60]
	}
	return strings.TrimRight(n, "_")
}

func (a AdviseCtx) createIndex(table string, cols []string) string {
	return fmt.Sprintf("CREATE INDEX %s ON %s (%s);", IndexName(table, cols), table, strings.Join(cols, ", "))
}

func (a AdviseCtx) analyzeSQL(table string) string {
	if a.Dialect == "mysql" {
		return "ANALYZE TABLE " + table + ";"
	}
	return "ANALYZE " + table + ";"
}

// covering returns an existing index whose leading columns are exactly cols.
func (a AdviseCtx) covering(table string, cols []string) string {
	for name, ic := range a.Indexes[strings.ToLower(table)] {
		if len(ic) < len(cols) {
			continue
		}
		ok := true
		for i, c := range cols {
			if ic[i] != strings.ToLower(strings.TrimSuffix(c, " DESC")) {
				ok = false
				break
			}
		}
		if ok {
			return name
		}
	}
	return ""
}

// leading returns an existing index that starts with col (but covers less).
func (a AdviseCtx) leading(table, col string) (string, []string) {
	for name, ic := range a.Indexes[strings.ToLower(table)] {
		if len(ic) > 0 && ic[0] == strings.ToLower(strings.TrimSuffix(col, " DESC")) {
			return name, ic
		}
	}
	return "", nil
}

// Advise applies rule-based checks to a route's queries.
func Advise(r *OpResult, ac AdviseCtx) {
	tableRows, unindexedFK, cols := ac.TableRows, ac.UnindexedFK, ac.Cols
	isCol := func(t, c string) bool {
		return cols == nil || cols[strings.ToLower(t)][strings.ToLower(strings.TrimSuffix(c, " DESC"))]
	}
	add := func(a Advice) {
		for _, x := range r.Advice {
			if x.Rule == a.Rule && x.SQL == a.SQL && x.Query == a.Query {
				return
			}
		}
		r.Advice = append(r.Advice, a)
	}
	for _, q := range r.Queries {
		if q.Base == nil {
			continue
		}
		// find the sort key above a scan (for composite index suggestion)
		var sortKey string
		q.Base.Walk(func(n *plan.Node, _ int) {
			if (n.Op == plan.OpSort || n.Op == plan.OpTopNSort) && sortKey == "" {
				sortKey = n.SortKey
			}
		})
		var parentFilter = map[*plan.Node]string{}
		var filterNode = map[*plan.Node]*plan.Node{}
		hasLimit := false
		q.Base.Walk(func(n *plan.Node, _ int) {
			if n.Op == plan.OpFilter {
				for _, c := range n.Children {
					parentFilter[c] = n.Filter
					filterNode[c] = n
				}
			}
			hasLimit = hasLimit || n.Op == plan.OpLimit
		})
		topN := hasLimit && sortKey != "" // ORDER BY … LIMIT: an index on (filter, sort) avoids the scan
		q.Base.Walk(func(n *plan.Node, _ int) {
			examined := n.Rows + n.RowsRemoved
			switch n.Op {
			case plan.OpSeqScan:
				f := n.Filter
				if f == "" {
					f = parentFilter[n]
				}
				big := tableRows[n.Relation] >= 10000 || examined >= 10000
				if !big {
					return
				}
				var cols []string
				for _, c := range filterCols(f) {
					if isCol(n.Relation, c) {
						cols = append(cols, c)
					}
				}
				out := n.Rows
				if fn := filterNode[n]; fn != nil { // MySQL: the filter is a separate node above the scan
					out = fn.Rows
				}
				if len(cols) > 0 && (out <= 0.5*examined || topN) {
					idx := cols
					if sortKey != "" {
						for _, s := range sortCols(sortKey) {
							if !contains(idx, strings.TrimSuffix(s, " DESC")) && isCol(n.Relation, s) {
								idx = append(idx, s)
							}
						}
					}
					switch name, lead := ac.covering(n.Relation, cols), ""; {
					case name != "":
						add(Advice{Rule: "index_not_used", Query: q.ID,
							Message: fmt.Sprintf("Index %s on %s (%s) exists but the planner scanned the whole table (%.0f rows): statistics may be stale or the filter isn't selective", name, n.Relation, strings.Join(cols, ", "), examined),
							SQL:     ac.analyzeSQL(n.Relation)})
					default:
						if ln, lc := ac.leading(n.Relation, idx[0]); ln != "" && len(lc) < len(idx) {
							lead = ln
						}
						msg := fmt.Sprintf("Seq/table scan on %s (%.0f rows examined) filtered by %s", n.Relation, examined, strings.Join(cols, ", "))
						if lead != "" {
							msg += fmt.Sprintf("; existing index %s covers only part of it, replace it with", lead)
						}
						add(Advice{Rule: "unindexed_filter", Query: q.ID, Message: msg,
							SQL: ac.createIndex(n.Relation, idx), Expected: fmt.Sprintf("O(log n_%s + k)", n.Relation)})
					}
				} else if len(cols) == 0 && q.Kind == "select" {
					add(Advice{Rule: "full_scan", Query: q.ID,
						Message: fmt.Sprintf("Full scan of %s (%.0f rows) with no filter — inherent O(n); consider pagination, pre-aggregation or a summary table", n.Relation, examined)})
				}
			case plan.OpSort:
				if examined >= 10000 || strings.Contains(n.SortMethod, "external") {
					add(Advice{Rule: "sort_not_indexed", Query: q.ID,
						Message:  fmt.Sprintf("Sort over %.0f rows (%s)%s", n.Rows, n.SortKey, ifs(strings.Contains(n.SortMethod, "external"), " spilled to disk", "")),
						Expected: "index matching WHERE + ORDER BY removes the sort"})
				}
			case plan.OpNestedLoop:
				if len(n.Children) > 1 {
					in := n.Children[1]
					if in.Op == plan.OpFilter && len(in.Children) > 0 {
						in = in.Children[0]
					}
					if in.Op == plan.OpSeqScan && in.Loops > 10 {
						var jc []string
						for _, c := range filterCols(firstNonEmptyS(in.Filter, firstNonEmptyS(n.Filter, n.IndexCond))) {
							if isCol(in.Relation, c) {
								jc = append(jc, c)
							}
						}
						sql := fmt.Sprintf("-- add an index on the join column of %s", in.Relation)
						if len(jc) > 0 && ac.covering(in.Relation, jc[:1]) == "" {
							sql = ac.createIndex(in.Relation, jc[:1])
						}
						add(Advice{Rule: "missing_join_index", Query: q.ID,
							Message: fmt.Sprintf("Nested loop scans %s fully %.0f times", in.Relation, in.Loops), SQL: sql})
					}
				}
			case plan.OpSubPlan:
				if n.Loops > 10 {
					add(Advice{Rule: "correlated_subquery", Query: q.ID, Message: fmt.Sprintf("Correlated subquery executed %.0f times; rewrite as JOIN/LATERAL", n.Loops)})
				}
			}
			if n.Scan && n.Relation != "" && n.EstRows > 0 && n.Loops > 0 {
				act := n.Rows / n.Loops
				if act > 100 && (act/n.EstRows > 10 || n.EstRows/act > 10) {
					add(Advice{Rule: "stale_stats", Query: q.ID, Message: fmt.Sprintf("Row estimate %.0f vs actual %.0f on %s", n.EstRows, act, firstNonEmptyS(n.Relation, n.RawType)), SQL: ac.analyzeSQL(n.Relation)})
				}
			}
		})
		if q.PlanFlip != "" {
			add(Advice{Rule: "plan_flip", Query: q.ID, Message: q.PlanFlip})
		}
		if strings.Contains(strings.ToLower(q.SQL), " offset ") {
			add(Advice{Rule: "deep_offset", Query: q.ID, Message: "OFFSET pagination cost grows with page number; use keyset pagination (WHERE id > last_id)"})
		}
		if q.Kind == "delete" || q.Kind == "update" {
			for _, rel := range q.Base.Relations() {
				for _, child := range unindexedFK[rel] {
					ct, cc, _ := strings.Cut(child, ".")
					add(Advice{Rule: "unindexed_fk", Query: q.ID, Message: fmt.Sprintf("FK %s has no index; every %s on %s scans %s", child, strings.ToUpper(q.Kind), rel, ct),
						SQL: ac.createIndex(ct, []string{cc})})
				}
			}
		}
	}
	for _, fp := range r.NPlusOne {
		kind := "SELECT"
		for _, q := range r.Queries {
			if q.ID == fp {
				kind = strings.ToUpper(q.Kind)
			}
		}
		msg := "Query runs once per returned item (N+1); batch with JOIN / IN (...) / = ANY($1)"
		if kind == "INSERT" {
			msg = "INSERT runs once per input item; use a multi-row INSERT / COPY"
		}
		add(Advice{Rule: "n_plus_one", Query: fp, Message: msg, Expected: "queries/request constant"})
	}
	for _, fp := range r.Redundant {
		add(Advice{Rule: "redundant_query", Query: fp, Message: "Identical query repeated within one request; cache per request"})
	}
	if r.AppKExp >= 1.6 {
		add(Advice{Rule: "app_superlinear", Message: fmt.Sprintf("App time (excluding DB) grows ~k^%.1f; inspect in-memory loops/sorting", r.AppKExp)})
	}
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

func ifs(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func firstNonEmptyS(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
