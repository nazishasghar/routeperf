// Command routeperf: point it at an OpenAPI spec, a running API and its local
// database; it measures every route, replays the SQL with EXPLAIN ANALYZE and
// estimates Big O per route.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/report"
	"github.com/nazishasghar/routeperf/internal/runner"
	"github.com/nazishasghar/routeperf/internal/spec"
	"github.com/nazishasghar/routeperf/internal/version"
)

type flags struct {
	config, spec, api, dbURL, token, cookieJar, out, pgLog, planMode string
	headers, cookies, only, exclude                                  []string
	yes, noWrites, noScale, allowRemote, verbose, nonInter           bool
	ci                                                               bool
	iterations                                                       int
}

var f flags

func main() {
	root := &cobra.Command{
		Use:   "routeperf",
		Short: "Per-route performance and Big O estimates from an OpenAPI spec + EXPLAIN ANALYZE",
		Long: `routeperf calls every route of your API (from its Swagger/OpenAPI spec), captures the SQL
each route runs on your local Postgres/MySQL, replays it with EXPLAIN ANALYZE on nested
1%→100% data subsets and reports latency, DB time, queries/request and an estimated Big O.

Start with:  routeperf init   →   routeperf check   →   routeperf run`,
		Example: `  routeperf init
  routeperf check
  routeperf run --spec http://localhost:3000/swagger.json --api-url http://localhost:3000 \
                --db-url postgres://me@localhost:5432/app --token "$API_TOKEN"
  routeperf run --no-writes --only tag:orders
  routeperf run --ci -o perf-report`,
		Version:       version.String(),
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	defCfg := "routeperf.yaml"
	if v := os.Getenv("ROUTEPERF_CONFIG"); v != "" {
		defCfg = v
	}
	pf := root.PersistentFlags()
	pf.StringVarP(&f.config, "config", "c", defCfg, "config file (env ROUTEPERF_CONFIG)")
	pf.StringVar(&f.spec, "spec", "", "swagger/OpenAPI URL or file")
	pf.StringVar(&f.api, "api-url", "", "API base URL")
	pf.StringVar(&f.dbURL, "db-url", "", "database URL (postgres://… or mysql://…)")
	pf.StringVar(&f.token, "token", "", "bearer token")
	pf.StringArrayVarP(&f.headers, "header", "H", nil, "custom header 'Name: value' (repeatable)")
	pf.StringArrayVarP(&f.cookies, "cookie", "b", nil, "cookie 'name=value' (repeatable)")
	pf.StringVar(&f.cookieJar, "cookie-jar", "", "Netscape/curl cookie file")
	pf.StringVar(&f.pgLog, "pg-log-file", "", "Postgres stderr log path or docker:<container> (auto-detected)")
	pf.StringVar(&f.planMode, "pg-plan-mode", "", "Postgres replay plans: auto (match the app's prepared statements) | custom | generic")
	pf.BoolVar(&f.allowRemote, "allow-remote-db", false, "allow a non-local database (disposable test DBs only)")
	pf.BoolVar(&f.nonInter, "non-interactive", false, "never prompt")
	pf.BoolVarP(&f.verbose, "verbose", "v", false, "verbose logs")

	runCmd := &cobra.Command{Use: "run", Short: "Check connections, run every route, analyze and report", RunE: cmdRun}
	runCmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "don't ask before running write operations")
	runCmd.Flags().BoolVar(&f.noWrites, "no-writes", false, "only GET/HEAD operations")
	runCmd.Flags().BoolVar(&f.noScale, "no-scale", false, "skip data/output scale experiments (static Big O only)")
	runCmd.Flags().StringSliceVar(&f.only, "only", nil, "operationIds or tag:<name> to include")
	runCmd.Flags().StringSliceVar(&f.exclude, "exclude", nil, "operationIds or tag:<name> to exclude")
	runCmd.Flags().StringVarP(&f.out, "out", "o", "", "output directory (default routeperf-out)")
	runCmd.Flags().IntVarP(&f.iterations, "iterations", "n", 0, "timed requests per operation")
	runCmd.Flags().BoolVar(&f.ci, "ci", false, "exit 2 when any route FAILs")

	checkCmd := &cobra.Command{Use: "check", Short: "Verify spec, API, auth and database connections and say what is wrong", RunE: cmdCheck}
	discoverCmd := &cobra.Command{Use: "discover", Short: "List operations, inputs and auth mapping without running anything heavy", RunE: cmdDiscover}
	initCmd := &cobra.Command{Use: "init", Short: "Interactive setup: asks for spec, API, DB and auth, writes routeperf.yaml", RunE: cmdInit}
	repairCmd := &cobra.Command{Use: "repair", Short: "Undo leftovers of an interrupted run (log settings, snapshot, subsets)", RunE: cmdRepair}
	reportCmd := &cobra.Command{Use: "report <results.json>", Short: "Re-render report.md from results.json", Args: cobra.ExactArgs(1), RunE: cmdReport}
	versionCmd := &cobra.Command{Use: "version", Short: "Print the routeperf version", Run: func(*cobra.Command, []string) {
		fmt.Println("routeperf", version.String())
	}}
	root.AddCommand(runCmd, checkCmd, discoverCmd, initCmd, repairCmd, reportCmd, versionCmd)

	report.Color = isTTY(os.Stdout)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "\033[90m"+format+"\033[0m\n", a...)
}

