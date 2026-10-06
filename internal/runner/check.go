package runner

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/auth"
	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/inputs"
	"github.com/nazishasghar/routeperf/internal/spec"
	"github.com/nazishasghar/routeperf/internal/version"
)

func mustURL(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		return &url.URL{}
	}
	return u
}

type Check struct {
	Name   string `json:"name"`
	Status string `json:"status"` // ok | warn | fail | skip
	Detail string `json:"detail"`
	Fix    string `json:"fix,omitempty"`
}

func (c Check) Failed() bool { return c.Status == "fail" }

// Doctor verifies every connection and prerequisite, preparing the runner as
// it goes. It never stops at the first problem unless later checks depend on it.
func (r *Runner) Doctor(ctx context.Context) []Check {
	c := r.cfg
	var out []Check
	add := func(name, status, detail, fix string) {
		out = append(out, Check{name, status, strings.Join(strings.Fields(detail), " "), fix})
	}
	if c.API.BaseURL == "" { // GraphQL / gRPC: the spec often is the server
		switch r.protocol() {
		case "grpc":
			if t, _ := r.grpcTarget(); t != "" {
				c.API.BaseURL = "grpc://" + t
			}
		case "graphql":
			c.API.BaseURL, _ = r.graphqlEndpoint()
		}
	}
	var missing []string
	if c.Spec == "" {
		missing = append(missing, "swagger/OpenAPI URL (--spec)")
	}
	if c.API.BaseURL == "" {
		missing = append(missing, "API base URL (--api-url)")
	}
	if c.DB.URL == "" {
		missing = append(missing, "database URL (--db-url)")
	}
	if len(missing) > 0 {
		add("Inputs", "fail", "missing: "+strings.Join(missing, ", "), "run `routeperf init` or pass the flags")
		return out
	}
	add("Inputs", "ok", "spec, API URL and DB URL provided", "")

	// --- auth manager + API reachability
	var err error
	if r.baseURL, err = url.Parse(strings.TrimRight(c.API.BaseURL, "/")); err != nil || r.baseURL.Host == "" {
		add("API URL", "fail", fmt.Sprintf("invalid API URL %q", c.API.BaseURL), "use a full URL like http://localhost:3000")
		return out
	}
	if r.am, err = auth.New(c.Auth, c.API.BaseURL, c.timeout()); err != nil {
		add("Auth config", "fail", err.Error(), "check cookie_jar path / auth settings")
		return out
	}
	apiUp := true
	if conn, err := net.DialTimeout("tcp", hostPort(r.baseURL), 3*time.Second); err != nil {
		apiUp = false
		add("API reachable", "fail", fmt.Sprintf("cannot connect to %s: %v", hostPort(r.baseURL), err), "start the API server, or fix --api-url")
	} else {
		conn.Close()
		add("API reachable", "ok", "TCP connect to "+hostPort(r.baseURL)+" succeeded", "")
	}
	if c.Auth.OAuth2 != nil || c.Auth.Login != nil || c.Auth.BearerCommand != "" {
		if !apiUp && c.Auth.Login != nil {
			add("Auth flow", "skip", "API not reachable", "")
		} else if err := r.am.Init(ctx); err != nil {
			add("Auth flow", "fail", err.Error(), "check login path/body or OAuth2 token_url/client credentials")
		} else {
			add("Auth flow", "ok", authFlowName(c.Auth)+" succeeded; token/cookies acquired", "")
		}
	}

	// --- spec
	if ok := r.loadSpec(ctx, add); !ok {
		return out
	}

	// --- security scheme coverage
	if len(r.sp.Schemes) > 0 {
		var parts []string
		allOK := true
		for name := range r.sp.Schemes {
			ok, _ := r.am.Satisfies(openapi3.SecurityRequirements{openapi3.SecurityRequirement{name: {}}}, r.sp.Schemes)
			mark := "✓"
			if !ok {
				mark = "✗"
				allOK = false
			}
			parts = append(parts, name+" "+mark)
		}
		st, fix := "ok", ""
		if !allOK {
			unsat := 0
			for _, o := range r.sp.Ops {
				reqs := r.sp.Global
				if o.Security != nil {
					reqs = *o.Security
				}
				if ok, _ := r.am.Satisfies(reqs, r.sp.Schemes); !ok {
					unsat++
				}
			}
			if unsat > 0 {
				st, fix = "warn", fmt.Sprintf("%d operation(s) need a ✗ scheme and will be skipped; add --token / -H / -b or auth.login", unsat)
			} else {
				parts[len(parts)-1] += " — every operation has a satisfied alternative"
			}
		}
		add("Auth coverage", st, strings.Join(parts, ", "), fix)
	}

	// --- DB
	if !c.DB.AllowRemote && !db.IsLocalHost(c.DB.URL) {
		add("DB host", "fail", "database host is not local", "routeperf runs EXPLAIN ANALYZE and write routes against the DB; only use disposable test DBs (--allow-remote-db to override)")
		return out
	}
	r.db, err = db.Open(ctx, c.DB.URL, db.Options{PGLogFile: c.Capture.PGLogFile, Schemas: c.DB.Schemas, NoSilence: c.Proxy()})
	if err != nil {
		add("DB connect", "fail", err.Error(), dbFix(c.DB.URL, err))
		return out
	}
	info := r.db.Info()
	add("DB connect", "ok", fmt.Sprintf("%s %s, database %q", info.Dialect, info.Version, info.Database), "")
	switch {
	case c.Proxy():
		add("DB privileges", "ok", "proxy capture: no admin rights needed (subsets need CREATE privilege)", "")
	case info.Dialect == "postgres" && !info.Super:
		add("DB privileges", "fail", "role is not superuser", "routeperf needs a superuser on the local DB to toggle statement logging (ALTER SYSTEM) and read the log; without one use --capture proxy")
	default:
		add("DB privileges", "ok", "can change logging settings", "")
	}
	r.cap = r.db
	if c.Proxy() {
		if err := r.startProxy(ctx); err != nil {
			add("SQL proxy", "fail", err.Error(), "pick a free port with --proxy-listen")
			return out
		}
		add("SQL proxy", "ok", "listening on "+c.Capture.ProxyListen+" → "+hostPort(mustURL(c.DB.URL))+"; the app must connect to "+r.ProxyURL(), "")
	}
	if r.tables, err = r.db.Tables(ctx); err != nil {
		add("DB catalog", "fail", err.Error(), "")
		return out
	}
	if r.fks, err = r.db.FKs(ctx, r.tables); err != nil {
		add("DB catalog", "fail", err.Error(), "")
		return out
	}
	var total, biggest float64
	bigName := ""
	for _, t := range r.tables {
		total += t.Rows
		if t.Rows > biggest {
			biggest, bigName = t.Rows, t.Name
		}
	}
	switch {
	case len(r.tables) == 0:
		add("Test data", "fail", "database has no tables", "point --db-url at the database your API uses, with bulk test data loaded")
	case biggest < 10000:
		add("Test data", "warn", fmt.Sprintf("%d tables, %.0f rows; largest %s has %.0f rows", len(r.tables), total, bigName, biggest),
			"growth (Big O) needs ≥10k rows in the main tables; load more bulk data for reliable results")
	default:
		add("Test data", "ok", fmt.Sprintf("%d tables, %.0f rows; largest %s has %.0f rows", len(r.tables), total, bigName, biggest), "")
	}
	declared, inferred := 0, 0
	for _, f := range r.fks {
		if f.Declared {
			declared++
		} else {
			inferred++
		}
	}
	add("Foreign keys", "ok", fmt.Sprintf("%d declared, %d inferred from *_id naming (used for consistent data subsets)", declared, inferred), "")

	fixtures := map[string]inputs.Fixture{}
	if b, err := os.ReadFile(c.Fixtures); err == nil {
		if err := yaml.Unmarshal(b, &fixtures); err != nil {
			add("Fixtures", "fail", fmt.Sprintf("%s: %v", c.Fixtures, err), "fix the YAML")
		} else {
			add("Fixtures", "ok", fmt.Sprintf("%d operations configured in %s", len(fixtures), c.Fixtures), "")
		}
	}
	r.res = inputs.New(r.db, r.tables, fixtures, c.Scale.KParams)
	r.res.FKs, r.res.Spec = r.fks, r.sp
	r.result = &Result{Tool: "routeperf", Version: version.String(), Started: time.Now(), Spec: c.Spec, SpecTitle: r.sp.Title, Protocol: r.protocol(), API: c.API.BaseURL,
		Dialect: info.Dialect, DBVersion: info.Version, Database: info.Database, Tables: map[string]float64{}, Snapshot: "none"}
	for n, t := range r.tables {
		r.result.Tables[n] = t.Rows
	}

	// --- explain works
	if _, err := r.db.Explain(ctx, analyze.Stmt{SQL: "SELECT 1", Kind: "select"}, db.Target{}); err != nil {
		add("EXPLAIN ANALYZE", "fail", err.Error(), "MySQL needs 8.0.18+; Postgres 13+")
	} else {
		add("EXPLAIN ANALYZE", "ok", "works on this server", "")
	}

	// --- capture + probe + does the app use this DB?
	if _, err := os.Stat(markerPath()); err == nil || r.db.HasSnapshot(ctx) {
		add("Previous run", "warn", "a previous run left pending state (marker or _rp_snap)", "run `routeperf repair` to restore settings/data")
	}
	if err := r.cap.StartCapture(ctx); err != nil {
		add("SQL capture", "fail", err.Error(), captureFix(info.Dialect))
	} else {
		if err := r.cap.Probe(ctx); err != nil {
			add("SQL capture", "fail", err.Error(), captureFix(info.Dialect))
		} else {
			add("SQL capture", "ok", "statements readable: "+r.cap.LogSource(), "")
			if n := r.idleProbe(ctx, time.Second); n > 0 {
				add("Quiet DB", "warn", fmt.Sprintf("%d statement(s) in 1s from other clients/jobs while idle; those query shapes will be excluded from endpoint numbers", n),
					"stop background workers/cron jobs during the run for the cleanest numbers")
			} else {
				add("Quiet DB", "ok", "no background SQL while idle", "")
			}
			if apiUp {
				add(r.appUsesDB(ctx))
			}
		}
		if err := r.cap.StopCapture(context.Background()); err != nil {
			add("Restore log settings", "fail", err.Error(), "run `routeperf repair`")
		}
	}

	// --- operation selection summary
	sel, skipped := r.Select()
	reasons := map[string]int{}
	for _, s := range skipped {
		reasons[s.Skipped]++
	}
	var rs []string
	for k, v := range reasons {
		rs = append(rs, fmt.Sprintf("%d %s", v, k))
	}
	st := "ok"
	if len(sel) == 0 {
		st = "fail"
	} else if len(skipped) > 0 {
		st = "warn"
	}
	detail := fmt.Sprintf("%d of %d operations will run", len(sel), len(r.sp.Ops))
	if len(rs) > 0 {
		detail += "; skipped: " + strings.Join(rs, ", ")
	}
	add("Operations", st, detail, "")
	if c.WritesEnabled() {
		add("Writes", "ok", fmt.Sprintf("enabled; snapshot strategy %q (data restored after the run)", c.Writes.Snapshot), "")
	}
	r.result.secrets = r.am.Secrets()
	return out
}

