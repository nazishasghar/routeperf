package runner

import (
	"reflect"
	"testing"

	"github.com/nazishasghar/routeperf/internal/spec"
)

func TestUnmatchedPatterns(t *testing.T) {
	ops := []*spec.Operation{
		{ID: "listUsers", Method: "GET", Path: "/users", Tags: []string{"user"}},
		{ID: "getOrder", Method: "GET", Path: "/orders/{id}"},
	}
	tests := []struct {
		name             string
		include, exclude []string
		want             []string
	}{
		{"no patterns", nil, nil, nil},
		{"id, method+glob and tag all match", []string{"listUsers", "GET /orders/**", "tag:user"}, nil, nil},
		{"case-insensitive id", []string{"LISTUSERS"}, nil, nil},
		{"typo in include", []string{"listUser"}, nil, []string{`"listUser"`}},
		{"unknown tag in exclude", nil, []string{"tag:ghost"}, []string{`"tag:ghost"`}},
		{"wrong method for path", []string{"POST /users"}, nil, []string{`"POST /users"`}},
		{"include then exclude order", []string{"nope"}, []string{"nada"}, []string{`"nope"`, `"nada"`}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Runner{cfg: &Config{}, sp: &spec.Spec{Ops: ops}}
			r.cfg.Run.Include, r.cfg.Run.Exclude = tt.include, tt.exclude
			if got := r.unmatchedPatterns(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("unmatchedPatterns(include=%v, exclude=%v) = %v, want %v", tt.include, tt.exclude, got, tt.want)
			}
		})
	}
}
