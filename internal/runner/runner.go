// Package runner drives a full routeperf run: discover → capture → replay →
// scale experiments → analysis → restore.
package runner

import (
	"bytes"
	"context"
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
	"github.com/nazishasghar/routeperf/internal/sqlutil"
)

type Result struct {
	Tool       string              `json:"tool"`
	Started    time.Time           `json:"started"`
	Seconds    float64             `json:"seconds"`
	Spec       string              `json:"spec"`
	SpecTitle  string              `json:"spec_title"`
	API        string              `json:"api"`
	Dialect    string              `json:"dialect"`
	DBVersion  string              `json:"db_version"`
	Database   string              `json:"database"`
	Tables     map[string]float64  `json:"tables"`
	DataSteps  []float64           `json:"data_steps"`
	Ops        []*analyze.OpResult `json:"operations"`
	Skipped    []*analyze.OpResult `json:"skipped"`
	Snapshot   string              `json:"snapshot"`
	Restored   []string            `json:"restored_tables,omitempty"`
	Verified   bool                `json:"restore_verified"`
	Warnings   []string            `json:"warnings,omitempty"`
	Unresolved map[string][]string `json:"unresolved_params,omitempty"`
	secrets    []string
}

// Secrets returns credential values to redact from any output.
func (r *Result) Secrets() []string { return r.secrets }

type Runner struct {
	cfg     *Config
	log     func(format string, a ...any)
	db      db.DB
	am      *auth.Manager
	sp      *spec.Spec
	res     *inputs.Resolver
	tables  map[string]*db.Table
	fks     []db.FK
	counts  map[float64]map[string]float64 // step → table → rows
	built   map[string]bool                // tables with subsets
	created map[string][]string            // collection path → created IDs
	baseURL *url.URL
	result  *Result
	nsSteps []float64
}