// loadConfig merges file, flags and (when interactive) prompts for anything missing.
func loadConfig(ask bool) (*runner.Config, error) {
	cfg, err := runner.LoadConfig(f.config)
	if err != nil {
		return nil, err
	}
	if f.spec != "" {
		cfg.Spec = f.spec
	}
	if f.api != "" {
		cfg.API.BaseURL = f.api
	}
	if f.dbURL != "" {
		cfg.DB.URL = f.dbURL
	}
	if f.token != "" {
		cfg.Auth.Bearer = f.token
	}
	if f.cookieJar != "" {
		cfg.Auth.CookieJar = f.cookieJar
	}
	if f.pgLog != "" {
		cfg.Capture.PGLogFile = f.pgLog
	}
	if f.planMode != "" {
		cfg.Capture.PGPlanMode = f.planMode
	}
	for _, h := range f.headers {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("header %q must be 'Name: value'", h)
		}
		if cfg.Auth.Headers == nil {
			cfg.Auth.Headers = map[string]string{}
		}
		cfg.Auth.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	for _, ck := range f.cookies {
		for _, part := range strings.Split(ck, ";") {
			k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
			if !ok {
				continue
			}
			if cfg.Auth.Cookies == nil {
				cfg.Auth.Cookies = map[string]string{}
			}
			cfg.Auth.Cookies[k] = v
		}
	}
	if f.allowRemote {
		cfg.DB.AllowRemote = true
	}
	if f.out != "" {
		cfg.Out = f.out
	}
	if f.iterations > 0 {
		cfg.Run.Iterations = f.iterations
	}
	if f.noScale {
		cfg.Scale.Disabled = true
	}
	if len(f.only) > 0 {
		cfg.Run.Include = f.only
	}
	if len(f.exclude) > 0 {
		cfg.Run.Exclude = f.exclude
	}
	if f.noWrites {
		cfg.Run.Methods = []string{"GET", "HEAD"}
	}
	cfg.Yes, cfg.Verbose = f.yes, f.verbose
	cfg.Defaults()
	if ask && !f.nonInter && isTTY(os.Stdin) {
		p := newPrompter()
		missing := cfg.Spec == "" || cfg.API.BaseURL == "" || cfg.DB.URL == ""
		if missing {
			fmt.Fprintln(os.Stderr, "\nA few details are needed (you can save them to routeperf.yaml).")
			askCore(p, cfg, false)
			if !hasAuth(cfg) {
				askAuth(p, cfg)
			}
		}
		if missing && p.yes("Save these settings to "+f.config+"?", true) {
			if err := saveConfig(p, cfg, f.config); err != nil {
				return nil, err
			}
		}
	}
	return cfg, nil
}

func hasAuth(c *runner.Config) bool {
	a := c.Auth
	return a.Bearer != "" || len(a.Headers) > 0 || len(a.Cookies) > 0 || a.CookieJar != "" || len(a.Query) > 0 || a.OAuth2 != nil || a.Login != nil
}

