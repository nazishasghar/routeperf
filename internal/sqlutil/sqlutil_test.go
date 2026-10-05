package sqlutil

import "testing"

func TestFingerprintIgnoresLiterals(t *testing.T) {
	a := Fingerprint("SELECT * FROM orders WHERE user_id = 42 AND status = 'paid'")
	b := Fingerprint("select *  from orders where user_id = $1 and status = $2")
	if a != b {
		t.Fatalf("fingerprints differ: %s %s", a, b)
	}
	if Fingerprint("SELECT 1 FROM a WHERE id IN (1,2,3)") != Fingerprint("SELECT 1 FROM a WHERE id IN (?)") {
		t.Fatal("IN lists should collapse")
	}
}

func TestKindTablesTarget(t *testing.T) {
	if k := Kind("  UPDATE users SET a=1"); k != "update" {
		t.Fatal(k)
	}
	ts := Tables(`SELECT o.id FROM "public"."orders" o JOIN order_items i ON i.order_id = o.id`)
	if len(ts) != 2 || ts[0] != "orders" || ts[1] != "order_items" {
		t.Fatalf("tables %v", ts)
	}
	if tt := TargetTable("DELETE FROM `sessions` WHERE user_id = 1"); tt != "sessions" {
		t.Fatal(tt)
	}
}

func TestInlineAndRewrite(t *testing.T) {
	got := InlinePG("SELECT * FROM t WHERE a = $1 AND b = $10", []string{"x'y", "", "", "", "", "", "", "", "", "9"}, nil)
	if got != "SELECT * FROM t WHERE a = 'x''y' AND b = '9'" {
		t.Fatal(got)
	}
	if r := RewriteSchema(`SELECT * FROM "public"."users"`, "public", "_rp_s10", []string{"users"}); r != "SELECT * FROM _rp_s10.users" {
		t.Fatal(r)
	}
}
