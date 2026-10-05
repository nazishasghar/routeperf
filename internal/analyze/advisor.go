package analyze

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/nazishasghar/routeperf/internal/plan"
)

var colRe = regexp.MustCompile("(?i)(?:\\b(\\w+)`?\\.)?[\"`]?(\\w+)[\"`]?\\s*(?:=|<>|<=|>=|<|>|\\bIN\\b|\\bLIKE\\b|~~|\\bBETWEEN\\b|= ANY)")

func filterCols(f string) []string {
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

// Advise applies rule-based checks to a route's queries.
func Advise(r *OpResult, tableRows map[string]float64, unindexedFK map[string][]string, cols map[string]map[string]bool) {
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
		q.Base.Walk(func(n *plan.Node, _ int) {
			if n.Op == plan.OpFilter {
				for _, c := range n.Children {
					parentFilter[c] = n.Filter
				}
			}
		})
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
				// MySQL puts the filter in a separate node above the scan.
				if len(cols) > 0 && (n.RowsRemoved > 0.9*examined || out < 0.1*examined || parentFilter[n] != "") {
					idx := cols
					if sortKey != "" {
						for _, s := range sortCols(sortKey) {
							if !contains(idx, strings.TrimSuffix(s, " DESC")) && isCol(n.Relation, s) {
								idx = append(idx, s)
							}
						}
					}
					add(Advice{Rule: "unindexed_filter", Query: q.ID,
						Message:  fmt.Sprintf("Seq/table scan on %s (%.0f rows examined) filtered by %s", n.Relation, examined, strings.Join(cols, ", ")),
						SQL:      fmt.Sprintf("CREATE INDEX ON %s (%s);", n.Relation, strings.Join(idx, ", ")),
						Expected: fmt.Sprintf("O(log n_%s + k)", n.Relation)})
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
						add(Advice{Rule: "missing_join_index", Query: q.ID,
							Message: fmt.Sprintf("Nested loop scans %s fully %.0f times", in.Relation, in.Loops),
							SQL:     fmt.Sprintf("CREATE INDEX ON %s (<join column>);", in.Relation)})
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
					add(Advice{Rule: "stale_stats", Query: q.ID, Message: fmt.Sprintf("Row estimate %.0f vs actual %.0f on %s", n.EstRows, act, firstNonEmptyS(n.Relation, n.RawType)), SQL: "ANALYZE " + n.Relation + ";"})
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
					add(Advice{Rule: "unindexed_fk", Query: q.ID, Message: fmt.Sprintf("FK %s has no index; every %s on %s scans %s", child, strings.ToUpper(q.Kind), rel, strings.SplitN(child, ".", 2)[0]),
						SQL: fmt.Sprintf("CREATE INDEX ON %s (%s);", strings.SplitN(child, ".", 2)[0], strings.SplitN(child+".", ".", 3)[1])})
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