func ctxWithSignals() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func doctor(ctx context.Context, r *runner.Runner) ([]runner.Check, bool) {
	fmt.Fprintln(os.Stderr, "\nChecking connections…")
	checks := r.Doctor(ctx)
	report.Checks(os.Stderr, checks)
	ok := true
	for _, c := range checks {
		if c.Failed() {
			ok = false
		}
	}
	return checks, ok
}

func cmdCheck(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()
	r := runner.New(cfg, logf)
	defer r.Close()
	_, ok := doctor(ctx, r)
	if !ok {
		fmt.Fprintln(os.Stderr, "\n✗ Not ready — fix the items above and run `routeperf check` again.")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "\n✓ Everything checks out. Run `routeperf run`.")
	return nil
}

func cmdRun(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()
	r := runner.New(cfg, logf)
	defer r.Close()
	if _, ok := doctor(ctx, r); !ok {
		fmt.Fprintln(os.Stderr, "\n✗ Not running — fix the failed checks above first.")
		os.Exit(1)
	}
	if cfg.WritesEnabled() && !cfg.Yes {
		if isTTY(os.Stdin) && !f.nonInter {
			fmt.Fprintln(os.Stderr, "\nWrite operations (POST/PUT/PATCH/DELETE) will change data while routeperf runs.")
			fmt.Fprintln(os.Stderr, "routeperf snapshots your tables first and restores the touched ones afterwards.")
			if !newPrompter().yes("Run write operations?", false) {
				cfg.Run.Methods = []string{"GET", "HEAD"}
			}
		} else {
			fmt.Fprintln(os.Stderr, "write operations skipped (non-interactive without --yes)")
			cfg.Run.Methods = []string{"GET", "HEAD"}
		}
	}
	fmt.Fprintln(os.Stderr)
	res, err := r.Run(ctx)
	if err != nil {
		return err
	}
	report.Terminal(os.Stdout, res)
	files, err := report.WriteAll(cfg.Out, res)
	if err != nil {
		return err
	}
	fmt.Printf("\nReports: %s\n", strings.Join(files, ", "))
	if f.ci {
		for _, o := range res.Ops {
			if o.Status == "FAIL" {
				os.Exit(2)
			}
		}
	}
	return nil
}

func cmdDiscover(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	ctx, cancel := ctxWithSignals()
	defer cancel()
	r := runner.New(cfg, logf)
	defer r.Close()
	if _, ok := doctor(ctx, r); !ok && r.Spec() == nil {
		os.Exit(1)
	}
	sel, skipped := r.Select()
	fmt.Printf("\n%d operations will run:\n", len(sel))
	for _, o := range sel {
		fmt.Printf("  %-6s %-40s %s\n", o.Method, o.Path, o.ID)
	}
	if len(skipped) > 0 {
		fmt.Printf("\n%d skipped:\n", len(skipped))
		for _, s := range skipped {
			fmt.Printf("  %-6s %-40s %s\n", s.Method, s.Path, s.Skipped)
		}
	}
	return nil
}

func cmdReport(cmd *cobra.Command, args []string) error {
	b, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	var res runner.Result
	if err := json.Unmarshal(b, &res); err != nil {
		return err
	}
	report.Terminal(os.Stdout, &res)
	out := strings.TrimSuffix(args[0], ".json") + ".md"
	if err := report.WriteMarkdown(out, &res); err != nil {
		return err
	}
	fmt.Println("\nwrote", out)
	return nil
}

func cmdRepair(cmd *cobra.Command, _ []string) error {
	cfg, err := loadConfig(true)
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, cfg.DB.URL, db.Options{PGLogFile: cfg.Capture.PGLogFile})
	if err != nil {
		return err
	}
	defer d.Close()
	if err := d.StartCapture(ctx); err == nil { // records current values, then resets to them
		_ = d.StopCapture(ctx)
	}
	if d.Info().Dialect == "postgres" {
		for _, p := range []string{"log_min_duration_statement", "log_parameter_max_length", "log_line_prefix"} {
			fmt.Println("note: if routeperf crashed mid-run, reset", p, "with: ALTER SYSTEM RESET", p+"; SELECT pg_reload_conf();")
		}
	}
	if d.HasSnapshot(ctx) {
		tables, err := d.Tables(ctx)
		if err != nil {
			return err
		}
		var ts []*db.Table
		for _, t := range tables {
			ts = append(ts, t)
		}
		fmt.Printf("restoring %d tables from %s…\n", len(ts), db.SnapNS)
		if err := d.Restore(ctx, ts); err != nil {
			return err
		}
		_ = d.DropNamespace(ctx, db.SnapNS)
	}
	for _, s := range []float64{0.01, 0.03, 0.1, 0.3} {
		_ = d.DropNamespace(ctx, db.NSName(s))
	}
	_ = os.Remove(".routeperf/pending.json")
	fmt.Println("repair done")
	return nil
}

