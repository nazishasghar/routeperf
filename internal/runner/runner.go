// Package runner drives a full routeperf run: discover → capture → replay →
// scale experiments → analysis → restore.
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/auth"
	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/inputs"
	"github.com/nazishasghar/routeperf/internal/spec"
)

type Result struct {
	Tool        string              `json:"tool"`
	Version     string              `json:"version,omitempty"`
	Started     time.Time           `json:"started"`
	Seconds     float64             `json:"seconds"`
	Spec        string              `json:"spec"`
	SpecTitle   string              `json:"spec_title"`
	Protocol    string              `json:"protocol,omitempty"`
	API         string              `json:"api"`
	Dialect     string              `json:"dialect"`
	DBVersion   string              `json:"db_version"`
	Database    string              `json:"database"`
	Capture     string              `json:"capture,omitempty"`
	DBTime      string              `json:"db_time_source,omitempty"` // server log | performance_schema | proxy | replay estimate
	Cache       string              `json:"cache,omitempty"`          // warm | warm+cold
	Correlated  bool                `json:"traceparent_correlation,omitempty"`
	Masked      bool                `json:"literals_masked,omitempty"`
	Tables      map[string]float64  `json:"tables"`
	DataSteps   []float64           `json:"data_steps"`
	Ops         []*analyze.OpResult `json:"operations"`
	Skipped     []*analyze.OpResult `json:"skipped"`
	Snapshot    string              `json:"snapshot"`
	Restored    []string            `json:"restored_tables,omitempty"`
	Verified    bool                `json:"restore_verified"`
	AdviceProof string              `json:"advice_proof,omitempty"` // hypopg | unavailable
	ColdMethod  string              `json:"cold_method,omitempty"`
	Warnings    []string            `json:"warnings,omitempty"`
	Unresolved  map[string][]string `json:"unresolved_params,omitempty"`
	secrets     []string
}

// Secrets returns credential values to redact from any output.
func (r *Result) Secrets() []string { return r.secrets }

// SetSecrets is used when re-rendering a saved result.
func (r *Result) SetSecrets(s []string) { r.secrets = s }

type Runner struct {
	cfg      *Config
	log      func(format string, a ...any)
	db       db.DB
	cap      db.Capture
	closers  []func()
	am       *auth.Manager
	sp       *spec.Spec
	res      *inputs.Resolver
	tables   map[string]*db.Table
	fks      []db.FK
	counts   map[float64]map[string]float64 // step → table → rows
	ptCounts map[float64]map[string]float64 // per-table shrink step → table → rows
	built    map[string]bool                // tables with subsets
	created  map[string][]string            // collection path → created IDs
	baseURL  *url.URL
	result   *Result
	nsSteps  []float64
	bg       map[string]string // fingerprints seen while no request was in flight
	corr     bool              // statements carry our traceparent: attribute by trace id
	tr       transport
}

// transport sends a built request (HTTP/OpenAPI, GraphQL or gRPC).
type transport interface {
	Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error)
	Close()
}

// idleProbe watches the statement log while routeperf sends nothing; any SQL
// seen comes from background jobs or other clients and is excluded later.
func (r *Runner) idleProbe(ctx context.Context, d time.Duration) int {
	m0, err := r.cap.Mark(ctx)
	if err != nil {
		return 0
	}
	time.Sleep(d)
	m1, _ := r.cap.Mark(ctx)
	stmts, _ := r.cap.Window(ctx, m0, m1)
	if r.bg == nil {
		r.bg = map[string]string{}
	}
	for _, st := range stmts {
		if st.Kind != "other" && st.TraceID == "" {
			r.bg[st.Fingerprint] = st.SQL
		}
	}
	return len(stmts)
}

func New(cfg *Config, logf func(string, ...any)) *Runner {
	cfg.Defaults()
	return &Runner{cfg: cfg, log: logf, counts: map[float64]map[string]float64{}, ptCounts: map[float64]map[string]float64{},
		built: map[string]bool{}, created: map[string][]string{}}
}

