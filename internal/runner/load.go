package runner

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/inputs"
	"github.com/nazishasghar/routeperf/internal/spec"
)

// loadPhase drives each endpoint with concurrent clients at increasing
// concurrency, sampling DB activity, to expose lock contention and pool
// limits that serial runs can't see.
func (r *Runner) loadPhase(ctx context.Context, results []*analyze.OpResult, ops []*spec.Operation) {
	c := r.cfg
	byID := map[string]*analyze.OpResult{}
	for _, res := range results {
		byID[res.ID] = res
	}
	for _, o := range ops {
		res := byID[o.ID]
		if res == nil || res.Skipped != "" || ctx.Err() != nil {
			continue
		}
		if o.Phase == "W" && (!c.Load.Writes || o.Method == "DELETE") {
			continue
		}
		pool := r.prebuild(ctx, o, 200)
		if len(pool) == 0 {
			continue
		}
		r.log("  load %-6s %-40s concurrency %v × %s", o.Method, o.Path, c.Load.Concurrency, c.loadDuration())
		for _, conc := range c.Load.Concurrency {
			if ctx.Err() != nil {
				break
			}
			res.Load = append(res.Load, r.loadLevel(ctx, pool, conc, c.loadDuration()))
		}
		res.LoadNote = loadVerdict(res.Load)
	}
}

// prebuild creates request inputs up front: the resolver isn't safe for
// concurrent use.
func (r *Runner) prebuild(ctx context.Context, o *spec.Operation, n int) []*inputs.Request {
	var out []*inputs.Request
	for i := 0; i < n; i++ {
		req, err := r.res.Build(ctx, o, 100000+i, -1, nil, nil)
		if err != nil {
			break
		}
		out = append(out, req)
	}
	return out
}

func cloneReq(q *inputs.Request) *inputs.Request {
	c := *q
	c.Header = make(map[string]string, len(q.Header)+1)
	for k, v := range q.Header {
		c.Header[k] = v
	}
	return &c
}

func (r *Runner) loadLevel(ctx context.Context, pool []*inputs.Request, conc int, d time.Duration) analyze.LoadStep {
	step := analyze.LoadStep{Concurrency: conc, Waits: map[string]int{}, Blocked: map[string]int{}}
	m0, _ := r.cap.Mark(ctx)
	var mu sync.Mutex
	var lat []float64
	traces := map[string]bool{}
	var next, errs int64
	lctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	// DB activity sampler
	sampDone := make(chan struct{})
	go func() {
		defer close(sampDone)
		t := time.NewTicker(100 * time.Millisecond)
		defer t.Stop()
		for {
			select {
			case <-lctx.Done():
				return
			case <-t.C:
				s, err := r.db.LoadSample(ctx)
				if err != nil {
					continue
				}
				step.MaxActive = max(step.MaxActive, s.Active)
				step.MaxConns = max(step.MaxConns, s.Connections)
				step.LockWaits = max(step.LockWaits, s.LockWaits)
				for k, v := range s.Waits {
					step.Waits[k] += v
				}
				for k, v := range s.Blocked {
					step.Blocked[k] += v
				}
			}
		}
	}()
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for lctx.Err() == nil {
				i := atomic.AddInt64(&next, 1)
				req := cloneReq(pool[int(i)%len(pool)])
				trace, span := newTrace()
				if r.cfg.TraceOn() {
					req.Header["traceparent"] = "00-" + trace + "-" + span + "-01"
				}
				status, _, _, ms, err := r.send(lctx, req)
				if lctx.Err() != nil && err != nil {
					return // cut off by the deadline
				}
				mu.Lock()
				if err != nil || status >= 400 || status == 0 {
					errs++
				} else {
					lat = append(lat, ms)
				}
				traces[trace] = true
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start).Seconds()
	cancel()
	<-sampDone
	step.Requests, step.Errors = len(lat)+int(errs), int(errs)
	step.RPS = math.Round(float64(len(lat))/elapsed*10) / 10
	step.P50, step.P95, step.P99 = analyze.Percentile(lat, 50), analyze.Percentile(lat, 95), analyze.Percentile(lat, 99)
	// per-request DB time from sqlcommenter-tagged statements
	time.Sleep(r.cfg.settle() + 100*time.Millisecond)
	m1, _ := r.cap.Mark(ctx)
	if stmts, err := r.cap.Window(ctx, m0, m1); err == nil {
		attributeLoad(&step, stmts, traces)
	}
	return step
}

