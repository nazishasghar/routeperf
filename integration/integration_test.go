//go:build integration

// End-to-end test: seeds the testbed, runs routeperf against it and asserts
// that every planted problem is detected with the right Big O and fix.
//
//	RP_IT_PG=postgres://localhost:5432/routeperf_it \
//	RP_IT_MYSQL=mysql://root@127.0.0.1:3306/routeperf_it \
//	go test -tags integration -v -timeout 20m ./integration/
package integration

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type advice struct {
	Rule, Message, SQL string
}

type query struct {
	ID    string `json:"id"`
	SQL   string `json:"sql"`
	Kind  string `json:"kind"`
	BigO  string `json:"big_o"`
	Error string `json:"error"`
	Scale []any  `json:"scale"`
}

type op struct {
	Method     string         `json:"method"`
	Path       string         `json:"path"`
	Status     string         `json:"status"`
	BigO       string         `json:"big_o"`
	Confidence string         `json:"confidence"`
	NPlusOne   []string       `json:"n_plus_one"`
	AppKExp    float64        `json:"app_k_exp"`
	Statuses   map[string]int `json:"statuses"`
	Queries    []query        `json:"queries"`
	Advice     []advice       `json:"advice"`
}

type result struct {
	Ops      []op     `json:"operations"`
	Verified bool     `json:"restore_verified"`
	Snapshot string   `json:"snapshot"`
	Warnings []string `json:"warnings"`
}

func build(t *testing.T, dir string) (string, string) {
	t.Helper()
	rp, tb := filepath.Join(dir, "routeperf"), filepath.Join(dir, "testbed")
	for out, pkg := range map[string]string{rp: "./cmd/routeperf", tb: "./testbed"} {
		cmd := exec.Command("go", "build", "-o", out, pkg)
		cmd.Dir = ".."
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", pkg, err, b)
		}
	}
	return rp, tb
}

func freePort(t *testing.T) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	rp, tb := build(t, dir)
	for _, c := range []struct{ name, url string }{{"postgres", os.Getenv("RP_IT_PG")}, {"mysql", os.Getenv("RP_IT_MYSQL")}} {
		t.Run(c.name, func(t *testing.T) {
			if c.url == "" {
				t.Skipf("set RP_IT_%s to run", strings.ToUpper(map[string]string{"postgres": "pg", "mysql": "mysql"}[c.name]))
			}
			runDialect(t, c.name, c.url, rp, tb, dir)
		})
	}
}