func (r *Runner) fetch(ctx context.Context, src string) ([]byte, error) {
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		req, _ := http.NewRequestWithContext(ctx, "GET", src, nil)
		if r.am != nil {
			r.am.Apply(ctx, req)
		}
		resp, err := r.am.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("GET %s: %s", src, resp.Status)
		}
		return io.ReadAll(resp.Body)
	}
	return os.ReadFile(src)
}

func (r *Runner) Close() {
	for i := len(r.closers) - 1; i >= 0; i-- {
		r.closers[i]()
	}
	if r.tr != nil {
		r.tr.Close()
	}
	if r.db != nil {
		r.db.Close()
	}
}

func (r *Runner) Spec() *spec.Spec { return r.sp }

// Select filters operations and returns (selected, skipped).
func (r *Runner) Select() ([]*spec.Operation, []*analyze.OpResult) {
	c := r.cfg
	allowed := map[string]bool{}
	for _, m := range c.Run.Methods {
		allowed[m] = true
	}
	match := func(list []string, o *spec.Operation) bool {
		for _, x := range list {
			if strings.HasPrefix(x, "tag:") {
				for _, t := range o.Tags {
					if strings.EqualFold(t, x[4:]) {
						return true
					}
				}
			} else if strings.EqualFold(x, o.ID) || strings.EqualFold(x, o.Method+" "+o.Path) {
				return true
			}
		}
		return false
	}
	var sel []*spec.Operation
	var skip []*analyze.OpResult
	for _, o := range r.sp.Ops {
		reason := ""
		reqs := r.sp.Global
		if o.Security != nil {
			reqs = *o.Security
		}
		method := o.Method
		if o.Protocol != "" && o.Protocol != "http" { // GraphQL / gRPC: map to read/write
			method = map[string]string{"R": "GET", "W": "POST"}[o.Phase]
		}
		switch {
		case !allowed[method]:
			reason = "method not enabled"
		case len(c.Run.Include) > 0 && !match(c.Run.Include, o):
			reason = "not included"
		case match(c.Run.Exclude, o):
			reason = "excluded"
		case spec.Dangerous(o) && !match(c.Run.DangerousOps, o):
			reason = "dangerous operation (list it in run.dangerous_ops to run)"
		case o.Phase == "W" && !c.WritesEnabled():
			reason = "writes disabled"
		case o.Unsupported != "":
			reason = o.Unsupported
		}
		if reason == "" && r.am != nil && r.sp.Schemes != nil {
			if ok, why := r.am.Satisfies(reqs, r.sp.Schemes); !ok {
				reason = why
			}
		}
		if reason != "" {
			skip = append(skip, &analyze.OpResult{ID: o.ID, Method: o.Method, Path: o.Path, Phase: o.Phase, Skipped: reason})
			continue
		}
		sel = append(sel, o)
	}
	return sel, skip
}

// ------------------------------------------------------------ pending marker

type pending struct {
	DBURL    string   `json:"db_url"`
	Capture  bool     `json:"capture"`
	Snapshot bool     `json:"snapshot"`
	Subsets  []string `json:"subsets"`
}

func markerPath() string { return filepath.Join(".routeperf", "pending.json") }

func writeMarker(p pending) {
	_ = os.MkdirAll(".routeperf", 0o755)
	b, _ := json.MarshalIndent(p, "", "  ")
	_ = os.WriteFile(markerPath(), b, 0o600)
}

// ------------------------------------------------------------ HTTP

func (r *Runner) send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	if r.tr != nil {
		return r.tr.Send(ctx, req)
	}
	return r.sendHTTP(ctx, req)
}

