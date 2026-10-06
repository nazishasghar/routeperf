package analyze

import (
	"math"
	"testing"
)

func pts(f func(x float64) float64) []Point {
	var out []Point
	for _, x := range []float64{2000, 6000, 20000, 60000, 200000} {
		out = append(out, Point{x, f(x)})
	}
	return out
}

func TestFitCurveClasses(t *testing.T) {
	cases := []struct {
		name string
		f    func(float64) float64
		want Class
	}{
		{"constant", func(x float64) float64 { return 42 }, O1},
		{"linear with intercept", func(x float64) float64 { return 5 + 0.3*x }, ON},
		{"quadratic", func(x float64) float64 { return 1e-6 * x * x }, ON2},
		{"log", func(x float64) float64 { return 3 * math.Log(x) }, OLogN},
	}
	for _, c := range cases {
		got := FitCurve(pts(c.f), ON3)
		if got.Class != c.want {
			t.Errorf("%s: got %s want %s (%v)", c.name, got.ClassName, c.want, got.All)
		}
	}
}

func TestDominantDegree(t *testing.T) {
	ks := []float64{1, 10, 100, 250, 500, 1000}
	mk := func(f func(float64) float64) []Point {
		var p []Point
		for _, k := range ks {
			p = append(p, Point{k, f(k)})
		}
		return p
	}
	if d := DominantDegree(mk(func(k float64) float64 { return 1 + 0.02*k + 2e-5*k*k })); d != 2 {
		t.Errorf("mixed k+k²: got %v want 2", d)
	}
	if d := DominantDegree(mk(func(k float64) float64 { return 1 + 0.01*k })); d != 1 {
		t.Errorf("linear: got %v want 1", d)
	}
}

func TestExprSimplify(t *testing.T) {
	e := Expr{Terms: []Term{
		T(map[string]float64{"n_orders": 1}, nil),
		T(nil, map[string]float64{"n_users": 1}),
		T(map[string]float64{"k": 1}, map[string]float64{"n_items": 1}),
		T(map[string]float64{"k": 1}, nil),
	}}
	if got := e.String(); got != "O(k·log n_items + n_orders)" && got != "O(n_orders + k·log n_items)" {
		t.Errorf("got %s", got)
	}
}

func TestFilterColsCasts(t *testing.T) {
	got := filterCols("((status)::text = 'paid'::text) AND (o.user_id = '42'::bigint)")
	if len(got) != 2 || got[0] != "status" || got[1] != "user_id" {
		t.Fatalf("got %v", got)
	}
	if n := IndexName("orders", []string{"user_id", "created_at DESC"}); n != "idx_orders_user_id_created_at" {
		t.Fatal(n)
	}
}

func TestPolyDegree(t *testing.T) {
	var mix, lin []Point
	for _, k := range []float64{1, 10, 100, 250, 500, 1000} {
		for r := 0; r < 3; r++ {
			noise := float64(r-1) * 0.05
			mix = append(mix, Point{k, 0.3 + 0.004*k + 0.00006*k*k + noise}) // k² term = 60ms of 64ms at k=1000
			lin = append(lin, Point{k, 0.3 + 0.01*k + noise})
		}
	}
	if d, share := PolyDegree(mix); d != 2 || share < 0.5 {
		t.Errorf("mixture: degree %d share %.2f, want 2", d, share)
	}
	if d, _ := PolyDegree(lin); d != 1 {
		t.Errorf("linear: degree %d, want 1", d)
	}
	if s, hw, ok := SlopeCI([]Point{{10, 10}, {100, 101}, {1000, 990}, {10, 11}, {100, 99}, {1000, 1010}}); !ok || s < 0.95 || s > 1.05 || hw > 0.05 {
		t.Errorf("slope %.3f ± %.3f", s, hw)
	}
	if d, sure := Degree(1.02, 0.04); d != 1 || !sure {
		t.Errorf("Degree(1.02±0.04) = %v %v", d, sure)
	}
	if _, sure := Degree(1.4, 0.8); sure {
		t.Errorf("a wide interval must not be sure")
	}
}
