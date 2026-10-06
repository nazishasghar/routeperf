package runner

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
)

// evict empties the DB cache for the given tables (and runs cache.cold_cmd).
func (r *Runner) evict(ctx context.Context, keys []string) error {
	var ts []*db.Table
	for _, k := range keys {
		if t := r.tables[k]; t != nil && t.HasData() {
			ts = append(ts, t)
		}
	}
	if cmd := r.cfg.Cache.ColdCmd; cmd != "" {
		cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		sh, flag := "sh", "-c"
		if runtime.GOOS == "windows" {
			sh, flag = "cmd", "/C"
		}
		if out, err := exec.CommandContext(cctx, sh, flag, cmd).CombinedOutput(); err != nil {
			return fmt.Errorf("cache.cold_cmd failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
	err := r.db.Evict(ctx, ts)
	if errors.Is(err, db.ErrUnsupported) && r.cfg.Cache.ColdCmd != "" {
		return nil
	}
	return err
}

// coldMethod describes what a cold measurement means on this setup, or an
// error when no eviction is possible.
func (r *Runner) coldMethod(ctx context.Context) (string, error) {
	err := r.db.Evict(ctx, nil)
	var parts []string
	if err == nil {
		if r.db.Info().Dialect == "postgres" {
			parts = append(parts, "shared_buffers evicted per table (pg_buffercache)")
		}
	}
	if r.cfg.Cache.ColdCmd != "" {
		parts = append(parts, "cold_cmd `"+r.cfg.Cache.ColdCmd+"` run before each sample")
	} else {
		parts = append(parts, "OS page cache still warm")
	}
	if err != nil && r.cfg.Cache.ColdCmd == "" {
		return "", err
	}
	if err != nil && r.db.Info().Dialect == "mysql" {
		parts = append(parts, "InnoDB buffer pool still warm")
	}
	return strings.Join(parts, "; "), nil
}

// coldPhase measures first-hit latency of each read endpoint right after the
// tables it touches are evicted from the cache.
func (r *Runner) coldPhase(ctx context.Context, results []*analyze.OpResult) {
	method, err := r.coldMethod(ctx)
	if err != nil {
		r.result.Warnings = append(r.result.Warnings, "cold-cache numbers unavailable: "+err.Error())
		r.cfg.Cache.Cold = false
		return
	}
	r.result.ColdMethod = method
	r.log("cold-cache samples (%s)", method)
	for _, res := range results {
		if res.Phase != "R" || res.Skipped != "" || len(res.Samples) == 0 || ctx.Err() != nil {
			continue
		}
		o := r.sp.ByID(res.ID)
		if o == nil {
			continue
		}
		set := map[string]bool{}
		for _, s := range res.Samples {
			for _, st := range s.Stmts {
				for _, t := range st.Tables {
					set[t] = true
				}
			}
		}
		keys := make([]string, 0, len(set))
		for k := range set {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var lat, dbms []float64
		for i := 0; i < r.cfg.Cache.ColdSamples; i++ {
			if err := r.evict(ctx, keys); err != nil {
				r.result.Warnings = append(r.result.Warnings, "cold sample failed: "+err.Error())
				return
			}
			req, err := r.res.Build(ctx, o, 70000+i, -1, nil, nil)
			if err != nil {
				break
			}
			s, _, _ := r.request(ctx, req, 0)
			if s.Err == "" {
				lat = append(lat, s.Ms)
				dbms = append(dbms, sampleDBMs(s))
			}
		}
		if len(lat) > 0 {
			res.Cold = &analyze.Latency{P50: analyze.Percentile(lat, 50), P95: analyze.Percentile(lat, 95), P99: analyze.Percentile(lat, 99), N: len(lat)}
			res.ColdDBMs = analyze.Median(dbms)
		}
	}
}

// coldReplay replays a query once right after evicting its tables.
func (r *Runner) coldReplay(ctx context.Context, q *analyze.QueryResult) {
	if err := r.evict(ctx, q.Example.Tables); err != nil {
		return
	}
	p, _, times, _, err := r.explainStmt(ctx, q.Example, db.Target{}, 0)
	if err != nil || len(times) == 0 {
		return
	}
	q.ColdMs = times[0]
	if p.Root != nil {
		q.ColdReads = p.Root.Reads
	}
}
