package inputs

import (
	"context"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/spec"
)

func objSchema(props map[string]*openapi3.Schema) *openapi3.Schema {
	s := openapi3.NewObjectSchema()
	for k, v := range props {
		s.Properties[k] = openapi3.NewSchemaRef("", v)
	}
	return s
}

// A protobuf page_size is an integer in the body: the k sweep sets it to k
// (it used to become an array of k items).
func TestScalarBodyK(t *testing.T) {
	op := &spec.Operation{ID: "ListUserOrders", Method: "RPC", Path: "Shop/ListUserOrders", Protocol: "grpc", Resource: "userorders",
		GRPC: &spec.GRPCOp{}, Body: &spec.Body{ContentType: "application/json", Schema: objSchema(map[string]*openapi3.Schema{
			"page_size": openapi3.NewIntegerSchema(), "user_id": openapi3.NewIntegerSchema()})}}
	r := New(fakeDB{}, map[string]*db.Table{}, nil, []string{"page_size"})
	ki := r.KParam(op)
	if ki == nil || !ki.Scalar || !ki.InBody {
		t.Fatalf("k param: %+v", ki)
	}
	req, err := r.Build(context.Background(), op, 0, 250, ki, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := req.Body.(map[string]any)["page_size"]; got != 250 {
		t.Errorf("page_size = %v, want 250", got)
	}
}

// A procedure argument that is a list of objects is the size knob, like a
// REST bulk body.
func TestArrayArgK(t *testing.T) {
	item := objSchema(map[string]*openapi3.Schema{"sku": openapi3.NewStringSchema(), "qty": openapi3.NewIntegerSchema()})
	items := openapi3.NewArraySchema()
	items.Items = openapi3.NewSchemaRef("", item)
	op := &spec.Operation{ID: "order.create", Method: "MUTATION", Path: "order.create", Protocol: "trpc", Phase: "W", Resource: "order",
		RPC:    &spec.RPCOp{Name: "order.create", Kind: "mutation", Wrap: "object"},
		Params: []spec.Param{{Name: "items", In: "arg", Required: true, Schema: items}, {Name: "note", In: "arg", Schema: openapi3.NewStringSchema()}}}
	r := New(fakeDB{}, map[string]*db.Table{}, nil, []string{"limit"})
	ki := r.KParam(op)
	if ki == nil || !ki.Array || ki.Name != "items" {
		t.Fatalf("k param: %+v", ki)
	}
	req, _ := r.Build(context.Background(), op, 3, 10, ki, nil)
	arr, ok := req.Vars["items"].([]any)
	if !ok || len(arr) != 10 {
		t.Fatalf("items: %v", req.Vars["items"])
	}
	if _, set := req.Vars["note"]; set {
		t.Errorf("optional arg without a source must stay unset: %v", req.Vars)
	}
	req, _ = r.Build(context.Background(), op, 3, -1, ki, nil)
	if arr, _ := req.Vars["items"].([]any); len(arr) != 1 {
		t.Errorf("default size: %v", req.Vars["items"])
	}
}

func TestInputFixtures(t *testing.T) {
	byID := &spec.Operation{ID: "user.byId", Method: "QUERY", Path: "user.byId", Protocol: "trpc", Resource: "user",
		RPC: &spec.RPCOp{Wrap: "object"}, Params: []spec.Param{{Name: "id", In: "arg", Required: true, Schema: openapi3.NewIntegerSchema()}}}
	byEmail := &spec.Operation{ID: "user.byEmail", Method: "QUERY", Path: "user.byEmail", Protocol: "trpc", Resource: "user",
		RPC: &spec.RPCOp{Wrap: "value"}, Params: []spec.Param{{Name: "input", In: "arg", Required: true, Schema: openapi3.NewStringSchema()}}}
	gql := &spec.Operation{ID: "user", Method: "QUERY", Path: "user", Protocol: "graphql", Resource: "user",
		Params: []spec.Param{{Name: "id", In: "arg", Required: true, Schema: openapi3.NewStringSchema()}}}
	fx := map[string]Fixture{
		"user.byId":    {Input: map[string]any{"id": "sql:SELECT id FROM users WHERE vip"}},
		"user.byEmail": {Input: "sql:SELECT email FROM users WHERE vip"},
		"user":         {Body: map[string]any{"id": "u_1"}},
	}
	r := New(fakeDB{answers: map[string][]string{"SELECT id FROM users": {"77"}, "SELECT email FROM users": {"vip@x.test"}}}, map[string]*db.Table{}, fx, nil)
	ctx := context.Background()
	if req, _ := r.Build(ctx, byID, 0, -1, nil, nil); req.Vars["id"] != int64(77) {
		t.Errorf("input map fixture: %#v", req.Vars)
	}
	if req, _ := r.Build(ctx, byEmail, 0, -1, nil, nil); req.Vars["input"] != "vip@x.test" {
		t.Errorf("scalar input fixture: %#v", req.Vars)
	}
	if req, _ := r.Build(ctx, gql, 0, -1, nil, nil); req.Vars["id"] != "u_1" {
		t.Errorf("body map as GraphQL args: %#v", req.Vars)
	}
}

func TestArgCursor(t *testing.T) {
	op := &spec.Operation{ID: "post.list", Method: "QUERY", Path: "post.list", Protocol: "trpc", Resource: "post", RPC: &spec.RPCOp{Wrap: "object"},
		Params: []spec.Param{{Name: "cursor", In: "arg", Schema: openapi3.NewStringSchema()}, {Name: "limit", In: "arg", Schema: openapi3.NewIntegerSchema()}}}
	if CursorParam(op) != "cursor" {
		t.Fatalf("cursor arg not found")
	}
	r := New(fakeDB{}, map[string]*db.Table{}, nil, []string{"limit"})
	ctx := context.Background()
	req, _ := r.Build(ctx, op, 0, -1, nil, nil)
	r.Observe(op, req, 200, []byte(`{"items":[1,2],"nextCursor":"c2"}`), nil)
	req, _ = r.Build(ctx, op, 1, -1, nil, nil)
	if req.Vars["cursor"] != "c2" || req.Page != 1 {
		t.Errorf("second page: %#v page %d", req.Vars, req.Page)
	}
}

func uuidSchema() *openapi3.Schema {
	s := openapi3.NewStringSchema()
	s.Format = "uuid"
	return s
}

const (
	custUUID = "c0ffee00-0000-4000-8000-000000000001"
	itemUUID = "c0ffee00-0000-4000-8000-0000000000aa"
	realUUID = "11111111-1111-4111-8111-111111111111"
)

// bill.create({ customerId, customerUuid, accountId, lines: [{ id, itemId, itemUuid, qty }] })
func billCreate() *spec.Operation {
	line := objSchema(map[string]*openapi3.Schema{"id": uuidSchema(), "itemId": uuidSchema(), "itemUuid": uuidSchema(), "qty": openapi3.NewIntegerSchema()})
	lines := openapi3.NewArraySchema()
	lines.Items = openapi3.NewSchemaRef("", line)
	lines.MinItems = 1
	return &spec.Operation{ID: "bill.create", Method: "MUTATION", Path: "bill.create", Protocol: "trpc", Phase: "W", Resource: "bill",
		RPC: &spec.RPCOp{Wrap: "object"},
		Params: []spec.Param{
			{Name: "accountId", In: "arg", Required: true, Schema: uuidSchema()},
			{Name: "customerId", In: "arg", Required: true, Schema: uuidSchema()},
			{Name: "customerUuid", In: "arg", Required: true, Schema: openapi3.NewStringSchema()},
			{Name: "lines", In: "arg", Required: true, Schema: lines},
		}}
}

func billResolver(fx map[string]Fixture, kparams []string) *Resolver {
	tables := map[string]*db.Table{
		"customers": {Key: "customers", Name: "customers", SQL: `"customers"`, Kind: "table", PK: "id", Cols: []string{"id", "uuid"}},
		"items":     {Key: "items", Name: "items", SQL: `"items"`, Kind: "table", PK: "id", Cols: []string{"id"}},
		"accounts":  {Key: "accounts", Name: "accounts", SQL: `"accounts"`, Kind: "table", PK: "id", Cols: []string{"id"}},
	}
	d := fakeDB{answers: map[string][]string{
		`s."uuid" FROM "customers"`: {custUUID},
		`s."id" FROM "customers"`:   {"7"},
		`FROM "items"`:              {itemUUID},
		`FROM "accounts"`:           {"3"},
		"FROM real_items":           {realUUID},
		"WHERE false":               nil,
	}}
	return New(d, tables, fx, kparams)
}

// Uuid inputs that reference a row get real values: a uuid PK, or the table's
// uuid column when the PK is an integer. A bare uuid "id" stays generated.
func TestUUIDInputsSampleDB(t *testing.T) {
	r := billResolver(nil, nil)
	req, _ := r.Build(context.Background(), billCreate(), 0, -1, nil, nil)
	v := req.Vars
	if v["customerId"] != custUUID || v["customerUuid"] != custUUID {
		t.Errorf("customer refs should come from customers.uuid: %#v", v)
	}
	if a, _ := v["accountId"].(string); a == "3" || !uuidValue.MatchString(a) {
		t.Errorf("integer PK must not land in a uuid input: accountId=%v", v["accountId"])
	}
	line := v["lines"].([]any)[0].(map[string]any)
	if line["itemId"] != itemUUID || line["itemUuid"] != itemUUID {
		t.Errorf("nested refs should be sampled: %#v", line)
	}
	if id, _ := line["id"].(string); id == itemUUID || !uuidValue.MatchString(id) {
		t.Errorf("a new row's own uuid id should be generated: %v", line["id"])
	}
}

// sql: fixtures resolve inside lists of an input, and a miss falls back to a
// sampled ID.
func TestNestedInputFixtures(t *testing.T) {
	raw := []any{
		map[string]any{"itemUuid": "sql: SELECT id FROM real_items", "qty": 2},
		map[string]any{"itemUuid": "sql: SELECT id FROM items WHERE false", "qty": 1},
	}
	r := billResolver(map[string]Fixture{"bill.create": {Input: map[string]any{"lines": raw}}}, nil)
	req, _ := r.Build(context.Background(), billCreate(), 0, -1, nil, nil)
	lines := req.Vars["lines"].([]any)
	if got := lines[0].(map[string]any)["itemUuid"]; got != realUUID {
		t.Errorf("nested sql: fixture = %v, want %s", got, realUUID)
	}
	if got := lines[1].(map[string]any)["itemUuid"]; got != itemUUID {
		t.Errorf("missed nested fixture should fall back to an items anchor: %v", got)
	}
	if r.FixtureMisses["bill.create arg:itemUuid"] == "" {
		t.Errorf("miss not reported: %v", r.FixtureMisses)
	}
	if raw[0].(map[string]any)["itemUuid"] != "sql: SELECT id FROM real_items" {
		t.Errorf("fixture was modified in place: %v", raw[0])
	}
}

// The k sweep of a list input repeats the fixture's first item instead of
// generating one.
func TestArrayArgKUsesFixture(t *testing.T) {
	fx := map[string]Fixture{"bill.create": {Input: map[string]any{
		"lines": []any{map[string]any{"itemUuid": "sql: SELECT id FROM real_items", "qty": 2}},
	}}}
	r := billResolver(fx, []string{"limit"})
	op := billCreate()
	ki := r.KParam(op)
	if ki == nil || !ki.Array || ki.Name != "lines" {
		t.Fatalf("k param: %+v", ki)
	}
	req, _ := r.Build(context.Background(), op, 0, 3, ki, nil)
	lines := req.Vars["lines"].([]any)
	if len(lines) != 3 {
		t.Fatalf("lines: %v", lines)
	}
	for _, l := range lines {
		if m := l.(map[string]any); m["itemUuid"] != realUUID || m["qty"] != 2 || len(m) != 2 {
			t.Errorf("item should copy the fixture: %#v", m)
		}
	}
}