func (r *Runner) sendHTTP(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	u := *r.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + req.Path
	u.RawQuery = req.Query.Encode()
	raw, ctype, err := inputs.Encode(req)
	if err != nil {
		return 0, nil, nil, 0, err
	}
	for attempt := 0; attempt < 2; attempt++ {
		var body io.Reader
		if raw != nil {
			body = strings.NewReader(string(raw))
		}
		hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
		if err != nil {
			return 0, nil, nil, 0, err
		}
		if raw != nil {
			hr.Header.Set("Content-Type", ctype)
		}
		hr.Header.Set("Accept", "application/json")
		r.am.Apply(ctx, hr)
		for k, v := range req.Header {
			hr.Header.Set(k, v)
		}
		t0 := time.Now()
		resp, err := r.am.Client.Do(hr)
		if err != nil {
			return 0, nil, nil, 0, err
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ms := float64(time.Since(t0).Microseconds()) / 1000
		if attempt == 0 && r.am.Refresh(ctx, resp.StatusCode) {
			continue
		}
		return resp.StatusCode, b, resp.Header, ms, nil
	}
	return 0, nil, nil, 0, errors.New("unreachable")
}

func newTrace() (string, string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b[:16]), hex.EncodeToString(b[16:])
}

// request sends one request and captures the SQL it caused.
func (r *Runner) request(ctx context.Context, req *inputs.Request, k int) (analyze.Sample, []byte, http.Header) {
	s := analyze.Sample{K: k}
	trace := ""
	if r.cfg.TraceOn() {
		var span string
		trace, span = newTrace()
		if req.Header == nil {
			req.Header = map[string]string{}
		}
		req.Header["traceparent"] = "00-" + trace + "-" + span + "-01"
	}
	m0, err := r.cap.Mark(ctx)
	if err != nil {
		s.Err = err.Error()
		return s, nil, nil
	}
	status, body, hdr, ms, err := r.send(ctx, req)
	if err != nil {
		s.Err = err.Error()
		return s, nil, nil
	}
	settle := r.cfg.settle()
	if strings.HasPrefix(r.cap.LogSource(), "docker") && settle < 150*time.Millisecond {
		settle = 150 * time.Millisecond // docker log stream lag
	}
	time.Sleep(settle)
	m1, _ := r.cap.Mark(ctx)
	stmts, err := r.cap.Window(ctx, m0, m1)
	if err != nil {
		s.Err = "capture: " + err.Error()
	}
	stmts = r.attribute(stmts, trace, &s)
	s.Status, s.Ms, s.Bytes, s.Stmts, s.NStmts, s.Page = status, ms, len(body), stmts, len(stmts), req.Page
	return s, body, hdr
}

// attribute keeps the statements this request caused: SQL tagged with our
// trace id (sqlcommenter) is ours, SQL tagged with another id isn't, and
// untagged SQL is ours unless its shape was seen while idle.
func (r *Runner) attribute(stmts []analyze.Stmt, trace string, s *analyze.Sample) []analyze.Stmt {
	kept := stmts[:0]
	for _, st := range stmts {
		if st.TraceID != "" && trace != "" {
			if st.TraceID == trace {
				r.corr = true
				kept = append(kept, st)
			} else {
				s.Noise++
			}
			continue
		}
		if _, isBG := r.bg[st.Fingerprint]; isBG && len(r.bg) > 0 {
			s.Noise++
			continue
		}
		kept = append(kept, st)
	}
	return kept
}

var idKeys = regexp.MustCompile(`(?i)^(id|_id|uuid|\w+_?id)$`)

func extractID(body []byte, hdr http.Header) string {
	var v any
	if json.Unmarshal(body, &v) == nil {
		for _, path := range []string{"id", "ID", "_id", "uuid", "data.id", "data.ID", "result.id", "item.id"} {
			if x, ok := auth.JSONPath(v, path); ok {
				if _, isObj := x.(map[string]any); !isObj && x != nil {
					return jsonScalar(x)
				}
			}
		}
		if m, ok := v.(map[string]any); ok {
			if d, ok := m["data"].(map[string]any); ok && len(d) == 1 { // GraphQL: {"data":{"createUser":{"id":…}}}
				for _, x := range d {
					if obj, ok := x.(map[string]any); ok && obj["id"] != nil {
						return jsonScalar(obj["id"])
					}
				}
			}
			for k, x := range m {
				if idKeys.MatchString(k) && strings.HasSuffix(strings.ToLower(k), "id") {
					return jsonScalar(x)
				}
			}
		}
	}
	if loc := hdr.Get("Location"); loc != "" {
		return loc[strings.LastIndex(loc, "/")+1:]
	}
	return ""
}

