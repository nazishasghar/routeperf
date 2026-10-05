package analyze

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/plan"
)

// Stmt is one SQL statement the app issued during a request.
type Stmt struct {
	SQL         string   `json:"sql"`
	Params      []string `json:"params,omitempty"`
	DurMs       float64  `json:"dur_ms,omitempty"`
	Kind        string   `json:"kind"` // select|insert|update|delete|other
	Fingerprint string   `json:"fp"`
	Tables      []string `json:"tables,omitempty"`
}

type Sample struct {
	K      int     `json:"k,omitempty"`
	Status int     `json:"status"`
	Ms     float64 `json:"ms"`
	Bytes  int     `json:"bytes"`
	Stmts  []Stmt  `json:"-"`
	NStmts int     `json:"stmts"`
	Err    string  `json:"err,omitempty"`
}

type ScalePoint struct {
	Step   float64    `json:"step"`
	N      float64    `json:"n"` // rows of the dominant table at this step
	Work   float64    `json:"rows_examined"`
	Ms     float64    `json:"ms"`
	Shape  string     `json:"shape"`
	Plan   *plan.Plan `json:"-"`
	Errors string     `json:"error,omitempty"`
}

type KPoint struct {
	K     int     `json:"k"`
	Count float64 `json:"count_per_req"`
	Work  float64 `json:"rows_examined"`
	Ms    float64 `json:"ms"`
}

type QueryResult struct {
	ID           string                `json:"id"`
	Fingerprint  string                `json:"fp"`
	SQL          string                `json:"sql"`
	Kind         string                `json:"kind"`
	Example      Stmt                  `json:"example"`
	CountPerReq  float64               `json:"count_per_req"`
	Base         *plan.Plan            `json:"plan,omitempty"`
	BaseWork     float64               `json:"rows_examined"`
	BaseMs       float64               `json:"ms"`
	ObservedMs   float64               `json:"observed_ms,omitempty"` // median duration in the app's own executions
	Scale        []ScalePoint          `json:"scale,omitempty"`
	KPoints      []KPoint              `json:"k_points,omitempty"`
	WorkFit      *Fit                  `json:"work_fit,omitempty"`
	TimeFit      *Fit                  `json:"time_fit,omitempty"`
	CountKExp    float64               `json:"count_k_exp"`
	WorkKExp     float64               `json:"work_k_exp"`
	Static       string                `json:"static"`
	Expr         Expr                  `json:"-"`
	BigO         string                `json:"big_o"`
	Degree       float64               `json:"degree"`
	Dominant     string                `json:"dominant,omitempty"`
	PlanFlip     string                `json:"plan_flip,omitempty"`
	Confidence   string                `json:"confidence"`
	Notes        []string              `json:"notes,omitempty"`
	Error        string                `json:"error,omitempty"`
	Growth       map[string]NodeGrowth `json:"-"`
	TriggerHeavy bool                  `json:"trigger_heavy,omitempty"`
}

type Latency struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
	N   int     `json:"n"`
}

type Advice struct {
	Rule     string `json:"rule"`
	Message  string `json:"message"`
	SQL      string `json:"sql,omitempty"`
	Expected string `json:"expected,omitempty"`
	Query    string `json:"query,omitempty"`
}

type OpResult struct {
	ID         string             `json:"operation_id"`
	Method     string             `json:"method"`
	Path       string             `json:"path"`
	Phase      string             `json:"phase"`
	Skipped    string             `json:"skipped,omitempty"`
	KParam     string             `json:"k_param,omitempty"`
	Samples    []Sample           `json:"-"`
	KSamples   map[int][]Sample   `json:"-"`
	Statuses   map[int]int        `json:"statuses"`
	Latency    Latency            `json:"latency_ms"`
	QPerReq    float64            `json:"queries_per_request"`
	DBMs       float64            `json:"db_ms_per_request"`
	RowsPerReq float64            `json:"rows_examined_per_request"`
	QModel     string             `json:"queries_model,omitempty"`
	NPlusOne   []string           `json:"n_plus_one,omitempty"`
	Redundant  []string           `json:"redundant,omitempty"`
	AppFit     *Fit               `json:"app_fit,omitempty"`
	AppKExp    float64            `json:"app_k_exp"`
	KLatency   map[int]float64    `json:"k_latency_ms,omitempty"`
	Queries    []*QueryResult     `json:"queries"`
	BigO       string             `json:"big_o"`
	Dominant   string             `json:"dominant,omitempty"`
	Confidence string             `json:"confidence"`
	Status     string             `json:"status"`
	Reasons    []string           `json:"reasons,omitempty"`
	Projection map[string]float64 `json:"projection_p50_ms,omitempty"`
	Advice     []Advice           `json:"advice,omitempty"`
	Notes      []string           `json:"notes,omitempty"`
}

// ---------------------------------------------------------------- verdicts

