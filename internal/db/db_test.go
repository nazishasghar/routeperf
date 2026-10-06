package db

import "testing"

func fk(child, col, parent string) FK {
	return FK{Child: child, ChildCol: col, ChildCols: []string{col}, Parent: parent, ParentCol: "id", ParentCols: []string{"id"}, Declared: true}
}

func deferredSet(fks []FK) map[string]bool {
	out := map[string]bool{}
	for _, f := range fks {
		if f.Deferred {
			out[f.Child+"."+f.ChildCol] = true
		}
	}
	return out
}

func TestMarkCycles(t *testing.T) {
	tables := map[string]*Table{
		"product_customer": {Key: "product_customer", Rows: 2000, NotNull: map[string]bool{"id": true}},
		"pbp_member":       {Key: "pbp_member", Rows: 6000, NotNull: map[string]bool{"id": true, "product_customer_id": true}},
		"category":         {Key: "category", Rows: 100, NotNull: map[string]bool{"id": true}},
		"claim":            {Key: "claim", Rows: 9000, NotNull: map[string]bool{"id": true, "pbp_member_id": true}},
	}
	fks := MarkCycles(tables, []FK{
		fk("product_customer", "pbp_member_id", "pbp_member"), // nullable: points back
		fk("pbp_member", "product_customer_id", "product_customer"),
		fk("category", "parent_id", "category"),
		fk("claim", "pbp_member_id", "pbp_member"),
		fk("claim", "category_id", "category"),
	})
	got := deferredSet(fks)
	if len(got) != 2 || !got["product_customer.pbp_member_id"] || !got["category.parent_id"] {
		t.Fatalf("deferred = %v, want the nullable side of the cycle and the self-reference", got)
	}
	var scope []*Table
	for _, t := range tables {
		scope = append(scope, t)
	}
	pos := map[string]int{}
	for i, t := range TopoOrder(scope, fks) {
		pos[t.Key] = i
	}
	for _, f := range fks {
		if !f.Deferred && pos[f.Parent] > pos[f.Child] {
			t.Errorf("%s loads before its parent %s", f.Child, f.Parent)
		}
	}

	// both sides nullable: the smaller table loads first, its reference waits
	tables["pbp_member"].NotNull = map[string]bool{"id": true}
	got = deferredSet(MarkCycles(tables, fks[:2]))
	if len(got) != 1 || !got["product_customer.pbp_member_id"] {
		t.Errorf("both nullable: deferred = %v, want product_customer.pbp_member_id", got)
	}
	tables["product_customer"].Rows = 1e6
	got = deferredSet(MarkCycles(tables, fks[:2]))
	if len(got) != 1 || !got["pbp_member.product_customer_id"] {
		t.Errorf("both nullable, members smaller: deferred = %v, want pbp_member.product_customer_id", got)
	}

	// a three-table cycle where every reference is NOT NULL still breaks once
	nn := func(col string) map[string]bool { return map[string]bool{"id": true, col: true} }
	tri := map[string]*Table{"a": {Key: "a", NotNull: nn("b_id")}, "b": {Key: "b", NotNull: nn("c_id")}, "c": {Key: "c", NotNull: nn("a_id")}}
	got = deferredSet(MarkCycles(tri, []FK{fk("a", "b_id", "b"), fk("b", "c_id", "c"), fk("c", "a_id", "a")}))
	if len(got) != 1 {
		t.Errorf("NOT NULL cycle: deferred = %v, want exactly one", got)
	}

	// no cycle: nothing deferred
	got = deferredSet(MarkCycles(tables, []FK{fk("claim", "pbp_member_id", "pbp_member"), fk("pbp_member", "product_customer_id", "product_customer")}))
	if len(got) != 0 {
		t.Errorf("acyclic: deferred = %v", got)
	}
}
