package runner

import (
	"reflect"
	"testing"

	"github.com/nazishasghar/routeperf/internal/spec"
)

func TestUnmatchedPatterns(t *testing.T) {
	r := &Runner{cfg: &Config{}, sp: &spec.Spec{Ops: []*spec.Operation{
		{ID: "listUsers", Method: "GET", Path: "/users", Tags: []string{"user"}},
		{ID: "getOrder", Method: "GET", Path: "/orders/{id}"},
	}}}
	r.cfg.Run.Include = []string{"listUsers", "GET /orders/**", "tag:user", "nope"}
	r.cfg.Run.Exclude = []string{"tag:ghost"}
	got := r.unmatchedPatterns()
	want := []string{`"nope"`, `"tag:ghost"`}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}