// AnalyzeQuery combines static algebra, data-scale fits and time fits.
func AnalyzeQuery(q *QueryResult, tableRows map[string]float64) {
	if q.Base == nil {
		q.BigO, q.Confidence = "unknown", "low"
		return
	}
	// Plan flip: keep the contiguous suffix of steps sharing the 100% shape.
	pts := append([]ScalePoint(nil), q.Scale...)
	sort.Slice(pts, func(i, j int) bool { return pts[i].Step < pts[j].Step })
	baseShape := q.Base.ShapeHash()
	seg := pts
	for i := len(pts) - 1; i >= 0; i-- {
		if pts[i].Shape != baseShape {
			seg = pts[i+1:]
			q.PlanFlip = fmt.Sprintf("plan changes between %.0f%% and %.0f%% of data", pts[i].Step*100, nextStep(pts, i)*100)
			break
		}
	}
	trig := 0.0
	for _, t := range q.Base.Triggers {
		trig += t.TimeMs
	}
	q.TriggerHeavy = trig > 0.5*q.Base.TotalMs() && trig > 1

	var work, tm []Point
	for _, p := range seg {
		if p.N <= 0 {
			continue
		}
		work = append(work, Point{p.N, p.Work})
		tm = append(tm, Point{p.N, p.Ms})
	}
	if len(work) >= 3 {
		wf := FitCurve(work, ON3)
		tf := FitCurve(tm, ON3)
		lo, hi := math.Inf(1), 0.0
		for _, p := range work {
			lo, hi = math.Min(lo, p.Y), math.Max(hi, p.Y)
		}
		if hi-lo < 5 { // a handful of rows at every size: constant work
			wf = Fit{Class: O1, ClassName: O1.String(), OK: true, R2: 1, Points: wf.Points, Decades: wf.Decades, SlopeAgree: true, All: wf.All}
		}
		maxT := 0.0
		for _, p := range tm {
			maxT = math.Max(maxT, p.Y)
		}
		if maxT < 0.05 { // sub-50µs timings are noise
			tf.OK = false
		}
		q.WorkFit, q.TimeFit = &wf, &tf
		// per-node growth from matched paths
		g := map[string]NodeGrowth{}
		rowsPts, loopPts, examPts := map[string][]Point{}, map[string][]Point{}, map[string][]Point{}
		for _, p := range seg {
			if p.Plan == nil {
				continue
			}
			p.Plan.Walk(func(n *plan.Node, _ int) {
				rowsPts[n.Path] = append(rowsPts[n.Path], Point{p.N, n.Rows})
				loopPts[n.Path] = append(loopPts[n.Path], Point{p.N, n.Loops})
				examPts[n.Path] = append(examPts[n.Path], Point{p.N, n.Rows + n.RowsRemoved})
			})
		}
		for path := range rowsPts {
			g[path] = NodeGrowth{Rows: GrowthExponent(rowsPts[path]), Loops: GrowthExponent(loopPts[path]), Exam: GrowthExponent(examPts[path]), HasExam: true}
		}
		q.Growth = g
	} else {
		q.Growth = FallbackGrowth(q.Base, tableRows)
		q.Notes = append(q.Notes, "data-scale experiment unavailable (tables too small or skipped); static estimate only")
	}
	static := StaticCost(q.Base, q.Growth)
	if q.TriggerHeavy {
		if child := triggerTarget(q); child != "" { // RI cascade/check scans an unindexed child
			static = static.Add(Expr{Terms: []Term{T(map[string]float64{sym(child): 1}, nil)}}).Simplify()
		}
	}
	q.Static = static.String()
	q.Expr = static
	q.Degree = static.NDegree()
	q.Dominant = static.Dominant()

	conf := 0
	if q.WorkFit != nil && q.WorkFit.OK {
		fit := q.WorkFit
		if q.TriggerHeavy && q.TimeFit != nil && q.TimeFit.OK {
			fit = q.TimeFit // FK/trigger work is invisible to rows-examined
			q.Notes = append(q.Notes, "trigger/FK time dominates; degree taken from time fit")
		}
		ed := float64(fit.Class.Degree())
		if ed != q.Degree {
			q.Notes = append(q.Notes, fmt.Sprintf("static estimate %s disagrees with measured %s; measured wins", q.Static, fit.ClassName))
			dom := q.Dominant
			if dom == "" {
				dom = domRel(q.Base.Root)
			}
			if q.TriggerHeavy {
				if d := triggerTarget(q); d != "" {
					dom = d
				}
			}
			if dom == "" {
				dom = "rows"
			}
			var logs map[string]float64
			if fit.Class.HasLog() || ed == 0 && fit.Class == OLogN {
				logs = map[string]float64{sym(dom): 1}
			}
			if ed == 0 {
				q.Expr = Expr{Terms: []Term{T(nil, logs)}}
			} else {
				q.Expr = Expr{Terms: []Term{T(map[string]float64{sym(dom): ed}, logs)}}.Add(filterDeg(static, ed))
			}
			q.Degree, q.Dominant = ed, strings.TrimPrefix(sym(dom), "n_")
			conf--
		} else {
			conf++
		}
		if fit.Points >= 4 && fit.Decades >= 1.5 && fit.R2 >= 0.95 {
			conf++
		} else {
			conf--
		}
		if !fit.SlopeAgree {
			conf--
		}
		if q.PlanFlip != "" {
			conf--
		}
	} else {
		conf = -2
	}
	switch {
	case conf >= 2:
		q.Confidence = "high"
	case conf >= 0:
		q.Confidence = "medium"
	default:
		q.Confidence = "low"
	}
	q.BigO = q.Expr.String()
}

