package runner

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
)

// verifyAdvice proves each CREATE INDEX suggestion with a hypothetical index
// (HypoPG): advice the planner wouldn't use is withdrawn.
func (r *Runner) verifyAdvice(ctx context.Context, results []*analyze.OpResult) {
	for _, res := range results {
		var kept []analyze.Advice
		for _, a := range res.Advice {
			if !strings.HasPrefix(a.SQL, "CREATE INDEX") || a.Query == "" || r.result.AdviceProof == "unavailable" {
				kept = append(kept, a)
				continue
			}
			var q *analyze.QueryResult
			for _, x := range res.Queries {
				if x.ID == a.Query {
					q = x
				}
			}
			if q == nil || q.Base == nil {
				kept = append(kept, a)
				continue
			}
			probe := q.Example
			if a.Rule == "unindexed_fk" { // the planner never plans RI triggers: plan the cascade's own lookup
				fk, ok := r.fkLookup(ctx, a.SQL)
				if !ok {
					kept = append(kept, a)
					continue
				}
				probe = fk
			}
			h, err := r.db.HypoIndex(ctx, probe, a.SQL)
			if errors.Is(err, db.ErrUnsupported) {
				r.result.AdviceProof = "unavailable"
				kept = append(kept, a)
				continue
			}
			if err != nil {
				r.log("    hypopg %s: %v", a.Query, err)
				kept = append(kept, a)
				continue
			}
			r.result.AdviceProof = "hypopg"
			a.CostBefore, a.CostAfter = h.CostBefore, h.CostAfter
			if h.Used && h.CostAfter < 0.9*h.CostBefore {
				a.Verified = fmt.Sprintf("HypoPG: planner cost %s → %s with this index", shortNum(h.CostBefore), shortNum(h.CostAfter))
				if h.ScanBefore != "" && h.ScanAfter != "" && h.ScanBefore != h.ScanAfter {
					a.Verified += fmt.Sprintf(" (%s → %s)", h.ScanBefore, h.ScanAfter)
				}
				kept = append(kept, a)
				continue
			}
			why := "the planner would not use it"
			if h.Used {
				why = "it barely lowers the planner's cost"
			}
			a.Verified = fmt.Sprintf("HypoPG: withdrawn, %s (cost %s → %s)", why, shortNum(h.CostBefore), shortNum(h.CostAfter))
			q.Rejected = append(q.Rejected, a)
		}
		res.Advice = kept
	}
}

var indexOn = regexp.MustCompile(`(?i)\bON\s+(\S+)\s*\(([^)]*)\)`)

// fkLookup builds the query a foreign-key cascade/check runs on the child
// table (SELECT … WHERE fk_cols = value) for the columns of an index advice.
func (r *Runner) fkLookup(ctx context.Context, createIndex string) (analyze.Stmt, bool) {
	m := indexOn.FindStringSubmatch(createIndex)
	if m == nil {
		return analyze.Stmt{}, false
	}
	t := r.tables[strings.ToLower(m[1])]
	if t == nil {
		return analyze.Stmt{}, false
	}
	var conds []string
	for _, c := range strings.Split(m[2], ",") {
		c = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(c), " DESC"))
		v, err := r.db.FirstValues(ctx, fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL LIMIT 1", r.db.Quote(c), t.SQL, r.db.Quote(c)))
		if err != nil || len(v) == 0 {
			return analyze.Stmt{}, false
		}
		conds = append(conds, fmt.Sprintf("%s = '%s'", r.db.Quote(c), strings.ReplaceAll(v[0], "'", "''")))
	}
	return analyze.Stmt{SQL: fmt.Sprintf("SELECT 1 FROM %s WHERE %s", t.SQL, strings.Join(conds, " AND ")), Kind: "select"}, true
}

func shortNum(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	case v >= 100:
		return fmt.Sprintf("%.0f", v)
	}
	return fmt.Sprintf("%.1f", v)
}