func jsonScalar(x any) string {
	if f, ok := x.(float64); ok && f == math.Trunc(f) {
		return fmt.Sprintf("%.0f", f)
	}
	return fmt.Sprint(x)
}

// ------------------------------------------------------------ Run

func (r *Runner) Run(ctx context.Context) (*Result, error) {
	c := r.cfg
	ops, skipped := r.Select()
	r.result.Skipped = skipped
	var reads, writes []*spec.Operation
	for _, o := range ops {
		if o.Phase == "R" {
			reads = append(reads, o)
		} else {
			writes = append(writes, o)
		}
	}
	writes = spec.OrderWrites(writes)
	r.log("%d operations selected (%d read, %d write), %d skipped", len(ops), len(reads), len(writes), len(skipped))

	mk := pending{DBURL: redactURL(c.DB.URL), Capture: !c.Proxy()}
	writeMarker(mk)
	if err := r.cap.StartCapture(ctx); err != nil {
		return nil, fmt.Errorf("start capture: %w", err)
	}
	captureOn := true
	stopCapture := func() {
		if captureOn {
			if err := r.cap.StopCapture(context.Background()); err != nil {
				r.result.Warnings = append(r.result.Warnings, "restoring log settings failed: "+err.Error())
			}
			captureOn = false
		}
	}
	defer stopCapture()
	r.result.Capture = r.cap.LogSource()
	r.result.DBTime = r.timingSource()

	if n := r.idleProbe(ctx, 1500*time.Millisecond); n > 0 {
		r.log("background SQL detected while idle (%d statements); those query shapes are excluded", n)
	}
	var results []*analyze.OpResult
	for _, o := range reads {
		if ctx.Err() != nil {
			break
		}
		r.idleProbe(ctx, 250*time.Millisecond)
		results = append(results, r.runOp(ctx, o))
	}

	// Subsets come from pristine data, before any write.
	scaleSteps := []float64{}
	for _, s := range c.Scale.DataSteps {
		if s < 1 && !c.Scale.Disabled {
			scaleSteps = append(scaleSteps, s)
		}
	}
	r.nsSteps = scaleSteps
	r.result.DataSteps = c.Scale.DataSteps
	defer r.dropSubsets()
	if len(scaleSteps) > 0 {
		mk.Subsets = nsNames(scaleSteps)
		writeMarker(mk)
		r.buildSubsets(ctx, results)
	}

	snapshot := false
	var c0 map[string]float64
	if len(writes) > 0 && ctx.Err() == nil {
		if c.Writes.Snapshot == "tables" {
			var all []*db.Table
			for _, t := range r.tables {
				all = append(all, t)
			}
			r.log("snapshot: copying %d tables to %s", len(all), db.SnapNS)
			if err := r.db.Snapshot(ctx, all); err != nil {
				r.result.Warnings = append(r.result.Warnings, "snapshot failed, write operations skipped: "+err.Error())
				for _, o := range writes {
					r.result.Skipped = append(r.result.Skipped, &analyze.OpResult{ID: o.ID, Method: o.Method, Path: o.Path, Phase: "W", Skipped: "snapshot failed"})
				}
				writes = nil
			} else {
				snapshot = true
				r.result.Snapshot = "tables"
				mk.Snapshot = true
				writeMarker(mk)
			}
		}
		c0, _ = r.db.Counters(ctx)
		for _, o := range writes {
			if ctx.Err() != nil {
				break
			}
			r.idleProbe(ctx, 250*time.Millisecond)
			res := r.runWrite(ctx, o, writes)
			if res != nil {
				results = append(results, res)
			}
		}
	}
	if c.Cache.Cold && ctx.Err() == nil {
		r.coldPhase(ctx, results)
	}
	if c.Load.Enabled && ctx.Err() == nil {
		r.loadPhase(ctx, results, ops)
	}
	var touched []string
	if snapshot {
		time.Sleep(1500 * time.Millisecond) // PG flushes table stats lazily
		c1, _ := r.db.Counters(ctx)
		set := map[string]bool{}
		for t, v := range c1 {
			if v > c0[t] {
				set[t] = true
			}
		}
		for _, res := range results {
			if res.Phase != "W" && !c.Load.Writes {
				continue
			}
			for _, s := range append(res.Samples, flatten(res.KSamples)...) {
				for _, st := range s.Stmts {
					if st.Kind == "insert" || st.Kind == "update" || st.Kind == "delete" {
						if st.Target != "" {
							set[st.Target] = true
						}
					}
				}
			}
		}
		for t := range set {
			if tb := r.tables[t]; tb != nil && tb.HasData() {
				touched = append(touched, t)
			}
		}
		sort.Strings(touched)
	}
	stopCapture()

	if len(scaleSteps) > 0 && ctx.Err() == nil {
		r.buildSubsets(ctx, results) // tables first seen in write ops
	}
	r.log("replaying queries with EXPLAIN ANALYZE")
	for _, res := range results {
		if ctx.Err() != nil {
			break
		}
		r.replay(ctx, res)
	}
	if c.PerTable() && len(r.nsSteps) > 0 && ctx.Err() == nil {
		r.perTable(ctx, results)
	}

	if snapshot {
		var ts []*db.Table
		for _, t := range touched {
			ts = append(ts, r.tables[t])
		}
		r.log("restoring %d touched tables: %s", len(ts), strings.Join(touched, ", "))
		if err := r.db.Restore(context.Background(), ts); err != nil {
			r.result.Warnings = append(r.result.Warnings, "RESTORE FAILED (run `routeperf repair`): "+err.Error())
		} else {
			r.result.Restored = touched
			r.result.Verified = r.verifyRestore(ts)
			if !r.result.Verified {
				r.result.Warnings = append(r.result.Warnings, "row counts after restore differ from snapshot")
			}
			_ = r.db.DropNamespace(context.Background(), db.SnapNS)
		}
	}

	r.analyzeAll(ctx, results)
	if len(r.bg) > 0 {
		var shapes []string
		for _, q := range r.bg {
			shapes = append(shapes, truncateSQL(q, 80))
		}
		sort.Strings(shapes)
		noise := 0
		for _, res := range results {
			for _, s := range append(res.Samples, flatten(res.KSamples)...) {
				noise += s.Noise
			}
		}
		r.result.Warnings = append(r.result.Warnings, fmt.Sprintf("background SQL from other clients/jobs was detected; %d statement(s) of these shapes were excluded from endpoint numbers: %s", noise, strings.Join(shapes, " | ")))
	}
	r.result.Correlated = r.corr
	r.result.Cache = "warm"
	if c.Cache.Cold {
		r.result.Cache = "warm+cold"
	}
	r.result.Ops = results
	r.result.Unresolved = r.res.Unresolved
	misses := make([]string, 0, len(r.res.FixtureMisses))
	for where := range r.res.FixtureMisses {
		misses = append(misses, where)
	}
	sort.Strings(misses)
	for _, where := range misses {
		r.result.Warnings = append(r.result.Warnings, fmt.Sprintf("%s fixture %s found no value (%s); a sampled or generated value was used instead",
			c.Fixtures, where, r.res.FixtureMisses[where]))
	}
	r.result.Seconds = time.Since(r.result.Started).Seconds()
	if !snapshot || len(r.result.Warnings) == 0 || r.result.Verified {
		_ = os.Remove(markerPath())
	}
	return r.result, nil
}