func filterDeg(e Expr, maxDeg float64) Expr {
	var out Expr
	for _, t := range e.Terms {
		n, _, _, _ := t.dims()
		if n <= maxDeg {
			out.Terms = append(out.Terms, t)
		}
	}
	return out
}

func triggerTarget(q *QueryResult) string {
	for _, n := range q.Notes {
		if strings.HasPrefix(n, "fk-child:") {
			return strings.TrimPrefix(n, "fk-child:")
		}
	}
	return ""
}

func nextStep(pts []ScalePoint, i int) float64 {
	if i+1 < len(pts) {
		return pts[i+1].Step
	}
	return 1
}

// AnalyzeRoute composes per-query verdicts into the route verdict.
func AnalyzeRoute(r *OpResult, p95Limit float64, maxQ float64) {
	var e Expr
	minConf := 2
	for _, q := range r.Queries {
		if q.Kind == "other" || q.BigO == "" {
			continue
		}
		qe := q.Expr
		if len(qe.Terms) == 0 {
			qe = Expr{Terms: []Term{T(nil, nil)}}
		}
		if q.WorkKExp > 0 {
			qe = qe.Add(Expr{Terms: []Term{T(map[string]float64{"k": q.WorkKExp}, nil)}})
		}
		if q.CountKExp > 0 {
			qe = qe.MulTerm(T(map[string]float64{"k": q.CountKExp}, nil))
		}
		e = e.Add(qe)
		c := map[string]int{"high": 2, "medium": 1, "low": 0}[q.Confidence]
		if c < minConf {
			minConf = c
		}
	}
	if r.AppKExp >= 0.8 {
		e = e.Add(Expr{Terms: []Term{T(map[string]float64{"k": r.AppKExp}, nil)}})
	}
	e = e.Simplify()
	r.BigO = e.String()
	r.Dominant = e.Dominant()
	r.Confidence = []string{"low", "medium", "high"}[minConf]
	if len(r.Queries) == 0 {
		r.Confidence = "n/a"
	}
	status := "OK"
	bump := func(s, why string) {
		rank := map[string]int{"OK": 0, "WARN": 1, "FAIL": 2}
		if rank[s] > rank[status] {
			status = s
		}
		r.Reasons = append(r.Reasons, why)
	}
	if d := e.NDegree(); d >= 2 {
		bump("FAIL", fmt.Sprintf("superlinear in data size (%s)", r.BigO))
	} else if d >= 1 {
		bump("FAIL", "cost grows linearly with table size ("+r.Dominant+")")
	}
	if len(r.NPlusOne) > 0 {
		bump("WARN", "N+1 queries")
	}
	if r.AppKExp >= 1.6 {
		bump("WARN", fmt.Sprintf("app-side work grows ~k^%.1f", r.AppKExp))
	}
	if p95Limit > 0 && r.Latency.P95 > p95Limit {
		bump("FAIL", fmt.Sprintf("p95 %.0fms > %.0fms", r.Latency.P95, p95Limit))
	}
	if maxQ > 0 && r.QPerReq > maxQ {
		bump("WARN", fmt.Sprintf("%.0f queries/request > %.0f", r.QPerReq, maxQ))
	}
	bad := 0
	for c, n := range r.Statuses {
		if c >= 400 {
			bad += n
		}
	}
	if bad > 0 {
		bump("WARN", fmt.Sprintf("%d non-2xx responses", bad))
	}
	r.Status = status
	// projection from per-query time fits
	if r.Latency.P50 > 0 {
		proj := map[string]float64{}
		for _, mult := range []float64{10, 100} {
			extra := 0.0
			ok := false
			for _, q := range r.Queries {
				if q.TimeFit == nil || !q.TimeFit.OK || len(q.Scale) == 0 {
					continue
				}
				n := q.Scale[len(q.Scale)-1].N
				for _, p := range q.Scale {
					n = math.Max(n, p.N)
				}
				cnt := q.CountPerReq
				if cnt == 0 {
					cnt = 1
				}
				now := q.TimeFit.Predict(n)
				if now <= 0 {
					continue
				}
				base := q.ObservedMs // scale what the app actually sees
				if base <= 0 {
					base = q.BaseMs
				}
				extra += cnt * base * math.Max(0, q.TimeFit.Predict(n*mult)/now-1)
				ok = true
			}
			if ok {
				proj[fmt.Sprintf("%gx", mult)] = math.Round(r.Latency.P50 + extra)
			}
		}
		if len(proj) > 0 {
			r.Projection = proj
		}
	}
}
