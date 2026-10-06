package analyze

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// t975 holds the 97.5% quantile of Student's t for 1..30 degrees of freedom.
var t975 = []float64{12.706, 4.303, 3.182, 2.776, 2.571, 2.447, 2.365, 2.306, 2.262, 2.228,
	2.201, 2.179, 2.160, 2.145, 2.131, 2.120, 2.110, 2.101, 2.093, 2.086,
	2.080, 2.074, 2.069, 2.064, 2.060, 2.056, 2.052, 2.048, 2.045, 2.042}

func tCrit(df int) float64 {
	switch {
	case df < 1:
		return math.Inf(1)
	case df <= len(t975):
		return t975[df-1]
	case df <= 60:
		return 2.0
	}
	return 1.96
}

// SlopeCI fits log(y) = c + s·log(x) by least squares over every point (no
// aggregation, so repeats tighten it) and returns s with its 95% confidence
// half-width. ok is false with fewer than two distinct x values.
func SlopeCI(pts []Point) (s, hw float64, ok bool) {
	var xs, ys []float64
	distinct := map[float64]bool{}
	for _, p := range pts {
		if p.X <= 0 || p.Y <= 0 {
			continue
		}
		xs = append(xs, math.Log(p.X))
		ys = append(ys, math.Log(p.Y))
		distinct[p.X] = true
	}
	if len(distinct) < 2 {
		return 0, math.Inf(1), false
	}
	n := float64(len(xs))
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	mx, my = mx/n, my/n
	var sxx, sxy float64
	for i := range xs {
		sxx += (xs[i] - mx) * (xs[i] - mx)
		sxy += (xs[i] - mx) * (ys[i] - my)
	}
	if sxx == 0 {
		return 0, math.Inf(1), false
	}
	s = sxy / sxx
	if len(xs) <= 2 {
		return s, math.Inf(1), true
	}
	var ssr float64
	for i := range xs {
		r := ys[i] - (my + s*(xs[i]-mx))
		ssr += r * r
	}
	se := math.Sqrt(ssr / (n - 2) / sxx)
	return s, tCrit(len(xs)-2) * se, true
}

// Degree turns a measured exponent and its CI into a degree: the integer
// inside the interval when there is exactly one, otherwise the exponent
// snapped to the nearest half. sure reports a single supported integer.
func Degree(s, hw float64) (deg float64, sure bool) {
	if s < 0 {
		s = 0
	}
	lo, hi := s-hw, s+hw
	var ints []float64
	for d := math.Ceil(math.Max(lo, 0)); d <= math.Floor(hi) && d <= 4; d++ {
		ints = append(ints, d)
	}
	if len(ints) == 1 {
		return ints[0], true
	}
	return RoundExp(s), false
}

// FmtSlope renders "1.02 ± 0.04".
func FmtSlope(s, hw float64) string {
	if math.IsInf(hw, 0) || math.IsNaN(hw) {
		return fmt.Sprintf("%.2f", s)
	}
	return fmt.Sprintf("%.2f ± %.2f", s, hw)
}

// TableGrowth is how a query's rows examined grow when only one table
// shrinks (the others stay at 100%).
type TableGrowth struct {
	Table  string       `json:"table"`
	Slope  float64      `json:"slope"`
	CI     float64      `json:"ci"`
	Points []ScalePoint `json:"points"`
}

func (g TableGrowth) Exp() float64 {
	d, _ := Degree(g.Slope, g.CI)
	return d
}

// perTableExpr builds the measured expression from per-table exponents,
// deciding product vs sum by comparing with the joint (all tables shrink
// together) exponent: a nested loop multiplies, a hash join adds.
func perTableExpr(q *QueryResult, joint, jointCI float64) (Expr, string, bool) {
	var grow []TableGrowth
	var notes []string
	tg := append([]TableGrowth(nil), q.PerTable...)
	sort.Slice(tg, func(i, j int) bool { return tg[i].Slope > tg[j].Slope })
	sum := 0.0
	for _, g := range tg {
		notes = append(notes, fmt.Sprintf("%s %s", g.Table, FmtSlope(g.Slope, g.CI)))
		if e := g.Exp(); e > 0 {
			grow = append(grow, g)
			sum += e
		}
	}
	note := "per-table growth (others held at 100%): " + strings.Join(notes, ", ")
	if len(grow) == 0 {
		return Expr{}, note, true
	}
	tol := math.Max(0.35, jointCI)
	if len(grow) >= 2 && math.Abs(sum-joint) <= tol {
		vars := map[string]float64{}
		for _, g := range grow {
			vars[sym(g.Table)] = g.Exp()
		}
		return Expr{Terms: []Term{T(vars, nil)}}, note + "; growth multiplies (nested)", true
	}
	var e Expr
	maxE := 0.0
	for _, g := range grow {
		e.Terms = append(e.Terms, T(map[string]float64{sym(g.Table): g.Exp()}, nil))
		maxE = math.Max(maxE, g.Exp())
	}
	consistent := math.Abs(maxE-joint) <= tol
	if len(grow) >= 2 {
		note += "; growth adds (independent scans)"
	}
	return e, note, consistent
}

