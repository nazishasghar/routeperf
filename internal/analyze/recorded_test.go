package analyze

import "testing"

// Points recorded from the testbed: a real O(k²) loop vs a linear endpoint with
// a GC bump at k=1000.
func TestDominantDegreeRecorded(t *testing.T) {
	mk := func(v ...float64) []Point {
		ks := []float64{1, 10, 100, 250, 500, 1000}
		var p []Point
		for i, k := range ks {
			p = append(p, Point{k, v[i]})
		}
		return p
	}
	if d := DominantDegree(mk(0.92, 1.13, 2.73, 6.77, 15.29, 40.04)); d != 2 {
		t.Errorf("recommendations (pg): got %v want 2", d)
	}
	if d := DominantDegree(mk(0.65, 1.08, 2.74, 8.0, 15.9, 54.25)); d != 2 {
		t.Errorf("recommendations (mysql): got %v want 2", d)
	}
	if d := DominantDegree(mk(0.83, 0.86, 1.65, 1.83, 3.02, 8.42)); d == 2 {
		t.Errorf("list users: got %v, want not quadratic", d)
	}
}

// Rows examined by `WHERE status = $1 ORDER BY id DESC LIMIT 1` on nested
// 1%→100% subsets (testbed, keyset-paginated tRPC order.search): the PK walk
// stops at the first match, so the count jitters. Bounded, not O(n²).
func TestBoundedWorkRecorded(t *testing.T) {
	ns := []float64{800, 2680, 9480, 30280, 100000}
	mk := func(v ...float64) []Point {
		var p []Point
		for i, n := range ns {
			p = append(p, Point{n, v[i]})
		}
		return p
	}
	for _, w := range [][]float64{{2, 7, 20, 7, 67}, {2, 7, 20, 7, 31}} {
		if _, _, ok := boundedWork(mk(w...)); !ok {
			t.Errorf("%v: want bounded work", w)
		}
	}
	// growth stays growth: seq scans, and a noisy linear count
	for _, w := range [][]float64{{800, 2680, 9480, 30280, 100000}, {120, 160, 700, 2000, 9100}, {5, 9, 30, 25, 95}} {
		if s, hw, ok := boundedWork(mk(w...)); ok {
			t.Errorf("%v: growth classed as bounded (slope %s)", w, FmtSlope(s, hw))
		}
	}
}
