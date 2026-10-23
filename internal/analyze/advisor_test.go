package analyze

import (
	"testing"

	"github.com/nazishasghar/routeperf/internal/plan"
)

func adviceRules(r *OpResult) map[string]bool {
	got := map[string]bool{}
	for _, a := range r.Advice {
		got[a.Rule] = true
	}
	return got
}

func TestMissingJoinIndexNeedsBigInnerTable(t *testing.T) {
	inner := func(rel string, rows, loops float64) *plan.Plan {
		return &plan.Plan{Root: &plan.Node{
			Op: plan.OpNestedLoop,
			Children: []*plan.Node{
				{Op: plan.OpIndexLookup, Relation: "orders", Rows: 12, Loops: 1},
				{Op: plan.OpSeqScan, Relation: rel, Rows: rows, Loops: loops, Filter: "(order_id = o.id)"},
			},
		}}
	}
	tests := []struct {
		name      string
		plan      *plan.Plan
		tableRows map[string]float64
		want      bool
	}{
		{"tiny lookup table is a fine seq scan", inner("statuses", 96, 12), map[string]float64{"statuses": 8}, false},
		{"big inner table needs an index", inner("items", 120000, 12), map[string]float64{"items": 10000}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &OpResult{Queries: []*QueryResult{{ID: "q1", Kind: "select", SQL: "SELECT 1", Base: tt.plan}}}
			Advise(r, AdviseCtx{Dialect: "postgres", TableRows: tt.tableRows})
			if got := adviceRules(r)["missing_join_index"]; got != tt.want {
				t.Fatalf("missing_join_index = %v, want %v (advice %+v)", got, tt.want, r.Advice)
			}
		})
	}
}

func TestStaleStatsIgnoresEarlyStopUnderLimit(t *testing.T) {
	scan := &plan.Node{Op: plan.OpIndexRange, Relation: "events", Scan: true, EstRows: 50000, Rows: 500, Loops: 1}
	withLimit := &plan.Plan{Root: &plan.Node{Op: plan.OpLimit, Children: []*plan.Node{scan}}}
	r := &OpResult{Queries: []*QueryResult{{ID: "q1", Kind: "select", SQL: "SELECT 1", Base: withLimit}}}
	Advise(r, AdviseCtx{Dialect: "postgres"})
	if adviceRules(r)["stale_stats"] {
		t.Fatalf("LIMIT stopped the scan early, estimate was fine: %+v", r.Advice)
	}
	// without a LIMIT the same gap is a real misestimate
	r = &OpResult{Queries: []*QueryResult{{ID: "q1", Kind: "select", SQL: "SELECT 1", Base: &plan.Plan{Root: scan}}}}
	Advise(r, AdviseCtx{Dialect: "postgres"})
	if !adviceRules(r)["stale_stats"] {
		t.Fatal("misestimate without LIMIT should still be reported")
	}
}

func TestUnindexedFKOnUpdateNeedsKeyChange(t *testing.T) {
	upd := func(sql string) *OpResult {
		p := &plan.Plan{Root: &plan.Node{Op: plan.OpUniqueLookup, Relation: "orders", Rows: 1, Loops: 1}}
		return &OpResult{Queries: []*QueryResult{{ID: "q1", Kind: "update", SQL: sql, Base: p}}}
	}
	ac := AdviseCtx{Dialect: "postgres", UnindexedFK: map[string][]string{"orders": {"order_items|order_id|id"}}}
	tests := []struct {
		name string
		sql  string
		want bool
	}{
		{"non-key column", "UPDATE orders SET status = 'paid' WHERE id = 1", false},
		{"key column", "UPDATE orders SET id = 2 WHERE id = 1", true},
		{"quoted key column", `UPDATE orders SET "id" = 2 WHERE id = 1`, true},
		{"unparseable keeps advice", "UPDATE orders", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := upd(tt.sql)
			Advise(r, ac)
			if got := adviceRules(r)["unindexed_fk"]; got != tt.want {
				t.Fatalf("unindexed_fk = %v, want %v", got, tt.want)
			}
		})
	}
	// DELETE always checks the child table
	r := upd("DELETE FROM orders WHERE id = 1")
	r.Queries[0].Kind = "delete"
	Advise(r, ac)
	if !adviceRules(r)["unindexed_fk"] {
		t.Fatal("DELETE on a parent must report the unindexed FK")
	}
}
