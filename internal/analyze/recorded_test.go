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
