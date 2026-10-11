package rpcspec

import (
	"encoding/json"
	"testing"
)

func TestResource(t *testing.T) {
	for name, want := range map[string]string{
		"user.byId":            "user",
		"admin.users.list":     "users",
		"orderItems.byOrderId": "order_items",
		"getUser":              "user",
		"listUserOrders":       "user_orders",
		"users_get":            "users",
		"user_create":          "user",
		"get_user":             "user",
		"settings.get":         "settings",
		"issues.list":          "issues",
		"eth_getBlockByNumber": "eth",
		"health":               "health",
	} {
		if got := Resource(name); got != want {
			t.Errorf("Resource(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestIsRead(t *testing.T) {
	for name, want := range map[string]bool{
		"user_get": true, "orders_listByUser": true, "getUser": true, "eth_getBalance": true, "user.search": true,
		"user_create": false, "users_recent": false, "settings.set": false, "issues_open": false, "isAdmin": true,
	} {
		if got := IsRead(name); got != want {
			t.Errorf("IsRead(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestSchema(t *testing.T) {
	var js map[string]any
	_ = json.Unmarshal([]byte(`{"type":"object","required":["id"],"properties":{
		"id":{"type":"integer","exclusiveMinimum":0},
		"limit":{"type":"integer","minimum":1,"maximum":100,"default":20},
		"cursor":{"type":["string","null"]},
		"tag":{"anyOf":[{"$ref":"#/$defs/Tag"},{"type":"null"}]},
		"items":{"type":"array","items":{"$ref":"#/$defs/Item"},"minItems":1}},
		"$defs":{"Tag":{"type":"string","enum":["a","b"]},"Item":{"type":"object","properties":{"sku":{"type":"string"}}}}}`), &js)
	s := Schema(js, js)
	if !s.Type.Is("object") || len(s.Required) != 1 {
		t.Fatalf("object: %+v", s)
	}
	if id := s.Properties["id"].Value; !id.Type.Is("integer") || id.Min == nil || *id.Min != 1 {
		t.Errorf("exclusiveMinimum 0 should give minimum 1: %+v", id)
	}
	if c := s.Properties["cursor"].Value; !c.Type.Is("string") || !c.Nullable {
		t.Errorf("nullable string: %+v", c)
	}
	if tag := s.Properties["tag"].Value; len(tag.Enum) != 2 || !tag.Nullable {
		t.Errorf("$ref through anyOf null: %+v", tag)
	}
	if it := s.Properties["items"].Value; !it.Type.Is("array") || it.MinItems != 1 || !it.Items.Value.Type.Is("object") {
		t.Errorf("array of $ref objects: %+v", it)
	}
}

func TestTRPCOperations(t *testing.T) {
	m, err := ParseManifest([]byte(`{"trpc":{"transformer":"superjson","procedures":[
		{"path":"user.byId","type":"query","input":{"type":"object","properties":{"id":{"type":"integer"}},"required":["id"]}},
		{"path":"user.byEmail","type":"query","input":{"type":"string","format":"email"}},
		{"path":"user.search","type":"query","input":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"x-optional":true}},
		{"path":"health","type":"query","no_input":true},
		{"path":"user.create","type":"mutation","input":{"type":"object","properties":{"email":{"type":"string"},"name":{"type":"string"}},"required":["email"]}},
		{"path":"post.onAdd","type":"subscription","no_input":true},
		{"path":"legacy","type":"query"}]}}`))
	if err != nil {
		t.Fatal(err)
	}
	sp := TRPCOperations(m, "t")
	byID := map[string]int{}
	for i, o := range sp.Ops {
		byID[o.ID] = i
	}
	get := func(id string) int {
		i, ok := byID[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		return i
	}
	o := sp.Ops[get("user.byId")]
	if o.Phase != "R" || o.Method != "QUERY" || o.RPC.Wrap != "object" || len(o.Params) != 1 || !o.Params[0].Required || o.Resource != "user" {
		t.Errorf("user.byId: %+v %+v", o, o.RPC)
	}
	if o.Tags[0] != "user" {
		t.Errorf("namespace tag: %v", o.Tags)
	}
	if o := sp.Ops[get("user.byEmail")]; o.RPC.Wrap != "value" || o.Params[0].Name != "input" || !o.Params[0].Required {
		t.Errorf("scalar input: %+v", o.Params)
	}
	if o := sp.Ops[get("user.search")]; o.RPC.Wrap != "object" || !o.Params[0].Required {
		// an optional root object still needs its required fields once sent
		t.Errorf("optional object: %+v", o.Params)
	}
	if o := sp.Ops[get("health")]; o.RPC.Wrap != "none" || len(o.Params) != 0 {
		t.Errorf("no input: %+v", o.RPC)
	}
	if o := sp.Ops[get("user.create")]; o.Phase != "W" || o.Params[1].Required {
		t.Errorf("mutation: phase %s params %+v", o.Phase, o.Params)
	}
	if o := sp.Ops[get("post.onAdd")]; o.Unsupported == "" {
		t.Errorf("subscription should be unsupported")
	}
	if o := sp.Ops[get("legacy")]; o.RPC.Wrap != "object" {
		t.Errorf("unconvertible input → {}: %+v", o.RPC)
	}
}

func TestParseOpenRPC(t *testing.T) {
	doc := `{"jsonrpc":"2.0","id":1,"result":{"openrpc":"1.3.2","info":{"title":"Shop"},"servers":[{"url":"http://x/rpc"}],
	  "components":{"contentDescriptors":{"UserId":{"name":"userId","required":true,"schema":{"$ref":"#/components/schemas/Id"}}},
	                "schemas":{"Id":{"type":"integer","minimum":1}}},
	  "methods":[
	    {"name":"user_get","paramStructure":"by-name","params":[{"$ref":"#/components/contentDescriptors/UserId"}],"result":{"name":"u","schema":{}},
	     "examples":[{"name":"one","params":[{"name":"userId","value":42}],"result":{"name":"r","value":{}}}]},
	    {"name":"orders_list","params":[{"$ref":"#/components/contentDescriptors/UserId"},{"name":"limit","schema":{"type":"integer"}}],"result":{"name":"o","schema":{}}},
	    {"name":"users_recent","x-readonly":true,"params":[],"result":{"name":"o","schema":{}}},
	    {"name":"user_getOrCreate","x-side-effects":true,"params":[],"result":{"name":"o","schema":{}}},
	    {"name":"rpc.discover","params":[],"result":{"name":"d","schema":{}}}]}}`
	d, err := ParseOpenRPC([]byte(doc), "")
	if err != nil {
		t.Fatal(err)
	}
	if d.Title != "Shop" || len(d.Servers) != 1 || len(d.Ops.Ops) != 4 {
		t.Fatalf("doc: %+v ops %d", d, len(d.Ops.Ops))
	}
	ops := map[string]int{}
	for i, o := range d.Ops.Ops {
		ops[o.ID] = i
	}
	u := d.Ops.Ops[ops["user_get"]]
	if u.RPC.Wrap != "object" || u.Phase != "R" || len(u.Params) != 1 || u.Params[0].Example != float64(42) || !u.Params[0].Schema.Type.Is("integer") {
		t.Errorf("user_get: %+v %+v", u.RPC, u.Params)
	}
	if o := d.Ops.Ops[ops["orders_list"]]; o.RPC.Wrap != "array" || len(o.RPC.Args) != 2 || o.RPC.Args[0] != "userId" {
		t.Errorf("by-position: %+v", o.RPC)
	}
	if o := d.Ops.Ops[ops["users_recent"]]; o.Phase != "R" {
		t.Errorf("x-readonly: %s", o.Phase)
	}
	if o := d.Ops.Ops[ops["user_getOrCreate"]]; o.Phase != "W" {
		t.Errorf("x-side-effects overrides the get verb: %s", o.Phase)
	}
	d, _ = ParseOpenRPC([]byte(doc), "by-name")
	for _, o := range d.Ops.Ops {
		if o.RPC.Wrap != "object" {
			t.Errorf("params_by by-name: %s %s", o.ID, o.RPC.Wrap)
		}
	}
	if _, err := ParseOpenRPC([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Unauthorized"}}`), ""); err == nil {
		t.Error("discover error should fail")
	}
}
