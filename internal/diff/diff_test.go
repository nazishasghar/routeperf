package diff

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseBigO(t *testing.T) {
	cases := map[string]deg{
		"O(1)":                              {},
		"O(log n_users)":                    {nlog: 1},
		"O(k·log n_order_items + n_orders)": {n: 1, k: 1, klog: 0},
		"O(n_orders^2 + k)":                 {n: 2, k: 1},
		"O(n_orders·log n_orders)":          {n: 1, nlog: 1},
		"O(k^1.5)":                          {k: 1.5},
	}
	for s, want := range cases {
		got := parseBigO(s)
		if got.n != want.n || got.k != want.k || got.nlog != want.nlog {
			t.Errorf("%s: got %+v want %+v", s, got, want)
		}
	}
	if compareDeg(parseBigO("O(log n_orders)"), parseBigO("O(n_orders)")) <= 0 {
		t.Error("O(log n) → O(n) must be a regression")
	}
	if compareDeg(parseBigO("O(n_orders)"), parseBigO("O(log n_orders)")) >= 0 {
		t.Error("O(n) → O(log n) must be an improvement")
	}
}

func TestCompare(t *testing.T) {
	dir := t.TempDir()
	base := `{"operations":[
	 {"method":"GET","path":"/a","status":"OK","big_o":"O(log n_t)","latency_ms":{"p95":10},"queries_per_request":1},
	 {"method":"GET","path":"/b","status":"OK","big_o":"O(1)","latency_ms":{"p95":10},"queries_per_request":1},
	 {"method":"GET","path":"/gone","status":"OK","big_o":"O(1)","latency_ms":{"p95":1}}]}`
	cur := `{"operations":[
	 {"method":"GET","path":"/a","status":"FAIL","big_o":"O(n_t)","latency_ms":{"p95":11},"queries_per_request":1},
	 {"method":"GET","path":"/b","status":"OK","big_o":"O(1)","latency_ms":{"p95":10.5},"queries_per_request":1},
	 {"method":"GET","path":"/new","status":"OK","big_o":"O(1)","latency_ms":{"p95":1}}]}`
	bp, np := filepath.Join(dir, "base.json"), filepath.Join(dir, "new.json")
	os.WriteFile(bp, []byte(base), 0o644)
	os.WriteFile(np, []byte(cur), 0o644)
	rep, err := Compare(bp, np, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Failed || len(rep.Changes[0].Problems) != 2 || rep.Changes[0].Endpoint != "GET /a" {
		t.Fatalf("want /a to fail on Big O and status: %+v", rep.Changes)
	}
	if len(rep.Added) != 1 || len(rep.Removed) != 1 {
		t.Errorf("added %v removed %v", rep.Added, rep.Removed)
	}
	// p95 +20% but below the 5 ms floor is noise
	for _, c := range rep.Changes {
		if c.Endpoint == "GET /b" && len(c.Problems) > 0 {
			t.Errorf("/b must not regress: %v", c.Problems)
		}
	}
}
