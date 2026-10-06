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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type advice struct {
	Rule, Message, SQL, Verified string
}

type query struct {
	ID       string `json:"id"`
	SQL      string `json:"sql"`
	Kind     string `json:"kind"`
	BigO     string `json:"big_o"`
	Error    string `json:"error"`
	Scale    []any  `json:"scale"`
	PerTable []struct {
		Table string  `json:"table"`
		Slope float64 `json:"slope"`
		CI    float64 `json:"ci"`
	} `json:"per_table"`
	WorkFit *struct {
		Slope   float64 `json:"slope"`
		SlopeCI float64 `json:"slope_ci"`
	} `json:"work_fit"`
}

type op struct {
	Pages      int            `json:"cursor_pages"`
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
	DBTime   string   `json:"db_time_source"`
	Proof    string   `json:"advice_proof"`
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

func dbName(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(u.Path, "/")
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestEndToEnd(t *testing.T) {
	dir := t.TempDir()
	if keep := os.Getenv("RUNNER_TEMP"); keep != "" { // CI: keep the reports as an artifact
		dir = filepath.Join(keep, "routeperf-it")
		_ = os.MkdirAll(dir, 0o755)
	}
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
	args := []string{"run", "--yes", "--non-interactive", "-c", filepath.Join(dir, "none.yaml"),
		"--spec", api + "/swagger.json", "--api-url", api, "--db-url", dbURL, "--token", "testtoken", "-n", "6", "-o", out}
	if dialect == "mysql" {
		args = append(args, "--db-schemas", dbName(dbURL)+"_archive")
	}
	cmd := exec.Command(rp, args...)
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
	for _, f := range []string{"report.md", "report.html"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Errorf("%s missing: %v", f, err)
		}
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
	// partitioned table with a composite key: unindexed filter across partitions
	if o := get("GET", "/events/search"); !strings.Contains(o.BigO, "n_events") || !hasAdvice(o, "unindexed_filter", "CREATE INDEX idx_events_kind") {
		t.Errorf("/events/search: want O(n_events) + index on events(kind), got %s %+v", o.BigO, o.Advice)
	}
	// a view over an unindexed join: subsets must contain the view too
	o = get("GET", "/users/{id}/totals")
	if !strings.Contains(o.BigO, "n_orders") {
		t.Errorf("/users/{id}/totals: want O(n_orders) through the view, got %s", o.BigO)
	}
	for _, q := range o.Queries {
		if q.Error != "" || len(q.Scale) < 3 {
			t.Errorf("/users/{id}/totals %s: error %q, %d scale points (view subset failed?)", q.ID, q.Error, len(q.Scale))
		}
	}
	// per-table scaling attributes a two-table join to both tables
	o = get("GET", "/orders/by-country")
	tables := map[string]float64{}
	for _, q := range o.Queries {
		for _, g := range q.PerTable {
			tables[g.Table] = g.Slope
		}
	}
	if dialect == "postgres" { // hash join: both scans grow
		if tables["orders"] < 0.7 || tables["users"] < 0.5 || !strings.Contains(o.BigO, "n_orders") || !strings.Contains(o.BigO, "n_users") {
			t.Errorf("/orders/by-country: want per-table growth in orders and users, got %v (%s)", tables, o.BigO)
		}
	} else if tables["orders"] < 0.7 || tables["users"] > 0.3 { // nested loop of PK lookups: only orders grows
		t.Errorf("/orders/by-country: want growth in orders only (PK lookups into users), got %v (%s)", tables, o.BigO)
	}
	// same-named table in another schema / database
	o = get("GET", "/archive/orders/{id}")
	for code, n := range o.Statuses {
		if code >= "300" {
			t.Errorf("/archive/orders/{id}: %d responses with status %s", n, code)
		}
	}
	for _, q := range o.Queries {
		if q.Error != "" || len(q.Scale) < 3 || !strings.Contains(q.BigO+o.BigO, "archive") && strings.Contains(o.BigO, "n_") {
			t.Errorf("/archive/orders/{id} %s: error %q, %d scale points, Big O %s", q.ID, q.Error, len(q.Scale), o.BigO)
		}
	}
	// cursor pagination is walked; form and multipart bodies are sent
	if o := get("GET", "/events/feed"); o.Pages < 2 {
		t.Errorf("/events/feed: want the cursor walked over several pages, got %d", o.Pages)
	}
	for _, x := range []struct{ m, p string }{{"POST", "/sessions"}, {"POST", "/users/{id}/avatar"}, {"GET", "/events/feed"}} {
		o := get(x.m, x.p)
		for code, n := range o.Statuses {
			if code >= "300" {
				t.Errorf("%s %s: %d responses with status %s", x.m, x.p, n, code)
			}
		}
	}
	// slopes carry confidence intervals
	if q := get("GET", "/orders/search").Queries; len(q) == 0 || q[0].WorkFit == nil || q[0].WorkFit.SlopeCI <= 0 && q[0].WorkFit.Slope == 0 {
		t.Errorf("/orders/search: want a rows-examined slope with a CI")
	}
	if res.Proof == "hypopg" { // HypoPG installed: index advice must come with its proof
		for _, path := range []string{"/orders/search", "/users/{id}/orders"} {
			for _, a := range get("GET", path).Advice {
				if a.Rule == "unindexed_filter" && !strings.HasPrefix(a.Verified, "HypoPG: planner cost") {
					t.Errorf("%s: index advice not proven: %+v", path, a)
				}
			}
		}
		for _, a := range get("DELETE", "/users/{id}").Advice {
			if a.Rule == "unindexed_fk" && !strings.HasPrefix(a.Verified, "HypoPG: planner cost") {
				t.Errorf("DELETE /users/{id}: FK index advice not proven: %+v", a)
			}
		}
	}
	if dialect == "mysql" && res.DBTime != "performance_schema" {
		t.Errorf("mysql DB time should come from performance_schema, got %q", res.DBTime)
	}
	bg := false
	for _, w := range res.Warnings {
		bg = bg || strings.Contains(w, "background SQL")
	}
	if !bg {
		t.Errorf("want a background-SQL warning, got %v", res.Warnings)
	}
}

// TestProxyAndLoad captures SQL through the wire proxy (no admin rights, no
// server log), attributes it by sqlcommenter traceparent and runs load mode.
func TestProxyAndLoad(t *testing.T) {
	dir := t.TempDir()
	rp, tb := build(t, dir)
	for _, c := range []struct{ name, url string }{{"postgres", os.Getenv("RP_IT_PG")}, {"mysql", os.Getenv("RP_IT_MYSQL")}} {
		t.Run(c.name, func(t *testing.T) {
			if c.url == "" {
				t.Skip("database URL not set")
			}
			seed := exec.Command(tb, "--db-url", c.url, "--seed", "--users", env("RP_IT_USERS", "10000"))
			if b, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seed: %v\n%s", err, b)
			}
			listen := fmt.Sprintf("127.0.0.1:%d", freePort(t))
			for { // the control API takes the next port
				h, p, _ := net.SplitHostPort(listen)
				var n int
				fmt.Sscan(p, &n)
				if l, err := net.Listen("tcp", fmt.Sprintf("%s:%d", h, n+1)); err == nil {
					l.Close()
					break
				}
				listen = fmt.Sprintf("127.0.0.1:%d", freePort(t))
			}
			px := exec.Command(rp, "proxy", "-c", filepath.Join(dir, "none.yaml"), "--db-url", c.url, "--proxy-listen", listen)
			px.Stdout, px.Stderr = os.Stderr, os.Stderr
			if err := px.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = px.Process.Kill(); _, _ = px.Process.Wait() }()
			h, p, _ := net.SplitHostPort(listen)
			var pn int
			fmt.Sscan(p, &pn)
			for i := 0; ; i++ { // wait for the proxy's control API
				if resp, err := http.Get(fmt.Sprintf("http://%s:%d/health", h, pn+1)); err == nil {
					resp.Body.Close()
					break
				}
				if i > 100 {
					t.Fatal("proxy did not start")
				}
				time.Sleep(100 * time.Millisecond)
			}
			u, _ := url.Parse(c.url)
			u.Host = listen
			if c.name == "postgres" {
				u.RawQuery = "sslmode=disable"
			}
			port := freePort(t)
			api := fmt.Sprintf("http://127.0.0.1:%d", port)
			srv := exec.Command(tb, "--db-url", u.String(), "--addr", fmt.Sprintf("127.0.0.1:%d", port), "--sqlcommenter")
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
			out := filepath.Join(dir, c.name+"-proxy-out")
			cmd := exec.Command(rp, "run", "--yes", "--non-interactive", "-c", filepath.Join(dir, "none.yaml"), "--capture", "proxy", "--proxy-listen", listen,
				"--spec", api+"/swagger.json", "--api-url", api, "--db-url", c.url, "--token", "testtoken", "-n", "4", "--no-writes",
				"--only", "getUserOrders,searchOrders,getUser", "--load", "--load-concurrency", "1,4,16", "--load-duration", "2s", "--mask-literals", "-o", out)
			b, err := cmd.CombinedOutput()
			t.Logf("routeperf output:\n%s", b)
			if err != nil {
				t.Fatalf("routeperf run: %v", err)
			}
			raw, err := os.ReadFile(filepath.Join(out, "results.json"))
			if err != nil {
				t.Fatal(err)
			}
			var res struct {
				Capture    string `json:"capture"`
				DBTime     string `json:"db_time_source"`
				Correlated bool   `json:"traceparent_correlation"`
				Masked     bool   `json:"literals_masked"`
				Ops        []struct {
					Path     string   `json:"path"`
					BigO     string   `json:"big_o"`
					NPlusOne []string `json:"n_plus_one"`
					Queries  []struct {
						SQL string `json:"sql"`
					} `json:"queries"`
					Load []struct {
						Concurrency int     `json:"concurrency"`
						RPS         float64 `json:"rps"`
						QPerReq     float64 `json:"queries_per_request"`
						MaxConns    int     `json:"db_connections_max"`
					} `json:"load"`
					LoadNote string `json:"load_verdict"`
				} `json:"operations"`
			}
			if err := json.Unmarshal(raw, &res); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(res.Capture, "proxy") || res.DBTime != "proxy (wire time)" {
				t.Errorf("want proxy capture, got capture=%q db_time=%q", res.Capture, res.DBTime)
			}
			if !res.Correlated {
				t.Errorf("want SQL attributed by traceparent (testbed runs with --sqlcommenter)")
			}
			if !res.Masked {
				t.Errorf("want literals masked")
			}
			for _, o := range res.Ops {
				for _, q := range o.Queries {
					if strings.Contains(q.SQL, "'paid'") {
						t.Errorf("%s: literal leaked into a masked report: %s", o.Path, q.SQL)
					}
				}
				if len(o.Load) != 3 || o.Load[2].RPS <= 0 {
					t.Errorf("%s: want 3 load steps, got %+v", o.Path, o.Load)
					continue
				}
				if o.Path == "/users/{id}/orders" {
					if len(o.NPlusOne) == 0 || !strings.Contains(o.BigO, "n_orders") {
						t.Errorf("/users/{id}/orders via proxy: want N+1 and O(… n_orders), got %s %v", o.BigO, o.NPlusOne)
					}
					if q := o.Load[1].QPerReq; q < 5 { // 1 + one per item, attributed under concurrency
						t.Errorf("/users/{id}/orders under load: want ≥5 queries/request attributed by traceparent, got %.0f", q)
					}
				}
				if o.Path == "/orders/search" && !strings.Contains(o.LoadNote, "connections") && o.Load[2].MaxConns > 10 {
					t.Errorf("/orders/search: the testbed pool is 10 connections, got %+v (%s)", o.Load, o.LoadNote)
				}
			}
		})
	}
}

