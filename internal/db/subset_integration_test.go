//go:build integration

// Subset builds against a real server, for schemas whose foreign keys form
// cycles (mutual references, self-references).
//
//	RP_IT_PG=postgres://localhost:5432/routeperf_it \
//	RP_IT_MYSQL=mysql://root@127.0.0.1:3306/routeperf_it \
//	go test -tags integration -run Cycle ./internal/db/
package db

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
)

const cycleDB = "routeperf_it_fkcycle"

func execAll(t *testing.T, d DB, stmts ...string) {
	t.Helper()
	ctx := context.Background()
	for _, s := range stmts {
		var err error
		switch x := d.(type) {
		case *pg:
			_, err = x.pool.Exec(ctx, s)
		case *my:
			_, err = x.db.ExecContext(ctx, s)
		}
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
}

func values(n int, row func(i int) string) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		if i > 1 {
			b.WriteString(", ")
		}
		b.WriteString("(" + row(i) + ")")
	}
	return b.String()
}

// openCycleDB recreates cycleDB on the server behind env and seeds:
// product_customer ⇄ pbp_member (mutual FKs, the customer → member side
// nullable), category → category (self FK), claim → pbp_member, category.
func openCycleDB(t *testing.T, env string) DB {
	raw := os.Getenv(env)
	if raw == "" {
		t.Skip(env + " not set")
	}
	ctx := context.Background()
	admin, err := Open(ctx, raw, Options{NoSilence: true})
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, admin, "DROP DATABASE IF EXISTS "+cycleDB, "CREATE DATABASE "+cycleDB)
	admin.Close()
	u, _ := url.Parse(raw)
	u.Path = "/" + cycleDB
	d, err := Open(ctx, u.String(), Options{NoSilence: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		d.Close()
		if a, err := Open(ctx, raw, Options{NoSilence: true}); err == nil {
			execAll(t, a, "DROP DATABASE IF EXISTS "+cycleDB)
			a.Close()
		}
	})
	text, intT := "text", "bigint"
	if _, ok := d.(*my); ok {
		text = "varchar(20)"
	}
	execAll(t, d,
		fmt.Sprintf("CREATE TABLE product_customer (id %s PRIMARY KEY, name %s, pbp_member_id %s)", intT, text, intT),
		fmt.Sprintf("CREATE TABLE pbp_member (id %s PRIMARY KEY, product_customer_id %s NOT NULL REFERENCES product_customer (id), name %s)", intT, intT, text),
		"ALTER TABLE product_customer ADD CONSTRAINT pc_member FOREIGN KEY (pbp_member_id) REFERENCES pbp_member (id)",
		fmt.Sprintf("CREATE TABLE category (id %s PRIMARY KEY, parent_id %s REFERENCES category (id))", intT, intT),
		fmt.Sprintf("CREATE TABLE claim (id %s PRIMARY KEY, pbp_member_id %s NOT NULL REFERENCES pbp_member (id), category_id %s REFERENCES category (id))", intT, intT, intT),
		"INSERT INTO product_customer (id, name, pbp_member_id) VALUES "+values(2000, func(i int) string { return fmt.Sprintf("%d, 'c%d', NULL", i, i) }),
		"INSERT INTO pbp_member (id, product_customer_id, name) VALUES "+values(6000, func(i int) string { return fmt.Sprintf("%d, %d, 'm%d'", i, (i-1)/3+1, i) }),
		"UPDATE product_customer SET pbp_member_id = (id - 1) * 3 + 1", // primary member: one of its own
		"INSERT INTO category (id, parent_id) VALUES "+values(1000, func(i int) string {
			if i == 1 {
				return "1, NULL"
			}
			return fmt.Sprintf("%d, %d", i, i/2)
		}),
		"INSERT INTO claim (id, pbp_member_id, category_id) VALUES "+values(12000, func(i int) string { return fmt.Sprintf("%d, %d, %d", i, i%6000+1, i%1000+1) }),
	)
	if _, ok := d.(*my); ok {
		execAll(t, d, "ANALYZE TABLE product_customer, pbp_member, category, claim")
	} else {
		execAll(t, d, "ANALYZE")
	}
	return d
}