func (r *Runner) needsAuth(o *spec.Operation) bool {
	reqs := r.sp.Global
	if o.Security != nil {
		reqs = *o.Security
	}
	for _, alt := range reqs {
		if len(alt) == 0 {
			return false
		}
	}
	return len(reqs) > 0
}

// appUsesDB hits a few GET routes and checks that SQL shows up in the log.
func (r *Runner) appUsesDB(ctx context.Context) (string, string, string, string) {
	sel, _ := r.Select()
	tried, statements := 0, 0
	var codes []string
	authFail, authTried := 0, 0
	sort.SliceStable(sel, func(i, j int) bool { return r.needsAuth(sel[i]) && !r.needsAuth(sel[j]) })
	for _, o := range sel {
		if o.Phase != "R" || tried >= 4 {
			continue
		}
		req, err := r.res.Build(ctx, o, 0, -1, nil, nil)
		if err != nil {
			continue
		}
		tried++
		s, _, _ := r.request(ctx, req, 0)
		statements += s.NStmts
		codes = append(codes, fmt.Sprintf("%s %d", o.Path, s.Status))
		if r.needsAuth(o) {
			authTried++
			if s.Status == http.StatusUnauthorized || s.Status == http.StatusForbidden {
				authFail++
			}
		}
	}
	switch {
	case tried == 0:
		return "App → DB link", "skip", "no GET operations to probe", ""
	case authTried > 0 && authFail == authTried:
		return "App → DB link", "fail", "all probe requests were rejected: " + strings.Join(codes, ", "), "credentials are wrong or missing (--token / -H / -b / auth.login)"
	case statements == 0 && r.cfg.Proxy():
		return "App → DB link", "fail", "API answered (" + strings.Join(codes, ", ") + ") but no SQL came through the proxy",
			"point the app's database URL at " + r.ProxyURL() + " (same user and database) and restart it, or let its pool reconnect"
	case statements == 0:
		return "App → DB link", "fail", "API answered (" + strings.Join(codes, ", ") + ") but issued no SQL to this database",
			"make sure the API's own DATABASE_URL points at the same database as --db-url"
	}
	return "App → DB link", "ok", fmt.Sprintf("%d SQL statements captured from %d probe requests (%s)", statements, tried, strings.Join(codes, ", ")), ""
}

