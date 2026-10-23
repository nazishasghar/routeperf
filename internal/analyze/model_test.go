package analyze

import (
	"strings"
	"testing"
)

func TestSampleFailed(t *testing.T) {
	if !(Sample{Err: "connection refused"}).Failed() {
		t.Error("transport failure not Failed")
	}
	if (Sample{Status: 200, Err: "capture: boom"}).Failed() {
		t.Error("capture error with a real response must keep its latency")
	}
	if (Sample{Status: 500}).Failed() {
		t.Error("HTTP 500 is a real, timed response")
	}
}

func TestAnalyzeRouteConfidenceWithoutAnalyzedQueries(t *testing.T) {
	r := &OpResult{Queries: []*QueryResult{{Kind: "other", BigO: "O(1)"}}}
	AnalyzeRoute(r, 0, 0)
	if r.Confidence != "n/a" {
		t.Fatalf("nothing analysed, confidence = %q, want n/a", r.Confidence)
	}
}

func TestAnalyzeRouteReportsUnanalysedQueries(t *testing.T) {
	r := &OpResult{Queries: []*QueryResult{{Kind: "select", BigO: "unknown", Confidence: "low"}}}
	AnalyzeRoute(r, 0, 0)
	if len(r.Reasons) == 0 || r.Confidence != "low" {
		t.Fatalf("unknown query must be flagged: reasons %v, confidence %q", r.Reasons, r.Confidence)
	}
}

func TestAnalyzeRouteFlagsP95OnFewSamples(t *testing.T) {
	r := &OpResult{Latency: Latency{P95: 400, N: 5}}
	AnalyzeRoute(r, 200, 0)
	if r.Status != "FAIL" || !strings.Contains(strings.Join(r.Reasons, ";"), "5 samples") {
		t.Fatalf("status %s reasons %v", r.Status, r.Reasons)
	}
}