func cmdInit(cmd *cobra.Command, _ []string) error {
	cfg, err := runner.LoadConfig(f.config)
	if err != nil {
		return err
	}
	if !isTTY(os.Stdin) {
		if err := os.WriteFile(f.config, []byte(runner.SampleConfig), 0o600); err != nil {
			return err
		}
		fmt.Println("wrote sample", f.config)
		return nil
	}
	cfg.Defaults()
	p := newPrompter()
	fmt.Fprintln(os.Stderr, "routeperf setup — press Enter to accept [defaults].")
	askCore(p, cfg, true)
	askAuth(p, cfg)
	w := p.yes("Run write operations (POST/PUT/PATCH/DELETE)? Data is snapshotted and restored.", true)
	cfg.Writes.Enabled = &w
	if err := saveConfig(p, cfg, f.config); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "\nNext: `routeperf check` to verify connections, then `routeperf run`.")
	return nil
}

// ------------------------------------------------------------ prompts

func askCore(p *prompter, cfg *runner.Config, all bool) {
	if cfg.Spec == "" || all {
		cfg.Spec = p.ask("Swagger / OpenAPI URL or file", cfg.Spec)
	}
	if cfg.API.BaseURL == "" || all {
		def := cfg.API.BaseURL
		if def == "" {
			def = guessServer(cfg.Spec)
		}
		cfg.API.BaseURL = p.ask("API base URL", def)
	}
	if cfg.DB.URL == "" || all {
		cfg.DB.URL = p.ask("Database URL (postgres://user:pass@localhost:5432/db or mysql://user:pass@127.0.0.1:3306/db)", cfg.DB.URL)
	}
}

func guessServer(src string) string {
	var data []byte
	if strings.HasPrefix(src, "http") {
		cl := &http.Client{Timeout: 5 * time.Second}
		resp, err := cl.Get(src)
		if err == nil {
			data, _ = io.ReadAll(resp.Body)
			resp.Body.Close()
		}
	} else {
		data, _ = os.ReadFile(src)
	}
	if len(data) > 0 {
		if s, err := spec.Load(context.Background(), data); err == nil && len(s.Doc.Servers) > 0 {
			u := s.Doc.Servers[0].URL
			if strings.HasPrefix(u, "http") {
				return u
			}
		}
	}
	if i := strings.Index(src, "://"); i > 0 {
		rest := src[i+3:]
		if j := strings.Index(rest, "/"); j > 0 {
			return src[:i+3+j]
		}
	}
	return "http://localhost:3000"
}