// TestGraphQLAndGRPC measures the testbed's GraphQL endpoint (schema from
// introspection) and its gRPC service (schema from server reflection).
func TestGraphQLAndGRPC(t *testing.T) {
	dir := t.TempDir()
	rp, tb := build(t, dir)
	for _, c := range []struct{ name, url string }{{"postgres", os.Getenv("RP_IT_PG")}, {"mysql", os.Getenv("RP_IT_MYSQL")}} {
		t.Run(c.name, func(t *testing.T) {
			if c.url == "" {
				t.Skip("database URL not set")
			}
			seed := exec.Command(tb, "--db-url", c.url, "--seed", "--users", env("RP_IT_USERS", "10000"))
			if b, err := seed.CombinedOutput(); err != nil {
				t.Fatalf("seed: %v\n%s", err, b)
			}
			port, gport := freePort(t), freePort(t)
			api := fmt.Sprintf("http://127.0.0.1:%d", port)
			srv := exec.Command(tb, "--db-url", c.url, "--addr", fmt.Sprintf("127.0.0.1:%d", port), "--grpc-addr", fmt.Sprintf("127.0.0.1:%d", gport))
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
			type gop struct {
				Method   string         `json:"method"`
				Path     string         `json:"path"`
				Status   string         `json:"status"`
				BigO     string         `json:"big_o"`
				NPlusOne []string       `json:"n_plus_one"`
				Statuses map[string]int `json:"statuses"`
				Advice   []advice       `json:"advice"`
			}
			runRP := func(name string, args ...string) map[string]gop {
				out := filepath.Join(dir, c.name+"-"+name)
				all := append([]string{"run", "--yes", "--non-interactive", "-c", filepath.Join(dir, "none.yaml"), "--db-url", c.url, "-n", "4", "-o", out}, args...)
				b, err := exec.Command(rp, all...).CombinedOutput()
				t.Logf("routeperf %s output:\n%s", name, b)
				if err != nil {
					t.Fatalf("routeperf %s: %v", name, err)
				}
				raw, err := os.ReadFile(filepath.Join(out, "results.json"))
				if err != nil {
					t.Fatal(err)
				}
				var res struct {
					Protocol string `json:"protocol"`
					Ops      []gop  `json:"operations"`
				}
				if err := json.Unmarshal(raw, &res); err != nil {
					t.Fatal(err)
				}
				if res.Protocol != name {
					t.Errorf("protocol = %q, want %s", res.Protocol, name)
				}
				m := map[string]gop{}
				for _, o := range res.Ops {
					m[o.Method+" "+o.Path] = o
					for code, n := range o.Statuses {
						if code >= "300" {
							t.Errorf("%s %s: %d responses with status %s", o.Method, o.Path, n, code)
						}
					}
				}
				return m
			}
			g := runRP("graphql", "--spec", api+"/graphql", "--token", "testtoken")
			if o := g["QUERY users"]; len(o.NPlusOne) == 0 || !strings.Contains(o.BigO, "k") {
				t.Errorf("GraphQL users { notes }: want the nested-resolver N+1, got %s %v", o.BigO, o.NPlusOne)
			}
			if o, ok := g["QUERY user"]; !ok || o.Status != "OK" {
				t.Errorf("GraphQL user(id): want OK, got %+v", o)
			}
			if _, ok := g["MUTATION createUser"]; !ok {
				t.Errorf("GraphQL createUser mutation missing: %v", g)
			}
			r := runRP("grpc", "--spec", fmt.Sprintf("grpc://127.0.0.1:%d", gport), "--token-cmd", "echo testtoken") // token from a command
			if o := r["RPC Shop/ListUserOrders"]; !strings.Contains(o.BigO, "n_orders") || len(o.Advice) == 0 {
				t.Errorf("gRPC ListUserOrders: want O(n_orders) and an index fix, got %s %+v", o.BigO, o.Advice)
			}
			if o := r["RPC Shop/GetUser"]; o.Status != "OK" || strings.Contains(strings.ReplaceAll(o.BigO, "log n_users", ""), "n_") {
				t.Errorf("gRPC GetUser: want OK and O(log n_users), got %s %s", o.Status, o.BigO)
			}
		})
	}
}
