// Package analyze turns measurements into complexity classes: curve fitting
// (Google Benchmark style normalized-RMS selection with an Occam rule and a
// trend-prof style log-log slope), static plan algebra, and route verdicts.
package analyze

import (
	"math"
	"sort"
)

type Class int

const (
	O1 Class = iota
	OLogN
	ON
	ONLogN
	ON2
	ON3
)

var classNames = map[Class]string{O1: "O(1)", OLogN: "O(log n)", ON: "O(n)", ONLogN: "O(n log n)", ON2: "O(n^2)", ON3: "O(n^3)"}

func (c Class) String() string { return classNames[c] }

// Degree is the polynomial degree of the class (log factors ignored).
func (c Class) Degree() int {
	switch c {
	case O1, OLogN:
		return 0
	case ON, ONLogN:
		return 1
	case ON2:
		return 2
	}
	return 3
}

func (c Class) HasLog() bool { return c == OLogN || c == ONLogN }

func classFn(c Class, x float64) float64 {
	switch c {
	case O1:
		return 0
	case OLogN:
		return math.Log(x)
	case ON:
		return x
	case ONLogN:
		return x * math.Log(x)
	case ON2:
		return x * x
	}
	return x * x * x
}

type Point struct{ X, Y float64 }

type Fit struct {
	Class      Class              `json:"-"`
	ClassName  string             `json:"class"`
	A          float64            `json:"intercept"`
	B          float64            `json:"coef"`
	R2         float64            `json:"r2"`
	NRMSE      float64            `json:"nrmse"`
	Slope      float64            `json:"slope"` // log-log exponent
	Points     int                `json:"points"`
	Decades    float64            `json:"decades"`
	SlopeAgree bool               `json:"slope_agrees"`
	All        map[string]float64 `json:"candidates"`
	OK         bool               `json:"ok"`
}

// Predict evaluates the fitted model at x.
func (f Fit) Predict(x float64) float64 { return f.A + f.B*classFn(f.Class, x) }

// FitCurve picks the simplest class whose normalized RMS error is within 10%
// of the best candidate. Points are aggregated (median) per distinct X.
func FitCurve(pts []Point, maxClass Class) Fit {
	pts = medianByX(pts)
	f := Fit{Points: len(pts), All: map[string]float64{}}
	if len(pts) < 3 {
		return f
	}
	minX, maxX := pts[0].X, pts[len(pts)-1].X
	if minX <= 0 {
		return f
	}
	f.Decades = math.Log10(maxX / minX)
	var meanY float64
	for _, p := range pts {
		meanY += p.Y
	}
	meanY /= float64(len(pts))
	if meanY <= 0 {
		f.Class, f.ClassName, f.OK = O1, O1.String(), true
		return f
	}
	type cand struct {
		c     Class
		a, b  float64
		nrmse float64
	}
	var cands []cand
	for c := O1; c <= maxClass; c++ {
		a, b := nnls(pts, c)
		var ss float64
		for _, p := range pts {
			r := p.Y - (a + b*classFn(c, p.X))
			ss += r * r
		}
		nr := math.Sqrt(ss/float64(len(pts))) / meanY
		cands = append(cands, cand{c, a, b, nr})
		f.All[c.String()] = round4(nr)
	}
	best := cands[0]
	for _, c := range cands {
		if c.nrmse < best.nrmse {
			best = c
		}
	}
	chosen := best
	for _, c := range cands { // ordered simplest → most complex
		if c.nrmse <= best.nrmse*1.10+0.005 {
			chosen = c
			break
		}
	}
	f.Class, f.ClassName, f.A, f.B, f.NRMSE = chosen.c, chosen.c.String(), chosen.a, chosen.b, chosen.nrmse
	// R²
	var ssTot, ssRes float64
	for _, p := range pts {
		ssTot += (p.Y - meanY) * (p.Y - meanY)
		r := p.Y - f.Predict(p.X)
		ssRes += r * r
	}
	if ssTot > 0 {
		f.R2 = 1 - ssRes/ssTot
	} else {
		f.R2 = 1
	}
	f.Slope = LogLogSlope(pts, f.A)
	f.SlopeAgree = slopeAgrees(f.Slope, chosen.c)
	f.OK = true
	return f
}

// LogLogSlope fits log(y − a) = c + s·log(x) by OLS and returns s.
func LogLogSlope(pts []Point, a float64) float64 {
	var xs, ys []float64
	for _, p := range pts {
		y := p.Y - a
		if p.X <= 0 || y <= 0 {
			continue
		}
		xs = append(xs, math.Log(p.X))
		ys = append(ys, math.Log(y))
	}
	if len(xs) < 2 {
		return 0
	}
	return olsSlope(xs, ys)
}

// GrowthExponent is the log-log slope of y on x without intercept handling;
// used for per-node row and loop growth. Zero-valued points count as 1.
func GrowthExponent(pts []Point) float64 {
	pts = medianByX(pts)
	var xs, ys []float64
	for _, p := range pts {
		if p.X <= 0 {
			continue
		}
		xs = append(xs, math.Log(p.X))
		ys = append(ys, math.Log(math.Max(p.Y, 1)))
	}
	if len(xs) < 2 {
		return 0
	}
	return olsSlope(xs, ys)
}

// RoundExp snaps an exponent to the nearest half-integer when close.
func RoundExp(e float64) float64 {
	if e < 0.2 {
		return 0
	}
	r := math.Round(e)
	if math.Abs(e-r) <= 0.25 {
		return r
	}
	return math.Round(e*2) / 2
}

