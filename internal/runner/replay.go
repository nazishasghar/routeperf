package runner

import (
	"context"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/plan"
	"github.com/nazishasghar/routeperf/internal/sqlutil"
)

func paramKey(st analyze.Stmt) string {
	return st.Fingerprint + "|" + strings.Join(st.Params, "\x1f") + "|" + st.SQL
}

// sampleDBMs is the DB time a sample's statements took (0 when unknown).
func sampleDBMs(s analyze.Sample) float64 {
	var d float64
	for _, st := range s.Stmts {
		if st.Kind != "other" {
			d += st.DurMs
		}
	}
	return d
}

// appPoints returns per-sample app-side time (latency minus logged DB time).
func appPoints(res *analyze.OpResult) []analyze.Point {
	var pts []analyze.Point
	for k, ss := range res.KSamples {
		for _, smp := range ss {
			pts = append(pts, analyze.Point{X: float64(k), Y: math.Max(0, smp.Ms-sampleDBMs(smp))})
		}
	}
	return pts
}

// appSlope fits the growth of app-side time (latency minus logged DB time,
// net of the time at the smallest k) against k over the sizes where it rises
// above noise. significant is false when the app does no measurable
// per-item work.
func appSlope(res *analyze.OpResult) (s, hw float64, significant bool) {
	ks := make([]int, 0, len(res.KSamples))
	for k := range res.KSamples {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	if len(ks) < 3 {
		return 0, 0, false
	}
	med := map[int]float64{}
	for _, k := range ks {
		var v []float64
		for _, smp := range res.KSamples[k] {
			v = append(v, math.Max(0, smp.Ms-sampleDBMs(smp)))
		}
		med[k] = analyze.Median(v)
	}
	base := med[ks[0]]
	maxGrowth := 0.0
	for _, k := range ks {
		maxGrowth = math.Max(maxGrowth, med[k]-base)
	}
	if maxGrowth <= 2 { // under 2 ms of extra app time at any size: jitter
		return 0, 0, false
	}
	floor := math.Max(0.5, 0.05*maxGrowth)
	var seg []int
	for _, k := range ks[1:] {
		if med[k]-base >= floor {
			seg = append(seg, k)
		}
	}
	if len(seg) < 2 {
		return 0, 0, false
	}
	var pts []analyze.Point
	for _, k := range seg {
		for _, smp := range res.KSamples[k] {
			pts = append(pts, analyze.Point{X: float64(k), Y: math.Max(0.01, smp.Ms-sampleDBMs(smp)-base)})
		}
	}
	s, hw, ok := analyze.SlopeCI(pts)
	return s, finite(hw), ok
}

// appDegree combines the polynomial decomposition (dominant term of
// mixtures) with the measured slope: k² needs both to agree.
func appDegree(pts []analyze.Point, s, hw float64) (float64, float64) {
	d, share := analyze.PolyDegree(pts)
	if d == 2 && s+hw >= 1.3 {
		return 2, share
	}
	deg, _ := analyze.Degree(s, hw)
	return math.Min(deg, 1.5), 0
}

func finite(v float64) float64 {
	if math.IsInf(v, 0) || math.IsNaN(v) || v > 9.99 {
		return 9.99
	}
	return v
}

// replay groups captured statements per fingerprint and runs EXPLAIN ANALYZE
// at 100%, on each data subset, and at each k step.
func (r *Runner) replay(ctx context.Context, res *analyze.OpResult) {
	type agg struct {
		q      *analyze.QueryResult
		counts []float64
		exam   []float64
	}
	byFP := map[string]*agg{}
	var order []string
	var stmtCounts []float64
	nplus := map[string]bool{}
	redundant := map[string]bool{}
	for _, s := range res.Samples {
		per := map[string]int{}
		identical := map[string]int{}
		distinct := map[string]map[string]bool{}
		data := 0
		for _, st := range s.Stmts {
			if st.Kind == "other" {
				continue
			}
			data++
			per[st.Fingerprint]++
			identical[paramKey(st)]++
			if distinct[st.Fingerprint] == nil {
				distinct[st.Fingerprint] = map[string]bool{}
			}
			distinct[st.Fingerprint][strings.Join(st.Params, ",")+st.SQL] = true
			if _, ok := byFP[st.Fingerprint]; !ok {
				byFP[st.Fingerprint] = &agg{q: &analyze.QueryResult{Fingerprint: st.Fingerprint, SQL: st.SQL, Kind: st.Kind, Example: st}}
				order = append(order, st.Fingerprint)
			}
		}
		stmtCounts = append(stmtCounts, float64(data))
		for fp, a := range byFP {
			a.counts = append(a.counts, float64(per[fp]))
			if per[fp] >= 5 && len(distinct[fp]) >= 5 {
				nplus[fp] = true
			}
		}
		for k, n := range identical {
			if n > 1 {
				fp := strings.SplitN(k, "|", 2)[0]
				if byFP[fp] != nil && byFP[fp].q.Kind == "select" {
					redundant[fp] = true
				}
			}
		}
	}
	res.QPerReq = analyze.Median(stmtCounts)

	// k-axis aggregation
	ks := make([]int, 0, len(res.KSamples))
	for k := range res.KSamples {
		ks = append(ks, k)
	}
	sort.Ints(ks)
	kCount := map[string]map[int]float64{}
	kStmt := map[string]map[int]analyze.Stmt{}
	kTdb := map[int]float64{}
	res.KLatency = map[int]float64{}
	for _, k := range ks {
		var lat, tdb []float64
		perFP := map[string][]float64{}
		for _, s := range res.KSamples[k] {
			lat = append(lat, s.Ms)
			cnt := map[string]float64{}
			for _, st := range s.Stmts {
				if st.Kind == "other" {
					continue
				}
				cnt[st.Fingerprint]++
				if kStmt[st.Fingerprint] == nil {
					kStmt[st.Fingerprint] = map[int]analyze.Stmt{}
				}
				if _, ok := kStmt[st.Fingerprint][k]; !ok {
					kStmt[st.Fingerprint][k] = st
				}
				if _, ok := byFP[st.Fingerprint]; !ok {
					byFP[st.Fingerprint] = &agg{q: &analyze.QueryResult{Fingerprint: st.Fingerprint, SQL: st.SQL, Kind: st.Kind, Example: st}}
					order = append(order, st.Fingerprint)
				}
			}
			for fp, c := range cnt {
				perFP[fp] = append(perFP[fp], c)
			}
			tdb = append(tdb, sampleDBMs(s))
		}
		res.KLatency[k] = analyze.Median(lat)
		kTdb[k] = analyze.Median(tdb)
		for fp, v := range perFP {
			if kCount[fp] == nil {
				kCount[fp] = map[int]float64{}
			}
			kCount[fp][k] = analyze.Median(v)
		}
	}
	for fp, m := range kCount {
		var pts []analyze.Point
		minC, maxC := math.Inf(1), 0.0
		for _, k := range ks {
			c := m[k]
			minC, maxC = math.Min(minC, c), math.Max(maxC, c)
			if c > 0 {
				pts = append(pts, analyze.Point{X: float64(k), Y: c})
			}
		}
		if len(ks) >= 2 && maxC >= 3 && maxC > minC*1.5 {
			// queries/request grows with k: an N+1 when the count's slope is ~1
			if s, hw, ok := analyze.SlopeCI(pts); !ok || s-finite(hw) > 0.3 {
				nplus[fp] = true
			}
		}
	}

	n := 0
	for _, fp := range order {
		a := byFP[fp]
		q := a.q
		if q.Kind == "other" {
			continue
		}
		n++
		q.ID = fmt.Sprintf("q%d", n)
		q.CountPerReq = analyze.Median(a.counts)
		var durs []float64
		for _, smp := range res.Samples {
			for _, st := range smp.Stmts {
				if st.Fingerprint == fp && st.DurMs > 0 {
					durs = append(durs, st.DurMs)
				}
			}
		}
		q.ObservedMs = analyze.Median(durs)
		if nplus[fp] {
			q.CountKExp = 1
			res.NPlusOne = append(res.NPlusOne, q.ID)
		}
		if redundant[fp] && !nplus[fp] {
			res.Redundant = append(res.Redundant, q.ID)
		}
		p, work, times, ex, err := r.explainStmt(ctx, q.Example, db.Target{}, r.cfg.Scale.Repeats)
		if err != nil {
			q.Error = err.Error()
			res.Queries = append(res.Queries, q)
			continue
		}
		ms := analyze.Median(times)
		mode := r.cfg.Capture.PGPlanMode
		if r.db.Info().Dialect == "postgres" && mode != "custom" && len(ex.Params) > 0 &&
			(mode == "generic" || (q.ObservedMs > 1 && q.ObservedMs > 3*ms)) {
			g := ex
			g.Generic = true
			if gp, gw, gt, gex, err := r.explainStmt(ctx, g, db.Target{}, r.cfg.Scale.Repeats); err == nil && gex.Generic {
				gms := analyze.Median(gt)
				if mode == "generic" || math.Abs(gms-q.ObservedMs) < math.Abs(ms-q.ObservedMs) {
					p, work, times, ms, ex = gp, gw, gt, gms, gex
					q.Notes = append(q.Notes, fmt.Sprintf("the app runs this as a prepared statement with a generic plan (app %.2fms vs custom-plan replay); replays use the generic plan too", q.ObservedMs))
				}
			}
		}
		q.Example = ex
		q.Base, q.BaseWork, q.BaseMs = p, work, ms
		if r.cfg.Cache.Cold {
			r.coldReplay(ctx, q)
		}
		dom := r.dominant(p, ex)
		q.Driver = dom
		full := r.counts[1][dom]
		if full == 0 && r.tables[dom] != nil {
			full = r.tables[dom].Rows
		}
		if r.covered(ex.Tables) && len(r.nsSteps) > 0 {
			for _, s := range r.nsSteps {
				nRows := r.counts[s][dom]
				if nRows < 200 {
					continue
				}
				sp, sw, st, _, err := r.explainStmt(ctx, ex, db.Target{NS: db.NSName(s)}, r.cfg.Scale.Repeats)
				pt := analyze.ScalePoint{Step: s, N: nRows}
				if err != nil {
					pt.Errors = err.Error()
					q.Scale = append(q.Scale, pt)
					continue
				}
				pt.Work, pt.Ms, pt.Times, pt.Shape, pt.Plan = sw, analyze.Median(st), st, sp.ShapeHash(), sp
				q.Scale = append(q.Scale, pt)
			}
			if full > 0 {
				q.Scale = append(q.Scale, analyze.ScalePoint{Step: 1, N: full, Work: work, Ms: ms, Times: times, Shape: p.ShapeHash(), Plan: p})
			}
			var good []analyze.ScalePoint
			for _, pt := range q.Scale {
				if pt.Errors == "" {
					good = append(good, pt)
				}
			}
			q.Scale = good
			r.tightenTimes(ctx, q)
		}
		var kpts []analyze.Point
		for _, k := range ks {
			st, ok := kStmt[fp][k]
			if !ok {
				continue
			}
			kp := analyze.KPoint{K: k, Count: kCount[fp][k]}
			if _, w, t, _, err := r.explainStmt(ctx, st, db.Target{}, r.cfg.Scale.Repeats); err == nil {
				kp.Work, kp.Ms = w, analyze.Median(t)
				if w > 0 {
					kpts = append(kpts, analyze.Point{X: float64(k), Y: w})
				}
			}
			q.KPoints = append(q.KPoints, kp)
		}
		q.WorkKExp = workKExp(kpts, q)
		res.Queries = append(res.Queries, q)
	}
	// Per-request DB cost: logged durations (PG log, MySQL performance_schema,
	// proxy) or a replay estimate.
	var dbms []float64
	for _, s := range res.Samples {
		dbms = append(dbms, sampleDBMs(s))
	}
	res.DBMs = analyze.Median(dbms)
	for _, q := range res.Queries {
		cnt := math.Max(q.CountPerReq, 1)
		res.RowsPerReq += cnt * q.BaseWork
		if res.DBMs == 0 {
			defer func(c, ms float64) { res.DBMs += c * ms }(cnt, q.BaseMs)
		}
	}
	// No logged durations: estimate DB time from replays.
	for _, k := range ks {
		if kTdb[k] > 0 {
			continue
		}
		var t float64
		for _, q := range res.Queries {
			for _, kp := range q.KPoints {
				if kp.K == k {
					t += kp.Count * kp.Ms
				}
			}
		}
		kTdb[k] = t
	}
	if len(ks) >= 3 {
		var pts []analyze.Point
		for _, k := range ks {
			pts = append(pts, analyze.Point{X: float64(k), Y: math.Max(0, res.KLatency[k]-kTdb[k])})
		}
		f := analyze.FitCurve(pts, analyze.ON3)
		f.SlopeCI = finite(f.SlopeCI)
		res.AppFit = &f
		if hasDurations(res) {
			if s, hw, ok := appSlope(res); ok {
				res.AppSlope, res.AppSlopeCI = s, hw
				res.AppKExp, res.AppShare = appDegree(appPoints(res), s, hw)
			}
		} else { // DB time per sample unknown: fit the medians net of the replay estimate
			base, maxGrowth := pts[0].Y, 0.0
			for _, p := range pts {
				maxGrowth = math.Max(maxGrowth, p.Y-base)
			}
			if maxGrowth > 2 {
				var app []analyze.Point
				for _, p := range pts[1:] {
					if p.Y-base >= math.Max(0.5, 0.05*maxGrowth) {
						app = append(app, analyze.Point{X: p.X, Y: p.Y - base})
					}
				}
				if s, hw, ok := analyze.SlopeCI(app); ok {
					res.AppSlope, res.AppSlopeCI = s, finite(hw)
					res.AppKExp, res.AppShare = appDegree(pts, s, finite(hw))
				}
			}
		}
	}
	var parts []string
	var konst float64
	for _, q := range res.Queries {
		if q.CountKExp > 0 {
			parts = append(parts, "k")
		} else {
			konst += q.CountPerReq
		}
	}
	if len(parts) > 0 {
		m := strings.Join(parts, " + ")
		if len(parts) > 1 {
			m = fmt.Sprintf("%d·k", len(parts))
		}
		if konst > 0 {
			m += fmt.Sprintf(" + %.0f", konst)
		}
		res.QModel = m
	} else {
		res.QModel = fmt.Sprintf("%.0f", res.QPerReq)
	}
}

func hasDurations(res *analyze.OpResult) bool {
	for _, ss := range res.KSamples {
		for _, s := range ss {
			if sampleDBMs(s) > 0 {
				return true
			}
		}
	}
	return false
}

// workKExp is the growth of a query's rows examined with k, from the slope's
// confidence interval rather than fixed cut-offs.
func workKExp(pts []analyze.Point, q *analyze.QueryResult) float64 {
	if len(pts) < 2 {
		return 0
	}
	lo, hi := math.Inf(1), 0.0
	for _, p := range pts {
		lo, hi = math.Min(lo, p.Y), math.Max(hi, p.Y)
	}
	if hi-lo < 5 { // a handful of rows at every k
		return 0
	}
	// growth that only starts at larger k: fit the upper part
	if len(pts) > 3 {
		pts = pts[len(pts)-3:]
	}
	s, hw, ok := analyze.SlopeCI(pts)
	if !ok {
		return 0
	}
	q.KSlope, q.KSlopeCI = s, finite(hw)
	d, _ := analyze.Degree(s, q.KSlopeCI)
	return d
}

// tightenTimes adds replays per data step until the time slope's 95% CI is
// within ci_target (or max_repeats), for queries cheap enough to repeat.
func (r *Runner) tightenTimes(ctx context.Context, q *analyze.QueryResult) {
	c := r.cfg
	if len(q.Scale) < 3 || q.BaseMs > 250 {
		return
	}
	slope := func() (float64, float64) {
		var pts []analyze.Point
		for _, pt := range q.Scale {
			for _, t := range pt.Times {
				pts = append(pts, analyze.Point{X: pt.N, Y: t})
			}
		}
		s, hw, _ := analyze.SlopeCI(pts)
		return s, hw
	}
	for reps := c.Scale.Repeats; reps < c.Scale.MaxRepeats && ctx.Err() == nil; reps += 2 {
		if _, hw := slope(); hw <= c.Scale.CITarget {
			return
		}
		for i := range q.Scale {
			pt := &q.Scale[i]
			tgt := db.Target{}
			if pt.Step < 1 {
				tgt.NS = db.NSName(pt.Step)
			}
			if _, _, t, _, err := r.explainStmt(ctx, q.Example, tgt, 2); err == nil {
				pt.Times = append(pt.Times, t...)
				pt.Ms = analyze.Median(pt.Times)
			}
		}
	}
}

// perTable shrinks one table at a time (others stay at 100%) for queries that
// read several large tables, so growth can be attributed to each table.
func (r *Runner) perTable(ctx context.Context, results []*analyze.OpResult) {
	dialect := r.db.Info().Dialect
	type job struct {
		q      *analyze.QueryResult
		tables []string
	}
	var jobs []job
	need := map[string]bool{}
	for _, res := range results {
		for _, q := range res.Queries {
			if q.Base == nil || q.Error != "" || !sqlutil.Parse(dialect, q.Example.SQL).OK {
				continue
			}
			direct := map[string]bool{}
			for _, t := range q.Example.Tables {
				direct[t] = true
			}
			var ts []string
			for _, rel := range q.Base.Relations() {
				t := r.tables[rel]
				if t == nil || !t.HasData() || !direct[rel] || r.fullRows(rel) < 2000 {
					continue
				}
				ts = append(ts, rel)
			}
			if len(ts) >= 2 {
				sort.Strings(ts)
				jobs = append(jobs, job{q, ts})
				for _, t := range ts {
					need[t] = true
				}
			}
		}
	}
	if len(jobs) == 0 {
		return
	}
	keys := make([]string, 0, len(need))
	for k := range need {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	r.log("per-table scaling for %d multi-table queries (%s)", len(jobs), strings.Join(keys, ", "))
	src := ""
	if r.result.Snapshot != "none" {
		src = db.SnapNS
	}
	for _, s := range r.nsSteps {
		r.ptCounts[s] = map[string]float64{}
		for _, k := range keys {
			n, err := r.db.BuildShrunk(ctx, db.PTName(s), s, r.tables[k], r.res.Anchors(ctx, r.tables[k]), src)
			if err != nil {
				r.result.Warnings = append(r.result.Warnings, "per-table subset failed: "+err.Error())
				return
			}
			r.ptCounts[s][k] = n
		}
	}
	for _, j := range jobs {
		for _, t := range j.tables {
			g := analyze.TableGrowth{Table: t}
			var pts []analyze.Point
			for _, s := range r.nsSteps {
				nRows := r.ptCounts[s][t]
				if nRows < 200 {
					continue
				}
				_, w, times, _, err := r.explainStmt(ctx, j.q.Example, db.Target{NS: db.PTName(s), Only: map[string]bool{t: true}}, 1)
				if err != nil {
					continue
				}
				g.Points = append(g.Points, analyze.ScalePoint{Step: s, N: nRows, Work: w, Ms: analyze.Median(times)})
				pts = append(pts, analyze.Point{X: nRows, Y: math.Max(w, 1)})
			}
			full := r.fullRows(t)
			g.Points = append(g.Points, analyze.ScalePoint{Step: 1, N: full, Work: j.q.BaseWork, Ms: j.q.BaseMs})
			pts = append(pts, analyze.Point{X: full, Y: math.Max(j.q.BaseWork, 1)})
			if len(pts) < 3 {
				continue
			}
			// fit with an intercept: the other tables' fixed work must not
			// flatten this table's exponent
			f := analyze.FitCurve(pts, analyze.ON3)
			if !f.OK {
				continue
			}
			lo, hi := math.Inf(1), 0.0
			for _, p := range pts {
				lo, hi = math.Min(lo, p.Y), math.Max(hi, p.Y)
			}
			if f.Class == analyze.O1 || hi-lo < 5 {
				f.Slope, f.SlopeCI = 0, 0
			}
			g.Slope, g.CI = f.Slope, finite(f.SlopeCI)
			j.q.PerTable = append(j.q.PerTable, g)
		}
	}
}

func (r *Runner) fullRows(t string) float64 {
	if v := r.counts[1][t]; v > 0 {
		return v
	}
	if tb := r.tables[t]; tb != nil {
		return tb.Rows
	}
	return 0
}

func (r *Runner) covered(tables []string) bool {
	if len(tables) == 0 {
		return false
	}
	for _, t := range tables {
		if r.tables[t] != nil && !r.built[t] {
			return false
		}
	}
	return true
}

// dominant is the relation whose size drives a query: the scan examining the
// most rows, else the statement's largest table (through views).
func (r *Runner) dominant(p *plan.Plan, st analyze.Stmt) string {
	if d := dominantRel(p, analyze.Stmt{}); d != "" {
		return d
	}
	best, bv := "", -1.0
	consider := func(t *db.Table) {
		if t != nil && t.HasData() && r.fullRows(t.Key) > bv {
			best, bv = t.Key, r.fullRows(t.Key)
		}
	}
	for _, k := range append([]string{st.Target}, st.Tables...) {
		t := r.tables[k]
		if t == nil {
			continue
		}
		if t.Kind == "view" {
			for _, d := range db.ViewDeps(r.db.Info().Dialect, t, r.tables) {
				consider(d)
			}
			continue
		}
		consider(t)
	}
	if best == "" {
		return dominantRel(p, st)
	}
	return best
}

func dominantRel(p *plan.Plan, st analyze.Stmt) string {
	best, bv := "", -1.0
	p.Walk(func(n *plan.Node, _ int) {
		if n.Scan && n.Relation != "" && n.Rows+n.RowsRemoved > bv {
			best, bv = strings.ToLower(n.Relation), n.Rows+n.RowsRemoved
		}
	})
	if best == "" {
		if st.Target != "" {
			return st.Target
		}
		if len(st.Tables) > 0 {
			return st.Tables[0]
		}
	}
	return best
}

func inlined(st analyze.Stmt) string {
	if len(st.Params) == 0 {
		return st.SQL
	}
	nulls := map[int]bool{}
	for i, v := range st.Params {
		if db.IsNull(v) {
			nulls[i] = true
		}
	}
	return sqlutil.InlinePG(st.SQL, st.Params, nulls)
}

// idEq matches `[t.]col = value` with numeric, quoted string or UUID values,
// in PG ("…") or MySQL (`…`) quoting.
var idEq = regexp.MustCompile("(?i)(?:[`\"]?\\w+[`\"]?\\.)?[`\"]?(\\w+)[`\"]?\\s*=\\s*('(?:[^']|'')*'|\\d+)")

var idColRe = regexp.MustCompile(`(?i)^(\w+?)_?id$`)

// retarget swaps a missing row id (deleted/created by the app) for an anchor id
// so DML replays exercise real rows (and FK cascades). Works for integer and
// string/UUID keys.
func (r *Runner) retarget(ctx context.Context, st analyze.Stmt) (analyze.Stmt, bool) {
	sql := inlined(st)
	for _, m := range idEq.FindAllStringSubmatchIndex(sql, -1) {
		col := strings.ToLower(sql[m[2]:m[3]])
		var t *db.Table
		if col == "id" {
			t = r.tables[st.Target]
			if t == nil && len(st.Tables) > 0 {
				t = r.tables[st.Tables[0]]
			}
		} else if mm := idColRe.FindStringSubmatch(col); mm != nil {
			for _, c := range []string{mm[1] + "s", mm[1], mm[1] + "es", strings.TrimSuffix(mm[1], "y") + "ies"} {
				if r.tables[strings.ToLower(c)] != nil {
					t = r.tables[strings.ToLower(c)]
					break
				}
			}
		}
		if t == nil || t.PK == "" {
			continue
		}
		a := r.res.Anchors(ctx, t)
		if len(a) == 0 {
			continue
		}
		out := st
		out.SQL = sql[:m[4]] + "'" + strings.ReplaceAll(a[len(a)-1], "'", "''") + "'" + sql[m[5]:]
		out.Params = nil
		return out, true
	}
	return st, false
}

func scanRows(p *plan.Plan) float64 {
	var t float64
	p.Walk(func(n *plan.Node, _ int) {
		if n.Scan {
			t += n.Rows
		}
	})
	return t
}

// explainStmt replays a statement (1 warmup + repeats) and returns the plan,
// rows examined and every timing. Handles unique violations and missing DML
// targets.
func (r *Runner) explainStmt(ctx context.Context, st analyze.Stmt, tgt db.Target, repeats int) (*plan.Plan, float64, []float64, analyze.Stmt, error) {
	var last *plan.Plan
	var times []float64
	cur := st
	retargeted := false
	for i := 0; i <= repeats; i++ {
		var p *plan.Plan
		var err error
		for attempt := 0; attempt < 4; attempt++ {
			p, err = r.db.Explain(ctx, cur, tgt)
			if err == nil {
				break
			}
			if v, dup := db.DuplicateValue(err); dup && v != "" {
				cur = mutateDup(cur, v, attempt)
				continue
			}
			if col, val, parent, ok := db.FKViolation(err); ok {
				if nst, ok := r.fixFK(ctx, cur, col, val, parent); ok {
					cur = nst
					continue
				}
			}
			break
		}
		if err != nil {
			return nil, 0, nil, cur, err
		}
		if i == 0 && !retargeted && cur.Kind != "insert" && scanRows(p) == 0 {
			if nst, ok := r.retarget(ctx, cur); ok {
				cur, retargeted = nst, true
				i = -1
				continue
			}
		}
		if i > 0 || repeats == 0 {
			times = append(times, p.TotalMs())
		}
		last = p
	}
	if last == nil {
		return nil, 0, nil, cur, fmt.Errorf("no plan")
	}
	return last, last.RowsExamined(), times, cur, nil
}

func mutateDup(st analyze.Stmt, v string, n int) analyze.Stmt {
	out := st
	suffix := fmt.Sprintf("_rp%d", n+1)
	found := false
	if len(st.Params) > 0 {
		out.Params = append([]string(nil), st.Params...)
		for i, p := range out.Params {
			if p == v {
				out.Params[i] = p + suffix
				found = true
			}
		}
	}
	if !found {
		out.SQL = strings.ReplaceAll(st.SQL, "'"+strings.ReplaceAll(v, "'", "''")+"'", "'"+strings.ReplaceAll(v+suffix, "'", "''")+"'")
	}
	return out
}

var insertCols = regexp.MustCompile(`(?is)^\s*insert\s+into\s+\S+\s*\(([^)]*)\)\s*values\s*\((.*)\)`)

// fixFK points a foreign-key value that doesn't exist (row created by the app,
// absent from the subset) at an anchor row of the parent table.
func (r *Runner) fixFK(ctx context.Context, st analyze.Stmt, col, val, parent string) (analyze.Stmt, bool) {
	t := r.tables[strings.ToLower(parent)]
	if t == nil {
		for _, x := range r.tables { // subset copy name (schema__table) or another schema
			if strings.EqualFold(x.NSName(), parent) || strings.EqualFold(x.Name, parent) {
				t = x
				break
			}
		}
	}
	if t == nil {
		return st, false
	}
	a := r.res.Anchors(ctx, t)
	if len(a) == 0 {
		return st, false
	}
	sql := inlined(st)
	if val == "" { // MySQL doesn't report the value: map column → VALUES position
		if m := insertCols.FindStringSubmatch(sql); m != nil {
			cols := strings.Split(m[1], ",")
			vals := splitValues(m[2])
			for i, c := range cols {
				if strings.EqualFold(strings.Trim(strings.TrimSpace(c), "`\""), col) && i < len(vals) {
					val = strings.Trim(strings.TrimSpace(vals[i]), "'")
				}
			}
		}
	}
	if val == "" {
		return st, false
	}
	re := regexp.MustCompile(`'?\b` + regexp.QuoteMeta(val) + `\b'?`)
	loc := re.FindStringIndex(sql)
	if loc == nil {
		return st, false
	}
	out := st
	out.SQL, out.Params = sql[:loc[0]]+"'"+a[0]+"'"+sql[loc[1]:], nil
	return out, true
}

func splitValues(s string) []string {
	var out []string
	depth, inStr, start := 0, false, 0
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '\'':
			inStr = !inStr
		case inStr:
		case c == '(':
			depth++
		case c == ')':
			depth--
		case c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}
