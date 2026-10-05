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

// replay groups captured statements per fingerprint and runs EXPLAIN ANALYZE
// at 100%, on each data subset, and at each k step.
func (r *Runner) replay(ctx context.Context, res *analyze.OpResult) {
	type agg struct {
		q      *analyze.QueryResult
		counts []float64
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
			var d float64
			for _, st := range s.Stmts {
				if st.Kind == "other" {
					continue
				}
				d += st.DurMs
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
			tdb = append(tdb, d)
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
		minC, maxC := math.Inf(1), 0.0
		for _, k := range ks {
			c := m[k]
			minC, maxC = math.Min(minC, c), math.Max(maxC, c)
		}
		if len(ks) >= 2 && maxC >= 3 && maxC > minC*1.5 {
			nplus[fp] = true
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
		p, work, ms, ex, err := r.explainStmt(ctx, q.Example, "")
		if err != nil {
			q.Error = err.Error()
			res.Queries = append(res.Queries, q)
			continue
		}
		q.Example = ex
		q.Base, q.BaseWork, q.BaseMs = p, work, ms
		dom := dominantRel(p, ex)
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
				sp, sw, sms, _, err := r.explainStmt(ctx, ex, db.NSName(s))
				pt := analyze.ScalePoint{Step: s, N: nRows}
				if err != nil {
					pt.Errors = err.Error()
					q.Scale = append(q.Scale, pt)
					continue
				}
				pt.Work, pt.Ms, pt.Shape, pt.Plan = sw, sms, sp.ShapeHash(), sp
				q.Scale = append(q.Scale, pt)
			}
			if full > 0 {
				q.Scale = append(q.Scale, analyze.ScalePoint{Step: 1, N: full, Work: work, Ms: ms, Shape: p.ShapeHash(), Plan: p})
			}
			var good []analyze.ScalePoint
			for _, pt := range q.Scale {
				if pt.Errors == "" {
					good = append(good, pt)
				}
			}
			q.Scale = good
		}
		for _, k := range ks {
			st, ok := kStmt[fp][k]
			if !ok {
				continue
			}
			kp := analyze.KPoint{K: k, Count: kCount[fp][k]}
			if _, w, ms, _, err := r.explainStmt(ctx, st, ""); err == nil {
				kp.Work, kp.Ms = w, ms
			}
			q.KPoints = append(q.KPoints, kp)
		}
		q.WorkKExp = maxLocalSlope(q.KPoints)
		res.Queries = append(res.Queries, q)
	}
	// Per-request DB cost: logged durations (PG) or replay estimate (MySQL).
	var dbms []float64
	for _, s := range res.Samples {
		var d float64
		for _, st := range s.Stmts {
			d += st.DurMs
		}
		dbms = append(dbms, d)
	}
	res.DBMs = analyze.Median(dbms)
	for _, q := range res.Queries {
		cnt := math.Max(q.CountPerReq, 1)
		res.RowsPerReq += cnt * q.BaseWork
		if res.DBMs == 0 {
			defer func(c, ms float64) { res.DBMs += c * ms }(cnt, q.BaseMs)
		}
	}
	// MySQL logs carry no durations: estimate DB time from replays.
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
		maxApp := 0.0
		for _, k := range ks {
			app := math.Max(0, res.KLatency[k]-kTdb[k])
			pts = append(pts, analyze.Point{X: float64(k), Y: app})
			maxApp = math.Max(maxApp, app)
		}
		f := analyze.FitCurve(pts, analyze.ON3)
		res.AppFit = &f
		first, last := pts[0].Y, pts[len(pts)-1].Y
		if maxApp > 2 && last-first > 2 && last > 2*first+1 { // real growth, not jitter
			res.AppKExp = analyze.DominantDegree(pts)
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

func maxLocalSlope(kp []analyze.KPoint) float64 {
	best := 0.0
	for i := 1; i < len(kp); i++ {
		a, b := kp[i-1], kp[i]
		if a.Work <= 0 || b.Work <= 0 || a.K <= 0 || b.K <= a.K {
			continue
		}
		s := math.Log(b.Work/a.Work) / math.Log(float64(b.K)/float64(a.K))
		best = math.Max(best, s)
	}
	switch {
	case best >= 1.6:
		return 2
	case best >= 0.6:
		return 1
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

func dominantRel(p *plan.Plan, st analyze.Stmt) string {
	best, bv := "", -1.0
	p.Walk(func(n *plan.Node, _ int) {
		if n.Scan && n.Relation != "" && n.Rows+n.RowsRemoved > bv {
			best, bv = strings.ToLower(n.Relation), n.Rows+n.RowsRemoved
		}
	})
	if best == "" {
		if t := sqlutil.TargetTable(st.SQL); t != "" {
			return t
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

var idEq = regexp.MustCompile(`(?i)("?(\w+)"?\s*=\s*)'?(\d+)'?`)

// retarget swaps a missing row id (deleted/created by the app) for an anchor id
// so DML replays exercise real rows (and FK cascades).
func (r *Runner) retarget(ctx context.Context, st analyze.Stmt) (analyze.Stmt, bool) {
	sql := inlined(st)
	m := idEq.FindStringSubmatchIndex(sql)
	if m == nil {
		return st, false
	}
	col := strings.ToLower(sql[m[4]:m[5]])
	var t *db.Table
	if col == "id" {
		t = r.tables[sqlutil.TargetTable(sql)]
	} else if mm := regexp.MustCompile(`^(\w+?)_?id$`).FindStringSubmatch(col); mm != nil {
		for _, c := range []string{mm[1] + "s", mm[1], mm[1] + "es"} {
			if r.tables[c] != nil {
				t = r.tables[c]
				break
			}
		}
	}
	if t == nil || t.PK == "" {
		return st, false
	}
	a := r.res.Anchors(ctx, t)
	if len(a) == 0 {
		return st, false
	}
	out := st
	out.SQL = sql[:m[6]] + a[len(a)-1] + sql[m[7]:]
	out.Params = nil
	return out, true
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
// rows examined and median execution time. Handles unique violations and
// missing DML targets.
func (r *Runner) explainStmt(ctx context.Context, st analyze.Stmt, ns string) (*plan.Plan, float64, float64, analyze.Stmt, error) {
	var last *plan.Plan
	var times []float64
	cur := st
	retargeted := false
	for i := 0; i <= r.cfg.Scale.Repeats; i++ {
		var p *plan.Plan
		var err error
		for attempt := 0; attempt < 4; attempt++ {
			p, err = r.db.Explain(ctx, cur, ns, r.tables)
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
			return nil, 0, 0, cur, err
		}
		if i == 0 && !retargeted && cur.Kind != "insert" && scanRows(p) == 0 {
			if nst, ok := r.retarget(ctx, cur); ok {
				cur, retargeted = nst, true
				i = -1
				continue
			}
		}
		if i > 0 || r.cfg.Scale.Repeats == 0 {
			times = append(times, p.TotalMs())
		}
		last = p
	}
	if last == nil {
		return nil, 0, 0, cur, fmt.Errorf("no plan")
	}
	return last, last.RowsExamined(), analyze.Median(times), cur, nil
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
