package sqlutil

import (
	"sort"
	"strings"
	"testing"
)

func TestParsePG(t *testing.T) {
	p := Parse("postgres", `WITH recent AS (SELECT * FROM orders WHERE created_at > now() - interval '1 day')
		SELECT u.id FROM "Billing"."Users" u JOIN recent r ON r.user_id = u.id WHERE u.id = $1`)
	if !p.OK || p.Kind != "select" {
		t.Fatalf("parse: %+v", p)
	}
	names := p.Names()
	sort.Strings(names)
	if got := strings.Join(names, ","); got != "orders,users" {
		t.Errorf("names = %s (CTE must be excluded, quoted names folded)", got)
	}
	if a, b := Parse("postgres", "SELECT * FROM t WHERE id IN (1,2,3)"), Parse("postgres", "select * from t where id in (7)"); a.Fingerprint != b.Fingerprint {
		t.Errorf("IN-list length must not change the fingerprint")
	}
	if p := Parse("postgres", "WITH d AS (DELETE FROM sessions WHERE user_id = 1 RETURNING id) SELECT count(*) FROM d"); p.Kind != "delete" {
		t.Errorf("data-modifying CTE kind = %s", p.Kind)
	}
	if p := Parse("postgres", "UPDATE ONLY public.orders SET status = 'x' WHERE id = 1"); p.Kind != "update" || p.Target.Name != "orders" || p.Target.Schema != "public" {
		t.Errorf("update target: %+v", p.Target)
	}
	if p := Parse("postgres", "SELECT 1"); p.Kind != "other" {
		t.Errorf("SELECT 1 kind = %s", p.Kind)
	}
}

func TestRewritePG(t *testing.T) {
	sql := `SELECT o.id, public.orders.total FROM public.orders o JOIN users ON users.id = o.user_id, LATERAL (SELECT 1 FROM ONLY items i WHERE i.order_id = o.id) x WHERE o.id = 'it''s'`
	out, err := Rewrite("postgres", sql, func(r Ref) (string, string, bool) {
		if r.Name == "orders" || r.Name == "items" {
			return "_rp_s10", r.Name, true
		}
		return "", "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT o.id, "_rp_s10".orders.total FROM "_rp_s10"."orders" o JOIN users ON users.id = o.user_id, LATERAL (SELECT 1 FROM ONLY "_rp_s10"."items" i WHERE i.order_id = o.id) x WHERE o.id = 'it''s'`
	if out != want {
		t.Errorf("rewrite:\n got %s\nwant %s", out, want)
	}
}

func TestParseMySQL(t *testing.T) {
	p := Parse("mysql", "DELETE oi FROM order_items oi JOIN orders o ON o.id = oi.order_id WHERE o.user_id = 5")
	if !p.OK || p.Kind != "delete" || p.Target.Name != "order_items" {
		t.Fatalf("multi-table delete: %+v", p)
	}
	if strings.Join(p.Names(), ",") != "order_items,orders" {
		t.Errorf("names = %v", p.Names())
	}
	a := Parse("mysql", "SELECT * FROM t WHERE id IN (1,2,3) /* traceparent='00-ab' */")
	b := Parse("mysql", "select * from t where id in (9)")
	if a.Fingerprint != b.Fingerprint {
		t.Errorf("fingerprints differ: %s %s", a.Fingerprint, b.Fingerprint)
	}
	c := Parse("mysql", "INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')")
	d := Parse("mysql", "INSERT INTO t (a, b) VALUES (3, 'z')")
	if c.Fingerprint != d.Fingerprint || c.Kind != "insert" || c.Target.Name != "t" {
		t.Errorf("multi-row insert: %+v vs %+v", c, d)
	}
	if p := Parse("mysql", "SELECT @@version"); p.Kind != "other" {
		t.Errorf("SELECT @@version kind = %s", p.Kind)
	}
}

func TestRewriteMySQL(t *testing.T) {
	out, err := Rewrite("mysql", "SELECT o.id FROM app.orders o JOIN users u ON u.id = o.user_id WHERE app.orders.id = 1", func(r Ref) (string, string, bool) {
		if r.Name == "orders" {
			return "_rp_s10", "orders", true
		}
		return "", "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "_rp_s10.orders as o") || !strings.Contains(out, "_rp_s10.orders.id = 1") || strings.Contains(out, "app.") {
		t.Errorf("rewrite: %s", out)
	}
	if got := MySQLWhereProbe("UPDATE users SET name = 'x' WHERE id = 7"); got != "select 1 from users where id = 7" {
		t.Errorf("probe: %s", got)
	}
}
