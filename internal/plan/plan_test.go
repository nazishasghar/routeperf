package plan

import "testing"

const tree = `-> Limit: 20 row(s)  (cost=20236 rows=20) (actual time=88.5..88.5 rows=10 loops=1)
    -> Sort: o.created_at DESC, limit input to 20 row(s) per chunk  (cost=20236 rows=199601) (actual time=88.5..88.5 rows=10 loops=1)
        -> Filter: (o.user_id = 5)  (cost=20236 rows=19960) (actual time=0.0447..85.3 rows=10 loops=1)
            -> Table scan on o  (cost=20236 rows=199601) (actual time=0.0428..72.4 rows=200000 loops=1)`

func TestParseMySQLTree(t *testing.T) {
	p, err := ParseMySQLTree(tree, MySQLAliases("SELECT * FROM orders o WHERE o.user_id = 5 ORDER BY o.created_at DESC LIMIT 20"))
	if err != nil {
		t.Fatal(err)
	}
	if p.RowsExamined() != 200000 {
		t.Fatalf("rows examined %v", p.RowsExamined())
	}
	if rel := p.Relations(); len(rel) != 1 || rel[0] != "orders" {
		t.Fatalf("relations %v", rel)
	}
	if p.Root.Children[0].Op != OpTopNSort {
		t.Fatalf("want top-N sort, got %s", p.Root.Children[0].Op)
	}
}

func TestParsePostgresJSONLoopsAndGather(t *testing.T) {
	js := `[{"Plan": {"Node Type": "Gather", "Actual Rows": 10, "Actual Loops": 1, "Plans": [
	  {"Node Type": "Nested Loop", "Actual Rows": 10, "Actual Loops": 1, "Plans": [
	    {"Node Type": "Seq Scan", "Relation Name": "orders", "Actual Rows": 10, "Actual Loops": 1, "Rows Removed by Filter": 199990},
	    {"Node Type": "Index Scan", "Relation Name": "order_items", "Index Name": "oi_idx", "Index Cond": "(order_id = o.id)", "Actual Rows": 3, "Actual Loops": 10}
	  ]}
	]}, "Execution Time": 12.5}]`
	p, err := ParsePostgresJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	if p.Root.Op != OpNestedLoop {
		t.Fatalf("Gather should collapse, root is %s", p.Root.Op)
	}
	if got := p.RowsExamined(); got != 10+199990+30 {
		t.Fatalf("rows examined %v", got)
	}
}