func (r *Runner) timingSource() string {
	if ts, ok := r.cap.(interface{ TimingSource() string }); ok {
		return ts.TimingSource()
	}
	if r.db.Info().Dialect == "postgres" {
		return "server log"
	}
	return "replay estimate"
}

// analyzeAll turns measurements into verdicts and verifies index advice.
func (r *Runner) analyzeAll(ctx context.Context, results []*analyze.OpResult) {
	c := r.cfg
	unindexed := map[string][]string{}
	for _, f := range r.fks {
		if f.Declared && !f.Indexed {
			unindexed[f.Parent] = append(unindexed[f.Parent], f.Child+"|"+strings.Join(f.ChildCols, ","))
		}
	}
	tableRows := map[string]float64{}
	colSet := map[string]map[string]bool{}
	idxSet := map[string]map[string][]string{}
	for n, t := range r.tables {
		tableRows[n] = t.Rows
		colSet[n] = map[string]bool{}
		for _, cn := range t.Cols {
			colSet[n][strings.ToLower(cn)] = true
		}
		idxSet[n] = t.Indexes
	}
	adviseCtx := analyze.AdviseCtx{Dialect: r.db.Info().Dialect, TableRows: tableRows, UnindexedFK: unindexed, Cols: colSet, Indexes: idxSet}
	for _, res := range results {
		for _, q := range res.Queries {
			if (q.Kind == "delete" || q.Kind == "update") && q.Example.Target != "" && len(unindexed[q.Example.Target]) > 0 {
				child, _, _ := strings.Cut(unindexed[q.Example.Target][0], "|")
				q.Notes = append(q.Notes, "fk-child:"+child)
			}
			analyze.AnalyzeQuery(q, tableRows)
		}
		analyze.AnalyzeRoute(res, c.Thresholds.P95Ms, c.Thresholds.MaxQueries)
		analyze.Advise(res, adviseCtx)
	}
	if c.Verify() {
		r.verifyAdvice(ctx, results)
	}
}