// PolyDegree fits y = c0 + c1·x + c2·x² by least squares over every sample
// and returns the highest degree whose coefficient is positive with 95%
// confidence and contributes at least a fifth of the prediction at the
// largest x, plus that term's share there. It finds the dominant term of
// mixtures (serialization O(k) + a nested loop O(k²)) whose log-log slope
// sits between the two degrees over a finite range.
func PolyDegree(pts []Point) (deg int, share float64) {
	if len(pts) < 4 {
		return 0, 0
	}
	xmax := 0.0
	for _, p := range pts {
		xmax = math.Max(xmax, p.X)
	}
	try := func(n int) (co, hw []float64, ok bool) {
		// normal equations with scaled x for conditioning
		k := n + 1
		a := make([][]float64, k)
		for i := range a {
			a[i] = make([]float64, k+1)
		}
		for _, p := range pts {
			x := p.X / xmax
			f := make([]float64, k)
			for j := range f {
				f[j] = math.Pow(x, float64(j))
			}
			for i := 0; i < k; i++ {
				for j := 0; j < k; j++ {
					a[i][j] += f[i] * f[j]
				}
				a[i][k] += f[i] * p.Y
			}
		}
		inv := invert(a, k)
		if inv == nil {
			return nil, nil, false
		}
		co = make([]float64, k)
		for i := 0; i < k; i++ {
			for j := 0; j < k; j++ {
				co[i] += inv[i][j] * a[j][k]
			}
		}
		var ssr float64
		for _, p := range pts {
			x := p.X / xmax
			y := 0.0
			for j := range co {
				y += co[j] * math.Pow(x, float64(j))
			}
			ssr += (p.Y - y) * (p.Y - y)
		}
		df := len(pts) - k
		if df < 1 {
			return nil, nil, false
		}
		s2 := ssr / float64(df)
		hw = make([]float64, k)
		for j := range hw {
			hw[j] = tCrit(df) * math.Sqrt(math.Max(s2*inv[j][j], 0))
		}
		return co, hw, true
	}
	for n := 2; n >= 1; n-- {
		co, hw, ok := try(n)
		if !ok {
			continue
		}
		pred := 0.0
		for j := range co {
			pred += math.Max(co[j], 0) // x/xmax = 1 at the largest x
		}
		if co[n]-hw[n] > 0 && pred > 0 && co[n]/pred >= 0.2 {
			return n, co[n] / pred
		}
	}
	return 0, 0
}

// invert returns the inverse of the k×k block of a (Gauss-Jordan), or nil.
func invert(a [][]float64, k int) [][]float64 {
	m := make([][]float64, k)
	for i := range m {
		m[i] = make([]float64, 2*k)
		copy(m[i], a[i][:k])
		m[i][k+i] = 1
	}
	for c := 0; c < k; c++ {
		piv := c
		for r := c + 1; r < k; r++ {
			if math.Abs(m[r][c]) > math.Abs(m[piv][c]) {
				piv = r
			}
		}
		if math.Abs(m[piv][c]) < 1e-12 {
			return nil
		}
		m[c], m[piv] = m[piv], m[c]
		d := m[c][c]
		for j := range m[c] {
			m[c][j] /= d
		}
		for r := 0; r < k; r++ {
			if r != c {
				f := m[r][c]
				for j := range m[r] {
					m[r][j] -= f * m[c][j]
				}
			}
		}
	}
	out := make([][]float64, k)
	for i := range out {
		out[i] = m[i][k:]
	}
	return out
}
