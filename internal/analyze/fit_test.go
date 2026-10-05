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
