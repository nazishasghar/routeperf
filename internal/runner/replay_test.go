package runner

import (
	"testing"

	"github.com/nazishasghar/routeperf/internal/analyze"
)

func TestPadZerosKeepsMedianHonest(t *testing.T) {
	// a statement ran once in 2 of 10 requests: the typical request never runs it
	got := analyze.Median(padZeros([]float64{1, 1}, 10))
	if got != 0 {
		t.Fatalf("median = %v, want 0", got)
	}
	if v := padZeros([]float64{3, 3, 3}, 2); len(v) != 3 {
		t.Fatalf("longer input must be left alone, got %v", v)
	}
}