func idSet(t *testing.T, d DB, ns, table string) map[string]bool {
	t.Helper()
	vals, err := d.FirstValues(context.Background(), fmt.Sprintf("SELECT id FROM %s.%s", d.Quote(ns), d.Quote(table)))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, v := range vals {
		out[v] = true
	}
	return out
}

func testCycleSubsets(t *testing.T, env string) {
	d := openCycleDB(t, env)
	ctx := context.Background()
	tables, err := d.Tables(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fks, err := d.FKs(ctx, tables)
	if err != nil {
		t.Fatal(err)
	}
	deferred := map[string]bool{}
	for _, f := range fks {
		if f.Deferred {
			deferred[f.Child+"."+f.ChildCol] = true
		}
	}
	if !deferred["product_customer.pbp_member_id"] || !deferred["category.parent_id"] || len(deferred) != 2 {
		t.Fatalf("deferred FKs = %v, want the nullable side of the cycle and the self-reference", deferred)
	}
	var keys []string
	for k := range tables {
		keys = append(keys, k)
	}
	order := TopoOrder(ParentClosure(d.Info().Dialect, keys, tables, fks), fks)
	steps := []float64{0.05, 0.2, 0.5} // not the default data steps: MySQL namespaces are server-wide
	for _, s := range steps {
		defer d.DropNamespace(context.Background(), NSName(s))
	}
	counts := map[float64]map[string]float64{}
	for _, s := range steps {
		cnt, err := d.BuildSubset(ctx, NSName(s), s, order, fks, "")
		if err != nil {
			t.Fatalf("subset %v: %v", s, err)
		}
		counts[s] = cnt
		for _, k := range []string{"product_customer", "pbp_member", "category", "claim"} {
			if cnt[k] == 0 {
				t.Errorf("subset %v: %s is empty", s, k)
			}
		}
		// members follow their customer: every member of a sampled customer, no others
		if cnt["pbp_member"] != 3*cnt["product_customer"] {
			t.Errorf("subset %v: %v members for %v customers, want 3 per customer", s, cnt["pbp_member"], cnt["product_customer"])
		}
	}
	for _, k := range []string{"product_customer", "pbp_member", "category", "claim"} {
		for i := 1; i < len(steps); i++ {
			small, big := idSet(t, d, NSName(steps[i-1]), k), idSet(t, d, NSName(steps[i]), k)
			for id := range small {
				if !big[id] {
					t.Errorf("%s id %s is in the %v subset but not in %v: subsets must nest", k, id, steps[i-1], steps[i])
					break
				}
			}
		}
	}
	// FK constraints are in place on the copies (cascades and RI checks cost the same)
	ns := d.Quote(NSName(0.2))
	for _, s := range []string{
		fmt.Sprintf("INSERT INTO %s.%s (id, pbp_member_id, category_id) VALUES (999999, -1, NULL)", ns, d.Quote("claim")),
		fmt.Sprintf("UPDATE %s.%s SET pbp_member_id = -1", ns, d.Quote("product_customer")),
		fmt.Sprintf("UPDATE %s.%s SET parent_id = -1", ns, d.Quote("category")),
	} {
		var err error
		switch x := d.(type) {
		case *pg:
			_, err = x.pool.Exec(ctx, s)
		case *my:
			_, err = x.db.ExecContext(ctx, s)
		}
		if err == nil {
			t.Errorf("%s: want a foreign-key violation on the subset copy", s)
		}
	}
}

func TestCycleSubsetsPostgres(t *testing.T) { testCycleSubsets(t, "RP_IT_PG") }
func TestCycleSubsetsMySQL(t *testing.T)    { testCycleSubsets(t, "RP_IT_MYSQL") }