func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	if u.Scheme == "https" {
		return u.Hostname() + ":443"
	}
	return u.Hostname() + ":80"
}

func authFlowName(a auth.Config) string {
	if a.Login != nil {
		return "login " + a.Login.Path
	}
	return "OAuth2 client credentials"
}

func dbFix(raw string, err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "database server is not running on that host/port"
	case strings.Contains(s, "password authentication failed"), strings.Contains(s, "Access denied"):
		return "wrong DB user/password in --db-url"
	case strings.Contains(s, "does not exist"), strings.Contains(s, "Unknown database"):
		return "database name in --db-url does not exist"
	case strings.Contains(s, "unsupported db scheme"):
		return "use postgres://user:pass@localhost:5432/db or mysql://user:pass@127.0.0.1:3306/db"
	}
	return "check --db-url (" + redactURL(raw) + ")"
}

func captureFix(dialect string) string {
	if dialect == "postgres" {
		return "set capture.pg_log_file to the server's stderr log (Homebrew: /opt/homebrew/var/log/postgresql@<ver>.log) or enable logging_collector"
	}
	return "the MySQL user needs SYSTEM_VARIABLES_ADMIN (or SUPER) to enable general_log"
}

// Prepare runs the doctor and fails if any check failed.
func (r *Runner) Prepare(ctx context.Context) ([]Check, error) {
	checks := r.Doctor(ctx)
	var bad []string
	for _, ch := range checks {
		if ch.Failed() {
			bad = append(bad, ch.Name+": "+ch.Detail)
		}
	}
	if len(bad) > 0 {
		return checks, errors.New(strings.Join(bad, "; "))
	}
	return checks, nil
}