func (r *Runner) verifyRestore(ts []*db.Table) bool {
	ok := true
	for _, t := range ts {
		a, _ := r.db.FirstValues(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s", t.SQL))
		b, _ := r.db.FirstValues(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", db.SnapNS, r.db.Quote(t.NSName())))
		if len(a) == 0 || len(b) == 0 || a[0] != b[0] {
			ok = false
		}
	}
	return ok
}

func truncateSQL(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func flatten(m map[int][]analyze.Sample) []analyze.Sample {
	var out []analyze.Sample
	ks := make([]int, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	for _, k := range ks {
		out = append(out, m[k]...)
	}
	return out
}

func nsNames(steps []float64) []string {
	var out []string
	for _, s := range steps {
		out = append(out, db.NSName(s), db.PTName(s))
	}
	return out
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "***"
	}
	if u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "REDACTEDPW")
			return strings.Replace(u.String(), "REDACTEDPW", "***", 1)
		}
	}
	return u.String()
}

// ------------------------------------------------------------ per-op runs

func (r *Runner) newResult(o *spec.Operation) *analyze.OpResult {
	return &analyze.OpResult{ID: o.ID, Method: o.Method, Path: o.Path, Phase: o.Phase, KSamples: map[int][]analyze.Sample{}, Statuses: map[int]int{}}
}

func (r *Runner) runOp(ctx context.Context, o *spec.Operation) *analyze.OpResult {
	return r.runOpWith(ctx, o, func(int) map[string]string { return nil })
}