func slopeAgrees(s float64, c Class) bool {
	d := float64(c.Degree())
	if c == OLogN || c == O1 {
		return s < 0.35
	}
	return math.Abs(s-d) <= 0.3 || (c == ONLogN && s >= 0.9 && s <= 1.5)
}

func olsSlope(xs, ys []float64) float64 {
	var mx, my float64
	for i := range xs {
		mx += xs[i]
		my += ys[i]
	}
	n := float64(len(xs))
	mx, my = mx/n, my/n
	var sxy, sxx float64
	for i := range xs {
		sxy += (xs[i] - mx) * (ys[i] - my)
		sxx += (xs[i] - mx) * (xs[i] - mx)
	}
	if sxx == 0 {
		return 0
	}
	return sxy / sxx
}

// nnls solves y ≈ a + b·g(x) with a, b ≥ 0.
func nnls(pts []Point, c Class) (float64, float64) {
	n := float64(len(pts))
	var my float64
	for _, p := range pts {
		my += p.Y
	}
	my /= n
	if c == O1 {
		return my, 0
	}
	var mg float64
	for _, p := range pts {
		mg += classFn(c, p.X)
	}
	mg /= n
	var sgy, sgg float64
	for _, p := range pts {
		g := classFn(c, p.X)
		sgy += (g - mg) * (p.Y - my)
		sgg += (g - mg) * (g - mg)
	}
	if sgg == 0 {
		return my, 0
	}
	b := sgy / sgg
	a := my - b*mg
	if b < 0 {
		return my, 0
	}
	if a < 0 { // through origin: b = Σ(g·y)/Σ(g²)
		var gy, gg float64
		for _, p := range pts {
			g := classFn(c, p.X)
			gy += g * p.Y
			gg += g * g
		}
		if gg == 0 {
			return my, 0
		}
		return 0, math.Max(gy/gg, 0)
	}
	return a, b
}

func medianByX(pts []Point) []Point {
	g := map[float64][]float64{}
	for _, p := range pts {
		g[p.X] = append(g[p.X], p.Y)
	}
	out := make([]Point, 0, len(g))
	for x, ys := range g {
		out = append(out, Point{x, Median(ys)})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].X < out[j].X })
	return out
}

func Median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	m := len(s) / 2
	if len(s)%2 == 1 {
		return s[m]
	}
	return (s[m-1] + s[m]) / 2
}

func Percentile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	i := int(math.Ceil(p/100*float64(len(s)))) - 1
	if i < 0 {
		i = 0
	}
	if i >= len(s) {
		i = len(s) - 1
	}
	return s[i]
}

func round4(v float64) float64 { return math.Round(v*1e4) / 1e4 }

// DominantDegree fits y = a + b·x + c·x² (non-negative) and returns the
// degree of the term that dominates at the largest x (0, 1 or 2). This
// catches mixtures like serialization O(k) + nested loop O(k²).
func DominantDegree(pts []Point) float64 {
	pts = medianByX(pts)
	if len(pts) < 3 {
		return 0
	}
	solve := func(fs []func(float64) float64) []float64 {
		n := len(fs)
		a := make([][]float64, n)
		for i := range a {
			a[i] = make([]float64, n+1)
		}
		for _, p := range pts {
			for i := 0; i < n; i++ {
				for j := 0; j < n; j++ {
					a[i][j] += fs[i](p.X) * fs[j](p.X)
				}
				a[i][n] += fs[i](p.X) * p.Y
			}
		}
		for c := 0; c < n; c++ { // Gaussian elimination
			piv := c
			for r := c + 1; r < n; r++ {
				if math.Abs(a[r][c]) > math.Abs(a[piv][c]) {
					piv = r
				}
			}
			a[c], a[piv] = a[piv], a[c]
			if a[c][c] == 0 {
				return nil
			}
			for r := 0; r < n; r++ {
				if r != c {
					f := a[r][c] / a[c][c]
					for k := c; k <= n; k++ {
						a[r][k] -= f * a[c][k]
					}
				}
			}
		}
		out := make([]float64, n)
		for i := range out {
			out[i] = a[i][n] / a[i][i]
		}
		return out
	}
	one := func(float64) float64 { return 1 }
	lin := func(x float64) float64 { return x }
	sq := func(x float64) float64 { return x * x }
	xmax := pts[len(pts)-1].X
	rmse := func(co []float64, fs []func(float64) float64) float64 {
		var ss float64
		for _, p := range pts {
			var y float64
			for i, f := range fs {
				y += co[i] * f(p.X)
			}
			ss += (p.Y - y) * (p.Y - y)
		}
		return math.Sqrt(ss / float64(len(pts)))
	}
	linF, quadF := []func(float64) float64{one, lin}, []func(float64) float64{one, lin, sq}
	coL := solve(linF)
	if co := solve(quadF); co != nil && co[2] > 0 && len(pts) >= 5 && coL != nil {
		b := math.Max(co[1], 0)
		pred := math.Max(co[0], 0) + b*xmax + co[2]*xmax*xmax
		// quadratic must cost ≥10ms at the biggest k, be ≥30% of it, and fit clearly better than a line
		if q := co[2] * xmax * xmax; pred > 0 && q/pred >= 0.3 && q >= 10 && rmse(coL, linF) > 1.5*rmse(co, quadF) {
			return 2
		}
	}
	if co := solve([]func(float64) float64{one, lin}); co != nil && co[1] > 0 {
		pred := math.Max(co[0], 0) + co[1]*xmax
		if pred > 0 && co[1]*xmax/pred > 0.5 {
			return 1
		}
	}
	return 0
}
