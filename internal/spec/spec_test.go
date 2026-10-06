package spec

import "testing"

func TestMatch(t *testing.T) {
	users := &Operation{ID: "getUser", Method: "GET", Path: "/admin/users/{id}", Tags: []string{"Admin"}}
	cases := map[string]bool{
		"*":                       true,
		"getuser":                 true,
		"tag:admin":               true,
		"tag:driver":              false,
		"GET /admin/users/{id}":   true,
		"POST /admin/users/{id}":  false,
		"/admin/**":               true,
		"/admin/*":                false, // * stays within one segment
		"/admin/users/*":          true,
		"GET /admin/users/*":      true,
		"* /admin/**":             true,
		"DELETE /admin/**":        false,
		"/admin/users/{id}/**":    true, // trailing /** also matches the bare prefix
		"/drivers/**":             false,
		"admin":                   false, // not a path, tag or operationId
		"/admin/users/{id}/extra": false,
	}
	for p, want := range cases {
		if got := Match(p, users); got != want {
			t.Errorf("Match(%q) = %v, want %v", p, got, want)
		}
	}
	if !MatchAny([]string{"tag:x", "/admin/**"}, users) || MatchAny(nil, users) {
		t.Error("MatchAny")
	}
}