func (r *Runner) runOpWith(ctx context.Context, o *spec.Operation, overrides func(i int) map[string]string) *analyze.OpResult {
	c := r.cfg
	res := r.newResult(o)
	ki := r.res.KParam(o)
	if ki != nil {
		res.KParam = ki.Name
	}
	observe := func(req *inputs.Request, s analyze.Sample, body []byte, hdr http.Header) {
		r.res.Observe(o, req, s.Status, body, hdr)
		if o.Method == "POST" && s.Status < 300 && s.Status > 0 {
			if id := extractID(body, hdr); id != "" {
				r.created[o.Path] = append(r.created[o.Path], id)
			}
		}
	}
	n := c.Run.Warmup + c.Run.Iterations
	for i := 0; i < n && ctx.Err() == nil; i++ {
		req, err := r.res.Build(ctx, o, i, -1, ki, overrides(i))
		if err != nil {
			res.Notes = append(res.Notes, err.Error())
			break
		}
		s, body, hdr := r.request(ctx, req, 0)
		observe(req, s, body, hdr)
		if s.Status >= 400 && c.Verbose && i == 0 {
			r.log("    %s %s → %d: %s", o.Method, o.Path, s.Status, truncateSQL(string(body), 200))
		}
		if i >= c.Run.Warmup {
			res.Samples = append(res.Samples, s)
		}
	}
	res.Pages = r.res.Pages(o)
	if ki != nil && !c.Scale.Disabled {
		iter := n
		sample := func(k int) bool {
			iter++
			req, err := r.res.Build(ctx, o, iter, k, ki, overrides(iter))
			if err != nil {
				return false
			}
			s, body, hdr := r.request(ctx, req, k)
			observe(req, s, body, hdr)
			res.KSamples[k] = append(res.KSamples[k], s)
			return true
		}
		var ks []int
		for _, k := range c.Scale.KSteps {
			if k < ki.Min || k > ki.Max || ctx.Err() != nil {
				continue
			}
			iter++
			if req, err := r.res.Build(ctx, o, iter, k, ki, overrides(iter)); err == nil { // warm this size once
				s, body, hdr := r.request(ctx, req, k)
				observe(req, s, body, hdr)
			}
			ks = append(ks, k)
			for j := 0; j < c.Run.KIterations; j++ {
				if !sample(k) {
					break
				}
			}
		}
		// keep sampling until the app-time slope's 95% CI is tight
		for round := c.Run.KIterations; round < c.Scale.MaxRepeats && ctx.Err() == nil; round++ {
			s, hw, significant := appSlope(res)
			if !significant || hw <= c.Scale.CITarget {
				break
			}
			if round == c.Run.KIterations {
				r.log("    k-axis slope %s: sampling more for a tighter interval", analyze.FmtSlope(s, hw))
			}
			for _, k := range ks {
				sample(k)
			}
		}
	}
	var lat []float64
	for _, s := range res.Samples {
		lat = append(lat, s.Ms)
		res.Statuses[s.Status]++
	}
	res.Latency = analyze.Latency{P50: analyze.Percentile(lat, 50), P95: analyze.Percentile(lat, 95), P99: analyze.Percentile(lat, 99), N: len(lat)}
	r.log("  %-6s %-40s p50 %7.1fms  stmts/req %d", o.Method, o.Path, res.Latency.P50, medianStmts(res.Samples))
	return res
}

func medianStmts(ss []analyze.Sample) int {
	var v []float64
	for _, s := range ss {
		v = append(v, float64(s.NStmts))
	}
	return int(analyze.Median(v))
}

