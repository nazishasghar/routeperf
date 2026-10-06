package runner

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/nazishasghar/routeperf/internal/auth"
	"github.com/nazishasghar/routeperf/internal/spec"
)

func roleOps() []*spec.Operation {
	return []*spec.Operation{
		{ID: "listOrders", Method: "GET", Path: "/orders", Phase: "R"},
		{ID: "getTrip", Method: "GET", Path: "/trips/{id}", Phase: "R"},
		{ID: "listUsers", Method: "GET", Path: "/admin/users", Tags: []string{"admin"}, Phase: "R"},
		{ID: "me", Method: "GET", Path: "/me", Phase: "R"},
	}
}

func jobNames(jobs []Job) []string {
	var out []string
	for _, j := range jobs {
		out = append(out, j.Op.ID+"@"+j.Role)
	}
	slices.Sort(out)
	return out
}

func newRoleRunner(t *testing.T, cfg *Config, schemes bool) *Runner {
	t.Helper()
	r := New(cfg, func(string, ...any) {})
	r.sp = &spec.Spec{Ops: roleOps()}
	if schemes {
		r.sp.Schemes = map[string]*openapi3.SecurityScheme{"bearerAuth": {Type: "http", Scheme: "bearer"}}
		r.sp.Global = openapi3.SecurityRequirements{{"bearerAuth": {}}}
	}
	var err error
	if r.am, err = auth.New(cfg.Auth.Creds, "http://localhost", time.Second); err != nil {
		t.Fatal(err)
	}
	r.roleAM = map[string]*auth.Manager{}
	for _, n := range cfg.RoleNames() {
		if r.roleAM[n], err = auth.New(cfg.Auth.Roles[n].Creds, "http://localhost", time.Second); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func TestSelectRunsOncePerMatchingRole(t *testing.T) {
	cfg := &Config{}
	cfg.Auth.Bearer = "cust"
	cfg.Auth.Roles = map[string]auth.Role{
		"admin":  {Creds: auth.Creds{Bearer: "adm"}, Ops: []string{"tag:admin", "GET /orders"}},
		"driver": {Creds: auth.Creds{Bearer: "drv"}, Ops: []string{"/trips/**", "listOrders"}},
	}
	r := newRoleRunner(t, cfg, true)
	jobs, skipped := r.Select()
	want := []string{"getTrip@driver", "listOrders@admin", "listOrders@driver", "listUsers@admin", "me@"}
	if got := jobNames(jobs); !slices.Equal(got, want) || len(skipped) != 0 {
		t.Errorf("jobs = %v, skipped %d; want %v", got, len(skipped), want)
	}
	if r.amFor("driver").Bearer() != "drv" || r.amFor("").Bearer() != "cust" || r.amFor("nobody").Bearer() != "cust" {
		t.Error("amFor picks the wrong credentials")
	}
}

func TestSelectSkipsUnclaimedOpsWithoutDefaultCreds(t *testing.T) {
	cfg := &Config{}
	cfg.Auth.Roles = map[string]auth.Role{"admin": {Creds: auth.Creds{Bearer: "adm"}, Ops: []string{"/admin/**"}}}
	r := newRoleRunner(t, cfg, true)
	jobs, skipped := r.Select()
	if got := jobNames(jobs); !slices.Equal(got, []string{"listUsers@admin"}) {
		t.Errorf("jobs = %v", got)
	}
	if len(skipped) != 3 || !strings.Contains(skipped[0].Skipped, "no auth role's ops match") {
		t.Errorf("skipped = %d, first %q", len(skipped), skipped[0].Skipped)
	}
	if r.specRole() != "admin" {
		t.Errorf("spec should be fetched as the only role with credentials, got %q", r.specRole())
	}
}

func TestSelectAsOverridesRoleOps(t *testing.T) {
	cfg := &Config{}
	cfg.Auth.Bearer = "cust"
	cfg.Auth.Roles = map[string]auth.Role{"admin": {Creds: auth.Creds{Bearer: "adm"}, Ops: []string{"tag:admin"}}}
	cfg.Run.As = []string{"default", "admin"}
	cfg.Run.Include = []string{"/orders", "/me"}
	r := newRoleRunner(t, cfg, false)
	jobs, _ := r.Select()
	want := []string{"listOrders@", "listOrders@admin", "me@", "me@admin"}
	if got := jobNames(jobs); !slices.Equal(got, want) {
		t.Errorf("jobs = %v, want %v", got, want)
	}
}

func TestOrderWritesKeepsRolesTogether(t *testing.T) {
	post := &spec.Operation{ID: "c", Method: "POST", Path: "/notes"}
	del := &spec.Operation{ID: "d", Method: "DELETE", Path: "/notes/{id}", Depth: 1}
	got := orderWrites([]Job{{del, "a"}, {post, "a"}, {del, "b"}, {post, "b"}})
	want := []Job{{post, "a"}, {post, "b"}, {del, "a"}, {del, "b"}}
	if !slices.Equal(got, want) {
		t.Errorf("order = %v", jobNames(got))
	}
}