func New(cfg *Config, logf func(string, ...any)) *Runner {
	cfg.Defaults()
	return &Runner{cfg: cfg, log: logf, counts: map[float64]map[string]float64{}, built: map[string]bool{}, created: map[string][]string{}}
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
		switch {
		case !allowed[o.Method]:
			reason = "method not enabled"
		case len(c.Run.Include) > 0 && !match(c.Run.Include, o):
			reason = "not included"
		case match(c.Run.Exclude, o):
			reason = "excluded"
		case spec.Dangerous(o) && !match(c.Run.DangerousOps, o):
			reason = "dangerous operation (list it in run.dangerous_ops to run)"
		case o.Phase == "W" && !c.WritesEnabled():
			reason = "writes disabled"
		}
		if reason == "" {
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
	u := *r.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + req.Path
	u.RawQuery = req.Query.Encode()
	var body io.Reader
	var raw []byte
	if req.Body != nil {
		raw, _ = json.Marshal(req.Body)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if raw != nil {
			body = bytes.NewReader(raw)
		}
		hr, err := http.NewRequestWithContext(ctx, req.Method, u.String(), body)
		if err != nil {
			return 0, nil, nil, 0, err
		}
		if raw != nil {
			hr.Header.Set("Content-Type", "application/json")
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

// request sends one request and captures the SQL it caused.
func (r *Runner) request(ctx context.Context, req *inputs.Request, k int) (analyze.Sample, []byte, http.Header) {
	s := analyze.Sample{K: k}
	m0, err := r.db.Mark(ctx)
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
	if strings.HasPrefix(r.db.LogSource(), "docker") && settle < 150*time.Millisecond {
		settle = 150 * time.Millisecond // docker log stream lag
	}
	time.Sleep(settle)
	m1, _ := r.db.Mark(ctx)
	stmts, err := r.db.Window(ctx, m0, m1)
	if err != nil {
		s.Err = "capture: " + err.Error()
	}
	s.Status, s.Ms, s.Bytes, s.Stmts, s.NStmts = status, ms, len(body), stmts, len(stmts)
	return s, body, hdr
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

	mk := pending{DBURL: redactURL(c.DB.URL), Capture: true}
	writeMarker(mk)
	if err := r.db.StartCapture(ctx); err != nil {
		return nil, fmt.Errorf("start capture: %w", err)
	}
	captureOn := true
	stopCapture := func() {
		if captureOn {
			if err := r.db.StopCapture(context.Background()); err != nil {
				r.result.Warnings = append(r.result.Warnings, "restoring log settings failed: "+err.Error())
			}
			captureOn = false
		}
	}
	defer stopCapture()

	var results []*analyze.OpResult
	for _, o := range reads {
		if ctx.Err() != nil {
			break
		}
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
			res := r.runWrite(ctx, o, writes)
			if res != nil {
				results = append(results, res)
			}
		}
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
			if res.Phase != "W" {
				continue
			}
			for _, s := range append(res.Samples, flatten(res.KSamples)...) {
				for _, st := range s.Stmts {
					if st.Kind == "insert" || st.Kind == "update" || st.Kind == "delete" {
						if t := sqlutil.TargetTable(st.SQL); t != "" {
							set[t] = true
						}
					}
				}
			}
		}
		for t := range set {
			if r.tables[t] != nil {
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

	unindexed := map[string][]string{}
	for _, f := range r.fks {
		if f.Declared && !f.Indexed {
			unindexed[f.Parent] = append(unindexed[f.Parent], f.Child+"."+f.ChildCol)
		}
	}
	tableRows := map[string]float64{}
	for n, t := range r.tables {
		tableRows[t.Name] = t.Rows
		tableRows[n] = t.Rows
	}
	colSet := map[string]map[string]bool{}
	for n, t := range r.tables {
		colSet[n] = map[string]bool{}
		for _, cn := range t.Cols {
			colSet[n][strings.ToLower(cn)] = true
		}
	}
	for _, res := range results {
		for _, q := range res.Queries {
			if q.Kind == "delete" || q.Kind == "update" {
				if t := sqlutil.TargetTable(q.SQL); t != "" && len(unindexed[t]) > 0 {
					q.Notes = append(q.Notes, "fk-child:"+strings.SplitN(unindexed[t][0], ".", 2)[0])
				}
			}
			analyze.AnalyzeQuery(q, tableRows)
		}
		analyze.AnalyzeRoute(res, c.Thresholds.P95Ms, c.Thresholds.MaxQueries)
		analyze.Advise(res, tableRows, unindexed, colSet)
	}
	r.result.Ops = results
	r.result.Unresolved = r.res.Unresolved
	r.result.Seconds = time.Since(r.result.Started).Seconds()
	if !snapshot || len(r.result.Warnings) == 0 || r.result.Verified {
		_ = os.Remove(markerPath())
	}
	return r.result, nil
}

func (r *Runner) verifyRestore(ts []*db.Table) bool {
	ok := true
	for _, t := range ts {
		a, _ := r.db.FirstValues(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s", t.Name))
		b, _ := r.db.FirstValues(context.Background(), fmt.Sprintf("SELECT COUNT(*) FROM %s.%s", db.SnapNS, t.Name))
		if len(a) == 0 || len(b) == 0 || a[0] != b[0] {
			ok = false
		}
	}
	return ok
}

func flatten(m map[int][]analyze.Sample) []analyze.Sample {
	var out []analyze.Sample
	for _, v := range m {
		out = append(out, v...)
	}
	return out
}

func nsNames(steps []float64) []string {
	var out []string
	for _, s := range steps {
		out = append(out, db.NSName(s))
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
			u.User = url.UserPassword(u.User.Username(), "***")
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
	n := c.Run.Warmup + c.Run.Iterations
	for i := 0; i < n && ctx.Err() == nil; i++ {
		req, err := r.res.Build(ctx, o, i, -1, ki, overrides(i))
		if err != nil {
			res.Notes = append(res.Notes, err.Error())
			break
		}
		s, body, hdr := r.request(ctx, req, 0)
		if o.Method == "POST" && s.Status < 300 && s.Status > 0 {
			if id := extractID(body, hdr); id != "" {
				coll := o.Path
				r.created[coll] = append(r.created[coll], id)
			}
		}
		if i >= c.Run.Warmup {
			res.Samples = append(res.Samples, s)
		}
	}
	if ki != nil && !c.Scale.Disabled {
		iter := n
		for _, k := range c.Scale.KSteps {
			if k < ki.Min || k > ki.Max || ctx.Err() != nil {
				continue
			}
			for j := 0; j < c.Run.KIterations+1; j++ {
				iter++
				req, err := r.res.Build(ctx, o, iter, k, ki, overrides(iter))
				if err != nil {
					break
				}
				s, body, hdr := r.request(ctx, req, k)
				if o.Method == "POST" && s.Status < 300 && s.Status > 0 {
					if id := extractID(body, hdr); id != "" {
						r.created[o.Path] = append(r.created[o.Path], id)
					}
				}
				if j > 0 {
					res.KSamples[k] = append(res.KSamples[k], s)
				}
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

// runWrite runs a write op with lifecycle ID chaining.
func (r *Runner) runWrite(ctx context.Context, o *spec.Operation, all []*spec.Operation) *analyze.OpResult {
	param := spec.LastParam(o.Path)
	if o.Method == "POST" || param == "" {
		return r.runOp(ctx, o)
	}
	coll := spec.CollectionPath(o.Path)
	ids := r.created[coll]
	need := r.cfg.Run.Warmup + r.cfg.Run.Iterations + 8
	if o.Method == "DELETE" && len(ids) < need {
		// pre-create resources (untimed) through the collection's POST
		for _, p := range all {
			if p.Method == "POST" && p.Path == coll {
				for j := 0; len(r.created[coll]) < need && j < need*2; j++ {
					req, err := r.res.Build(ctx, p, 50000+j, -1, nil, nil)
					if err != nil {
						break
					}
					status, body, hdr, _, err := r.send(ctx, req)
					if err == nil && status < 300 {
						if id := extractID(body, hdr); id != "" {
							r.created[coll] = append(r.created[coll], id)
						}
					}
				}
			}
		}
		ids = r.created[coll]
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
		r.created[coll] = nil
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
				if st.Kind == "delete" || st.Kind == "update" {
					dmlTargets = append(dmlTargets, sqlutil.TargetTable(st.SQL))
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
	scope := db.ParentClosure(want, r.tables, r.fks)
	var missing []*db.Table
	for _, t := range scope {
		if !r.built[t.Name] {
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
		v, _ := r.db.FirstValues(ctx, fmt.Sprintf("SELECT COUNT(*) FROM %s", t.Name))
		if len(v) > 0 {
			var f float64
			fmt.Sscan(v[0], &f)
			full[t.Name] = f
			t.Rows = f
		}
		r.built[t.Name] = true
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
		}
	}
}
