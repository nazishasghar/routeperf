package inputs

import (
	"context"
	"strings"
	"testing"

	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/spec"
)

// fakeDB answers queries by substring; everything not overridden panics.
type fakeDB struct {
	db.DB
	answers map[string][]string
}

func (f fakeDB) Quote(s string) string                    { return `"` + s + `"` }
func (f fakeDB) HashPred(expr string, _ float64) string   { return "hash(" + expr + ")" }
func (f fakeDB) KeyExpr(t *db.Table, alias string) string { return alias + "." + t.PK }
func (f fakeDB) FirstValues(_ context.Context, q string) ([]string, error) {
	for k, v := range f.answers {
		if strings.Contains(q, k) {
			return v, nil
		}
	}
	return nil, nil
}

func TestFixtureWithoutRowsFallsBack(t *testing.T) {
	users := &db.Table{Key: "users", Name: "users", SQL: `"users"`, Kind: "table", PK: "id", PKCols: []string{"id"}}
	d := fakeDB{answers: map[string][]string{
		"WHERE false":  nil,
		"NULL AS id":   {""},
		`FROM "users"`: {"7", "8"},
	}}
	r := New(d, map[string]*db.Table{"users": users}, map[string]Fixture{
		"getUser": {Path: map[string]any{"id": "sql: SELECT id FROM users WHERE false"}, Query: map[string]any{"tag": "sql: SELECT NULL AS id"}},
		"addNote": {Body: map[string]any{"user_id": "sql: SELECT id FROM users WHERE false", "text": "hi"}},
	}, nil)
	op := &spec.Operation{ID: "getUser", Method: "GET", Path: "/users/{id}",
		Params: []spec.Param{{Name: "id", In: "path", Required: true}, {Name: "tag", In: "query"}}}
	req, err := r.Build(context.Background(), op, 0, -1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.Path != "/users/7" {
		t.Errorf("path = %q, want the anchor ID /users/7", req.Path)
	}
	if req.Query.Has("tag") {
		t.Errorf("a NULL fixture value must not be sent: tag=%q", req.Query.Get("tag"))
	}
	if !strings.Contains(r.FixtureMisses["getUser path:id"], "no rows") || r.FixtureMisses["getUser query:tag"] == "" {
		t.Errorf("misses = %v", r.FixtureMisses)
	}

	post := &spec.Operation{ID: "addNote", Method: "POST", Path: "/notes", Body: &spec.Body{ContentType: "application/json"}}
	req, err = r.Build(context.Background(), post, 1, -1, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := req.Body.(map[string]any)
	if body["user_id"] != int64(8) || body["text"] != "hi" {
		t.Errorf("body = %v, want user_id from anchors", body)
	}
}

func TestAnchorsIgnoreCycleFKs(t *testing.T) {
	pc := &db.Table{Key: "product_customer", SQL: `"product_customer"`, Kind: "table", PK: "id", PKCols: []string{"id"}}
	pm := &db.Table{Key: "pbp_member", SQL: `"pbp_member"`, Kind: "table", PK: "id", PKCols: []string{"id"}}
	r := New(fakeDB{}, map[string]*db.Table{"product_customer": pc, "pbp_member": pm}, nil, nil)
	r.FKs = []db.FK{
		{Child: "product_customer", ChildCols: []string{"pbp_member_id"}, Parent: "pbp_member", ParentCols: []string{"id"}, Deferred: true},
		{Child: "pbp_member", ChildCols: []string{"product_customer_id"}, Parent: "product_customer", ParentCols: []string{"id"}},
	}
	// subsets sample customers by key hash and members by customer, so must anchors
	if got := r.anchorPred(pc, "s", 0); got != "hash(s.id)" {
		t.Errorf("customer anchors: %s", got)
	}
	if got := r.anchorPred(pm, "s", 0); !strings.Contains(got, `s."product_customer_id" IN (SELECT p0."id" FROM "product_customer" p0 WHERE hash(p0.id))`) {
		t.Errorf("member anchors: %s", got)
	}
}