// runWrite runs a write op with lifecycle ID chaining: IDs come from OpenAPI
// links when the spec declares them, else from the collection's POST.
func (r *Runner) runWrite(ctx context.Context, o *spec.Operation, all []*spec.Operation) *analyze.OpResult {
	param := spec.LastParam(o.Path)
	if o.Method == "POST" || param == "" || o.Protocol != "" && o.Protocol != "http" {
		return r.runOp(ctx, o)
	}
	if o.Method != "DELETE" && r.res.HasLinks(o) { // PUT/PATCH reuse linked IDs
		return r.runOp(ctx, o)
	}
	coll := spec.CollectionPath(o.Path)
	src, srcParam := r.res.LinkSource(o)
	current := func() []string {
		if src != nil {
			var out []string
			for _, v := range r.res.Linked(o.ID, srcParam) {
				out = append(out, fmt.Sprint(v))
			}
			return out
		}
		return r.created[coll]
	}
	ids := current()
	need := r.cfg.Run.Warmup + r.cfg.Run.Iterations + 8
	if o.Method == "DELETE" && len(ids) < need {
		// pre-create resources (untimed) through the op that links here, or the collection's POST
		creator := src
		if creator == nil {
			for _, p := range all {
				if p.Method == "POST" && p.Path == coll {
					creator = p
				}
			}
		}
		if creator != nil {
			for j := 0; len(current()) < need && j < need*2; j++ {
				req, err := r.res.Build(ctx, creator, 50000+j, -1, nil, nil)
				if err != nil {
					break
				}
				status, body, hdr, _, err := r.send(ctx, req)
				if err == nil && status < 300 {
					r.res.Observe(creator, req, status, body, hdr)
					if id := extractID(body, hdr); id != "" && src == nil {
						r.created[coll] = append(r.created[coll], id)
					}
				}
			}
		}
		ids = current()
	}
	if len(ids) == 0 {
		if o.Method == "DELETE" && r.result.Snapshot == "none" {
			res := r.newResult(o)
			res.Skipped = "no tool-created resource to delete and no snapshot"
			r.result.Skipped = append(r.result.Skipped, res)
			return nil
		}
		return r.runOp(ctx, o) // anchors; snapshot restores
	}
	if o.Method == "DELETE" {
		pool := append([]string(nil), ids...)
		if src != nil {
			r.res.TakeLinked(o.ID, srcParam)
		} else {
			r.created[coll] = nil
		}
		return r.runOpWith(ctx, o, func(i int) map[string]string {
			if i < len(pool) {
				return map[string]string{param: pool[len(pool)-1-i]}
			}
			return nil
		})
	}
	id := ids[0]
	return r.runOpWith(ctx, o, func(int) map[string]string { return map[string]string{param: id} })
}

// ------------------------------------------------------------ subsets

func (r *Runner) buildSubsets(ctx context.Context, results []*analyze.OpResult) {
	names := map[string]bool{}
	var dmlTargets []string
	for _, res := range results {
		for _, s := range append(res.Samples, flatten(res.KSamples)...) {
			for _, st := range s.Stmts {
				for _, t := range st.Tables {
					if r.tables[t] != nil {
						names[t] = true
					}
				}
				if (st.Kind == "delete" || st.Kind == "update") && st.Target != "" {
					dmlTargets = append(dmlTargets, st.Target)
				}
			}
		}
	}
	for _, t := range db.ChildClosure(dmlTargets, r.fks) {
		if r.tables[t] != nil {
			names[t] = true
		}
	}
	var want []string
	for n := range names {
		want = append(want, n)
	}
	scope := db.ParentClosure(r.db.Info().Dialect, want, r.tables, r.fks)
	var missing []*db.Table
	for _, t := range scope {
		if !r.built[t.Key] {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return
	}
	// rebuild the whole scope so FK-consistency holds across new tables
	order := db.TopoOrder(scope, r.fks)
	r.log("building nested subsets %v for %d tables", r.nsSteps, len(order))
	src := ""
	if r.result.Snapshot != "none" {
		src = db.SnapNS // pristine copy taken before write operations
	}
	for _, s := range r.nsSteps {
		cnt, err := r.db.BuildSubset(ctx, db.NSName(s), s, order, r.fks, src)
		if err != nil {
			r.result.Warnings = append(r.result.Warnings, "subset build failed: "+err.Error())
			r.nsSteps = nil
			return
		}
		r.counts[s] = cnt
	}
	full := map[string]float64{}
	for _, t := range order {
		if t.HasData() {
			v, _ := r.db.FirstValues(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", t.SQL))
			if len(v) > 0 {
				var f float64
				fmt.Sscan(v[0], &f)
				full[t.Key] = f
				t.Rows = f
			}
		}
		r.built[t.Key] = true
	}
	r.counts[1] = full
}

func (r *Runner) dropSubsets() {
	if r.cfg.Scale.Keep {
		return
	}
	for _, s := range r.cfg.Scale.DataSteps {
		if s < 1 {
			_ = r.db.DropNamespace(context.Background(), db.NSName(s))
			_ = r.db.DropNamespace(context.Background(), db.PTName(s))
		}
	}
}