func runDialect(t *testing.T, dialect, dbURL, rp, tb, dir string) {
	users := env("RP_IT_USERS", "10000")
	seed := exec.Command(tb, "--db-url", dbURL, "--seed", "--users", users)
	if b, err := seed.CombinedOutput(); err != nil {
		t.Fatalf("seed: %v\n%s", err, b)
	}
	port := freePort(t)
	api := fmt.Sprintf("http://127.0.0.1:%d", port)
	srv := exec.Command(tb, "--db-url", dbURL, "--addr", fmt.Sprintf("127.0.0.1:%d", port), "--background-noise", "120ms")
	srv.Stdout, srv.Stderr = os.Stderr, os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srv.Process.Kill(); _, _ = srv.Process.Wait() }()
	for i := 0; ; i++ {
		if resp, err := http.Get(api + "/health"); err == nil {
			resp.Body.Close()
			break
		}
		if i > 100 {
			t.Fatal("testbed did not start")
		}
		time.Sleep(100 * time.Millisecond)
	}
	out := filepath.Join(dir, dialect+"-out")
	cmd := exec.Command(rp, "run", "--yes", "--non-interactive", "-c", filepath.Join(dir, "none.yaml"),
		"--spec", api+"/swagger.json", "--api-url", api, "--db-url", dbURL, "--token", "testtoken", "-n", "6", "-o", out)
	cmd.Dir = dir
	b, err := cmd.CombinedOutput()
	t.Logf("routeperf output:\n%s", b)
	if err != nil {
		t.Fatalf("routeperf run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(out, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res result
	if err := json.Unmarshal(raw, &res); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(out, "report.md")); err != nil {
		t.Errorf("report.md missing: %v", err)
	}

	get := func(method, path string) op {
		for _, o := range res.Ops {
			if o.Method == method && o.Path == path {
				return o
			}
		}
		t.Fatalf("operation %s %s missing from results", method, path)
		return op{}
	}
	hasAdvice := func(o op, rule, sqlPrefix string) bool {
		for _, a := range o.Advice {
			if a.Rule == rule && strings.HasPrefix(a.SQL, sqlPrefix) {
				return true
			}
		}
		return false
	}

	if res.Snapshot != "tables" || !res.Verified {
		t.Errorf("write data must be restored and verified (snapshot=%s verified=%v)", res.Snapshot, res.Verified)
	}
	for _, w := range res.Warnings {
		if strings.Contains(w, "RESTORE FAILED") {
			t.Errorf("warning: %s", w)
		}
	}

	// seq scan + N+1
	o := get("GET", "/users/{id}/orders")
	if o.Status != "FAIL" || len(o.NPlusOne) == 0 || !strings.Contains(o.BigO, "n_orders") || !strings.Contains(o.BigO, "k") {
		t.Errorf("/users/{id}/orders: want FAIL, N+1 and O(k·… + n_orders), got %s %s N+1=%v", o.Status, o.BigO, o.NPlusOne)
	}
	if !hasAdvice(o, "unindexed_filter", "CREATE INDEX idx_orders_user_id") {
		t.Errorf("/users/{id}/orders: want named index advice on orders(user_id…), got %+v", o.Advice)
	}
	// unindexed status filter
	if o := get("GET", "/orders/search"); !strings.Contains(o.BigO, "n_orders") || !hasAdvice(o, "unindexed_filter", "CREATE INDEX idx_orders_status") {
		t.Errorf("/orders/search: want O(n_orders) + index on status, got %s %+v", o.BigO, o.Advice)
	}
	// app-side O(k²)
	if o := get("GET", "/users/{id}/recommendations"); o.AppKExp != 2 || o.Status != "WARN" {
		t.Errorf("/users/{id}/recommendations: want app-side k², got app_k_exp=%v status=%s", o.AppKExp, o.Status)
	}
	// good routes
	if o := get("GET", "/users/{id}"); o.Status != "OK" || strings.Contains(strings.ReplaceAll(o.BigO, "log n_users", ""), "n_") {
		t.Errorf("/users/{id}: want OK and O(log n)/O(1), got %s %s", o.Status, o.BigO)
	}
	if o := get("GET", "/orders"); o.BigO != "O(k)" || o.Status != "OK" {
		t.Errorf("/orders: want O(k) OK, got %s %s", o.BigO, o.Status)
	}
	// per-item INSERT loop
	if o := get("POST", "/orders"); len(o.NPlusOne) == 0 {
		t.Errorf("POST /orders: want insert-per-item N+1, got %+v", o)
	}
	// unindexed FK cascade
	o = get("DELETE", "/users/{id}")
	if !strings.Contains(o.BigO, "n_orders") {
		t.Errorf("DELETE /users/{id}: want O(n_orders), got %s", o.BigO)
	}
	if dialect == "postgres" && !hasAdvice(o, "unindexed_fk", "CREATE INDEX idx_orders_user_id ON orders (user_id);") {
		t.Errorf("DELETE /users/{id}: want unindexed FK advice, got %+v", o.Advice)
	}
	// UUID keys: writes on tool-created rows must replay on every subset
	for _, m := range []string{"GET", "PUT", "DELETE"} {
		o := get(m, "/notes/{id}")
		for code, n := range o.Statuses {
			if code >= "300" {
				t.Errorf("%s /notes/{id}: %d responses with status %s", m, n, code)
			}
		}
		if strings.Contains(strings.ReplaceAll(o.BigO, "log n_notes", ""), "n_notes") {
			t.Errorf("%s /notes/{id}: want O(log n)/O(1) on a UUID PK, got %s", m, o.BigO)
		}
		for _, q := range o.Queries {
			if q.Error != "" {
				t.Errorf("%s /notes/{id} %s replay error: %s", m, q.ID, q.Error)
			}
			if q.Kind != "insert" && len(q.Scale) < 3 {
				t.Errorf("%s /notes/{id} %s: only %d data-scale points (UUID retarget failed?)", m, q.ID, len(q.Scale))
			}
		}
	}
	// background job SQL must not be attributed to endpoints
	for _, o := range res.Ops {
		for _, q := range o.Queries {
			if strings.Contains(q.SQL, "token LIKE") {
				t.Errorf("%s %s: background query leaked into endpoint: %s", o.Method, o.Path, q.SQL)
			}
		}
	}
	bg := false
	for _, w := range res.Warnings {
		bg = bg || strings.Contains(w, "background SQL")
	}
	if !bg {
		t.Errorf("want a background-SQL warning, got %v", res.Warnings)
	}
}
