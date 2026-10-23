package report

import "testing"

func TestHumanRoundsBeforePickingUnit(t *testing.T) {
	for in, want := range map[float64]string{
		950: "950", 12000: "12k", 999600: "1.0M", 999400: "999k", 2500000: "2.5M",
	} {
		if got := human(in); got != want {
			t.Errorf("human(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestFmtNumRoundsBeforePickingUnit(t *testing.T) {
	for in, want := range map[float64]string{
		999960: "1M", 999960000: "1B", 1500: "1.5k", 2400000: "2.4M", 0.05: "0.05", 42: "42", -999960: "-1M",
	} {
		if got := fmtNum(in); got != want {
			t.Errorf("fmtNum(%v) = %q, want %q", in, got, want)
		}
	}
}