func askAuth(p *prompter, cfg *runner.Config) {
	fmt.Fprintln(os.Stderr, "\nHow does the API authenticate? (comma-separated numbers)")
	opts := []string{"Bearer token", "Cookies", "Custom headers (API key, tenant…)", "Login endpoint (returns token and/or cookie)", "OAuth2 client credentials", "None"}
	for i, o := range opts {
		fmt.Fprintf(os.Stderr, "  %d) %s\n", i+1, o)
	}
	for _, c := range p.choose("Choice", "6", len(opts)) {
		switch c {
		case 1:
			cfg.Auth.Bearer = p.secret("Bearer token")
		case 2:
			raw := p.ask("Cookies (name=value; name2=value2) or path to a cookie-jar file", "")
			if _, err := os.Stat(raw); err == nil {
				cfg.Auth.CookieJar = raw
			} else {
				cfg.Auth.Cookies = map[string]string{}
				for _, part := range strings.Split(raw, ";") {
					if k, v, ok := strings.Cut(strings.TrimSpace(part), "="); ok {
						cfg.Auth.Cookies[k] = v
					}
				}
			}
		case 3:
			cfg.Auth.Headers = map[string]string{}
			fmt.Fprintln(os.Stderr, "  Enter headers as 'Name: value', empty line to finish.")
			for {
				h := p.ask("  header", "")
				if h == "" {
					break
				}
				if k, v, ok := strings.Cut(h, ":"); ok {
					cfg.Auth.Headers[strings.TrimSpace(k)] = strings.TrimSpace(v)
				}
			}
		case 4:
			cfg.Auth.Login = &authLogin{}
			cfg.Auth.Login.Method = strings.ToUpper(p.ask("  Login method", "POST"))
			cfg.Auth.Login.Path = p.ask("  Login path", "/auth/login")
			body := p.ask(`  Login JSON body (e.g. {"email":"me@x.dev","password":"…"})`, "")
			var m map[string]any
			if err := json.Unmarshal([]byte(body), &m); err == nil {
				cfg.Auth.Login.Body = m
			} else if body != "" {
				fmt.Fprintln(os.Stderr, "  (not valid JSON — login body left empty)")
			}
			cfg.Auth.Login.Extract.Cookies = true
			cfg.Auth.Login.Extract.BearerFrom = p.ask("  JSON path of the token in the response (empty = cookie session)", "")
			cfg.Auth.Login.RefreshOn = []int{401}
		case 5:
			cfg.Auth.OAuth2 = &authOAuth{}
			cfg.Auth.OAuth2.TokenURL = p.ask("  Token URL", "/oauth/token")
			cfg.Auth.OAuth2.ClientID = p.ask("  Client ID", "")
			cfg.Auth.OAuth2.ClientSecret = p.secret("  Client secret")
			cfg.Auth.OAuth2.Scope = p.ask("  Scope", "")
		}
	}
}

// saveConfig writes routeperf.yaml; secrets go to env placeholders unless the user opts in.
func saveConfig(p *prompter, cfg *runner.Config, path string) error {
	c := *cfg
	keep := p.yes("Store secrets (tokens, passwords, cookies) in the file? (No = use env vars)", false)
	var exports []string
	if !keep {
		env := func(name, val string) string {
			if val == "" {
				return ""
			}
			exports = append(exports, fmt.Sprintf("export %s='%s'", name, strings.ReplaceAll(val, "'", `'\''`)))
			return "${" + name + "}"
		}
		c.Auth.Bearer = env("RP_BEARER", c.Auth.Bearer)
		if len(c.Auth.Headers) > 0 {
			h := map[string]string{}
			for k, v := range c.Auth.Headers {
				h[k] = env("RP_HEADER_"+envName(k), v)
			}
			c.Auth.Headers = h
		}
		if len(c.Auth.Cookies) > 0 {
			ck := map[string]string{}
			for k, v := range c.Auth.Cookies {
				ck[k] = env("RP_COOKIE_"+envName(k), v)
			}
			c.Auth.Cookies = ck
		}
		if c.Auth.OAuth2 != nil {
			o := *c.Auth.OAuth2
			o.ClientSecret = env("RP_CLIENT_SECRET", o.ClientSecret)
			c.Auth.OAuth2 = &o
		}
		if c.Auth.Login != nil {
			l := *c.Auth.Login
			b := map[string]any{}
			for k, v := range l.Body {
				if strings.Contains(strings.ToLower(k), "pass") || strings.Contains(strings.ToLower(k), "secret") {
					b[k] = env("RP_LOGIN_"+envName(k), fmt.Sprint(v))
				} else {
					b[k] = v
				}
			}
			l.Body = b
			c.Auth.Login = &l
		}
		if u := c.DB.URL; strings.Contains(u, "@") && strings.Contains(u[:strings.Index(u, "@")], ":") {
			c.DB.URL = env("RP_DB_URL", u)
		}
	}
	b, err := yaml.Marshal(&c)
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "saved %s\n", path)
	if len(exports) > 0 {
		sort.Strings(exports)
		fmt.Fprintln(os.Stderr, "Set these environment variables before running (e.g. in your shell profile or a .env you source):")
		for _, e := range exports {
			fmt.Fprintln(os.Stderr, "  "+e)
		}
		for _, e := range exports { // current process keeps working
			kv := strings.SplitN(strings.TrimPrefix(e, "export "), "=", 2)
			_ = os.Setenv(kv[0], strings.Trim(kv[1], "'"))
		}
	}
	return nil
}

func envName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(s) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return b.String()
}