func attributeLoad(step *analyze.LoadStep, stmts []analyze.Stmt, traces map[string]bool) {
	perTrace := map[string]float64{}
	count := map[string]int{}
	for _, st := range stmts {
		if st.Kind == "other" {
			continue
		}
		if st.TraceID == "" || !traces[st.TraceID] {
			step.Unattrib++
			continue
		}
		perTrace[st.TraceID] += st.DurMs
		count[st.TraceID]++
	}
	if len(count) == 0 {
		return
	}
	var dbms, q []float64
	for t, n := range count {
		dbms = append(dbms, perTrace[t])
		q = append(q, float64(n))
	}
	step.DBMs, step.QPerReq = analyze.Median(dbms), analyze.Median(q)
}

// loadVerdict explains how throughput and latency scale with concurrency.
func loadVerdict(steps []analyze.LoadStep) string {
	if len(steps) < 2 || steps[0].RPS == 0 {
		return ""
	}
	var notes []string
	base := steps[0]
	sat := 0
	for i := 1; i < len(steps); i++ {
		prev, cur := steps[i-1], steps[i]
		gain := cur.RPS / math.Max(prev.RPS, 0.1)
		want := float64(cur.Concurrency) / float64(prev.Concurrency)
		if sat == 0 && want > 1.5 && gain < 1.2 {
			sat = prev.Concurrency
		}
	}
	last := steps[len(steps)-1]
	peak := base
	for _, s := range steps {
		if s.RPS > peak.RPS {
			peak = s
		}
	}
	if sat > 0 {
		notes = append(notes, fmt.Sprintf("throughput stops scaling at concurrency %d (peak %.0f req/s)", sat, peak.RPS))
	} else {
		notes = append(notes, fmt.Sprintf("throughput scales to %.0f req/s at concurrency %d", last.RPS, last.Concurrency))
	}
	if base.P95 > 0 && last.P95 > 3*base.P95 {
		notes = append(notes, fmt.Sprintf("p95 grows %.0f× (%.1f → %.1f ms)", last.P95/base.P95, base.P95, last.P95))
	}
	if last.LockWaits > 0 {
		var rels []string
		for k := range last.Blocked {
			rels = append(rels, k)
		}
		sort.Strings(rels)
		msg := fmt.Sprintf("lock waits under load (up to %d sessions)", last.LockWaits)
		if len(rels) > 0 {
			msg += " on " + strings.Join(rels, ", ")
		}
		notes = append(notes, msg)
	}
	if sat > 0 && last.MaxConns > 0 && last.MaxConns < last.Concurrency && last.MaxActive <= last.MaxConns {
		plateau := true
		for _, s := range steps {
			if s.Concurrency >= sat && s.MaxConns > last.MaxConns {
				plateau = false
			}
		}
		if plateau {
			notes = append(notes, fmt.Sprintf("the DB never sees more than %d connections: likely the app's connection-pool size", last.MaxConns))
		}
	}
	if last.Errors > 0 {
		notes = append(notes, fmt.Sprintf("%d errors at concurrency %d", last.Errors, last.Concurrency))
	}
	if last.DBMs > 0 && steps[0].DBMs > 0 && last.DBMs > 3*steps[0].DBMs {
		notes = append(notes, fmt.Sprintf("DB time per request grows %.0f× under load (%.1f → %.1f ms)", last.DBMs/steps[0].DBMs, steps[0].DBMs, last.DBMs))
	}
	return strings.Join(notes, "; ")
}
