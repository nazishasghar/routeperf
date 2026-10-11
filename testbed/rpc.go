package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

// Procedure-style APIs over the same planted problems: tRPC (/trpc, and
// /trpc-sj with the superjson transformer), JSON-RPC 2.0 (/rpc, OpenRPC via
// rpc.discover) and the protobuf Shop service over Connect and Twirp.

type rpcErr struct {
	status int // HTTP meaning: 400, 401, 404, 500
	msg    string
}

func (e *rpcErr) Error() string { return e.msg }

func badInput(format string, a ...any) error { return &rpcErr{400, fmt.Sprintf(format, a...)} }

// procedure is one tRPC procedure / JSON-RPC method.
type procedure struct {
	kind   string         // query | mutation | subscription
	input  map[string]any // JSON Schema; nil = no input
	fn     func(r *http.Request, in any) (any, error)
	params []string // JSON-RPC: positional order
}

func obj(props map[string]any, required ...string) map[string]any {
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

var (
	intS    = map[string]any{"type": "integer", "exclusiveMinimum": 0}
	limitS  = map[string]any{"type": "integer", "minimum": 1, "maximum": 1000, "default": 20}
	statusS = map[string]any{"type": "string", "enum": []any{"paid", "pending", "refunded", "shipped"}}
	emailS  = map[string]any{"type": "string", "format": "email"}
)

// validate checks a value against the small JSON Schema subset used here,
// so wrongly shaped input from routeperf fails loudly (400).
func validate(s map[string]any, v any, at string) error {
	if s == nil {
		return nil
	}
	switch s["type"] {
	case "object":
		m, ok := v.(map[string]any)
		if !ok {
			return badInput("%s: want an object, got %T", at, v)
		}
		for _, r := range s["required"].([]string) {
			if _, ok := m[r]; !ok {
				return badInput("%s.%s: required", at, r)
			}
		}
		props := s["properties"].(map[string]any)
		for k, x := range m {
			ps, ok := props[k].(map[string]any)
			if !ok {
				return badInput("%s.%s: unknown field", at, k)
			}
			if x == nil {
				continue
			}
			if err := validate(ps, x, at+"."+k); err != nil {
				return err
			}
		}
	case "array":
		a, ok := v.([]any)
		if !ok {
			return badInput("%s: want an array, got %T", at, v)
		}
		if n, _ := s["minItems"].(int); len(a) < n {
			return badInput("%s: at least %d items", at, n)
		}
		for i, x := range a {
			if err := validate(s["items"].(map[string]any), x, fmt.Sprintf("%s[%d]", at, i)); err != nil {
				return err
			}
		}
	case "integer", "number":
		f, ok := v.(float64)
		if !ok {
			return badInput("%s: want a number, got %T", at, v)
		}
		if s["type"] == "integer" && f != float64(int64(f)) {
			return badInput("%s: want an integer", at)
		}
		if m, ok := s["minimum"].(int); ok && f < float64(m) {
			return badInput("%s: below %d", at, m)
		}
		if m, ok := s["exclusiveMinimum"].(int); ok && f <= float64(m) {
			return badInput("%s: must be above %d", at, m)
		}
		if m, ok := s["maximum"].(int); ok && f > float64(m) {
			return badInput("%s: above %d", at, m)
		}
	case "string":
		str, ok := v.(string)
		if !ok {
			return badInput("%s: want a string, got %T", at, v)
		}
		if e, ok := s["enum"].([]any); ok {
			found := false
			for _, x := range e {
				found = found || x == str
			}
			if !found {
				return badInput("%s: %q is not one of %v", at, str, e)
			}
		}
		if s["format"] == "email" && !strings.Contains(str, "@") {
			return badInput("%s: not an email", at)
		}
	}
	return nil
}

func field(in any, k string) any {
	m, _ := in.(map[string]any)
	return m[k]
}

func intField(in any, k string, def int) int {
	if f, ok := field(in, k).(float64); ok {
		return int(f)
	}
	return def
}

func strField(in any, k string) string {
	s, _ := field(in, k).(string)
	return s
}

func usersQuery(r *http.Request, query string, args ...any) ([]User, error) {
	rows, err := db.Query(sq(r, query), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		var u User
		var name, country sql.NullString
		if err := rows.Scan(&u.ID, &u.Email, &name, &country, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Name, u.Country = name.String, country.String
		out = append(out, u)
	}
	return out, rows.Err()
}

func oneUser(r *http.Request, query string, arg any) (any, error) {
	us, err := usersQuery(r, query, arg)
	if err != nil {
		return nil, err
	}
	if len(us) == 0 {
		return nil, &rpcErr{404, "user not found"}
	}
	return us[0], nil
}

// ordersWithItems: planted seq scan on orders.user_id + N+1 on order_items.
func ordersWithItems(r *http.Request, userID int64, limit int) (any, error) {
	rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), userID, limit)
	if err != nil {
		return nil, err
	}
	orders, err := scanOrders(rows)
	if err != nil {
		return nil, err
	}
	for i := range orders {
		irows, err := db.Query(sq(r, "SELECT id, sku, qty, price_cents FROM order_items WHERE order_id = ?"), orders[i].ID)
		if err != nil {
			return nil, err
		}
		for irows.Next() {
			var it Item
			_ = irows.Scan(&it.ID, &it.SKU, &it.Qty, &it.PriceCents)
			orders[i].Items = append(orders[i].Items, it)
		}
		irows.Close()
	}
	if orders == nil {
		orders = []Order{}
	}
	return orders, nil
}

// searchOrderPage: planted unindexed filter + sort; keyset cursor on id.
func searchOrderPage(r *http.Request, st string, limit int, cursor int64) (any, error) {
	if st == "" {
		st = "paid"
	}
	q, args := "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? ORDER BY id DESC LIMIT ?", []any{st, limit}
	if cursor > 0 {
		q, args = "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? AND id < ? ORDER BY id DESC LIMIT ?", []any{st, cursor, limit}
	}
	rows, err := db.Query(sq(r, q), args...)
	if err != nil {
		return nil, err
	}
	orders, err := scanOrders(rows)
	if err != nil {
		return nil, err
	}
	var next any
	if len(orders) == limit && len(orders) > 0 {
		next = orders[len(orders)-1].ID
	}
	if orders == nil {
		orders = []Order{}
	}
	return map[string]any{"items": orders, "nextCursor": next}, nil
}

func createUserProc(r *http.Request, in any) (any, error) {
	email, name, country := strField(in, "email"), strField(in, "name"), strField(in, "country")
	id, err := insertUser(sc(r, ""), email, name, country)
	if err != nil {
		if strings.Contains(err.Error(), "uplicate") {
			return nil, &rpcErr{409, "email exists"}
		}
		return nil, err
	}
	return map[string]any{"id": id, "email": email}, nil
}

// planted: one INSERT per item
func createOrderProc(r *http.Request, in any) (any, error) {
	uid := int64(intField(in, "userId", 0))
	items, _ := field(in, "items").([]any)
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id int64
	if dialect == "postgres" {
		err = tx.QueryRow(sc(r, "INSERT INTO orders (user_id, status, total_cents, created_at) VALUES ($1, 'pending', 0, now()) RETURNING id"), uid).Scan(&id)
	} else {
		var res sql.Result
		if res, err = tx.Exec(sc(r, "INSERT INTO orders (user_id, status, total_cents, created_at) VALUES (?, 'pending', 0, NOW())"), uid); err == nil {
			id, _ = res.LastInsertId()
		}
	}
	if err != nil {
		return nil, err
	}
	for _, it := range items {
		if _, err := tx.Exec(sq(r, "INSERT INTO order_items (order_id, sku, qty, price_cents) VALUES (?, ?, ?, ?)"), id, strField(it, "sku"), intField(it, "qty", 1), intField(it, "priceCents", 0)); err != nil {
			return nil, err
		}
	}
	return map[string]any{"id": id}, tx.Commit()
}

var trpcProcs = map[string]procedure{
	"user.byId": {kind: "query", input: obj(map[string]any{"id": intS}, "id"), fn: func(r *http.Request, in any) (any, error) {
		return oneUser(r, "SELECT id, email, name, country, created_at FROM users WHERE id = ?", intField(in, "id", 0))
	}},
	// scalar input (the whole input is a string)
	"user.byEmail": {kind: "query", input: emailS, fn: func(r *http.Request, in any) (any, error) {
		us, err := usersQuery(r, "SELECT id, email, name, country, created_at FROM users WHERE email = ?", in)
		if err != nil || len(us) == 0 {
			return nil, err
		}
		return us[0], nil
	}},
	"user.me": {kind: "query", fn: func(r *http.Request, _ any) (any, error) {
		return oneUser(r, "SELECT id, email, name, country, created_at FROM users WHERE id = ?", 1)
	}},
	"user.list": {kind: "query", input: obj(map[string]any{"limit": limitS}), fn: func(r *http.Request, in any) (any, error) {
		return usersQuery(r, "SELECT id, email, name, country, created_at FROM users ORDER BY id LIMIT ?", intField(in, "limit", 20))
	}},
	"order.byUser": {kind: "query", input: obj(map[string]any{"userId": intS, "limit": limitS}, "userId"), fn: func(r *http.Request, in any) (any, error) {
		return ordersWithItems(r, int64(intField(in, "userId", 0)), intField(in, "limit", 20))
	}},
	"order.search": {kind: "query", input: obj(map[string]any{"status": statusS, "limit": limitS, "cursor": map[string]any{"type": []any{"integer", "null"}}}),
		fn: func(r *http.Request, in any) (any, error) {
			return searchOrderPage(r, strField(in, "status"), intField(in, "limit", 20), int64(intField(in, "cursor", 0)))
		}},
	"user.create": {kind: "mutation", input: obj(map[string]any{"email": emailS, "name": map[string]any{"type": "string"}, "country": map[string]any{"type": "string", "maxLength": 2}}, "email"),
		fn: createUserProc},
	"order.create": {kind: "mutation", input: obj(map[string]any{"userId": intS, "items": map[string]any{"type": "array", "minItems": 1,
		"items": obj(map[string]any{"sku": map[string]any{"type": "string"}, "qty": map[string]any{"type": "integer", "minimum": 1, "maximum": 5}, "priceCents": map[string]any{"type": "integer", "minimum": 0}}, "sku", "qty", "priceCents")}},
		"userId", "items"), fn: createOrderProc},
	// skipped by routeperf: destructive name, and a subscription
	"admin.purgeSessions": {kind: "mutation", fn: func(r *http.Request, _ any) (any, error) {
		return nil, &rpcErr{500, "routeperf must not call this"}
	}},
	"order.onCreate": {kind: "subscription"},
}

// trpcManifest lists the procedures as `routeperf trpc-manifest` would.
func trpcManifest(transformer string) []byte {
	var procs []map[string]any
	for _, p := range sortedKeys(trpcProcs) {
		e := map[string]any{"path": p, "type": trpcProcs[p].kind}
		if in := trpcProcs[p].input; in != nil {
			e["input"] = in
		} else {
			e["no_input"] = true
		}
		procs = append(procs, e)
	}
	b, _ := json.Marshal(map[string]any{"trpc": map[string]any{"version": 11, "transformer": transformer, "procedures": procs}})
	return b
}

func sortedKeys[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	return ks
}

var trpcCodes = map[int]struct {
	name string
	code int
}{400: {"BAD_REQUEST", -32600}, 401: {"UNAUTHORIZED", -32001}, 404: {"NOT_FOUND", -32004}, 405: {"METHOD_NOT_SUPPORTED", -32005},
	409: {"CONFLICT", -32009}, 500: {"INTERNAL_SERVER_ERROR", -32603}}

func trpcHandler(superjson bool) http.HandlerFunc {
	wrap := func(v any) any {
		if superjson {
			return map[string]any{"json": v}
		}
		return v
	}
	fail := func(w http.ResponseWriter, path string, status int, msg string) {
		c := trpcCodes[status]
		writeJSON(w, status, map[string]any{"error": wrap(map[string]any{"message": msg, "code": c.code,
			"data": map[string]any{"code": c.name, "httpStatus": status, "path": path}})})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		path := r.PathValue("path")
		p, ok := trpcProcs[path]
		if !ok {
			fail(w, path, 404, `No procedure found on path "`+path+`"`)
			return
		}
		want := map[string]string{"GET": "query", "POST": "mutation"}[r.Method]
		if p.kind != want {
			fail(w, path, 405, fmt.Sprintf("Unsupported %s-request to %s procedure at path %q", r.Method, p.kind, path))
			return
		}
		if r.Header.Get("Authorization") != "Bearer testtoken" {
			fail(w, path, 401, "UNAUTHORIZED")
			return
		}
		raw := []byte(r.URL.Query().Get("input"))
		if r.Method == "POST" {
			raw, _ = io.ReadAll(r.Body)
		}
		var in any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &in); err != nil {
				fail(w, path, 400, "input is not JSON: "+err.Error())
				return
			}
			if superjson {
				m, ok := in.(map[string]any)
				if !ok || len(m) > 2 {
					fail(w, path, 400, `superjson input must be {"json": …}`)
					return
				}
				in = m["json"]
			}
		}
		if p.input == nil && in != nil {
			fail(w, path, 400, "procedure takes no input")
			return
		}
		if p.input != nil {
			if in == nil {
				fail(w, path, 400, "input required")
				return
			}
			if err := validate(p.input, in, "input"); err != nil {
				fail(w, path, 400, err.Error())
				return
			}
		}
		out, err := p.fn(r, in)
		if err != nil {
			st := 500
			if e, ok := err.(*rpcErr); ok {
				st = e.status
			}
			fail(w, path, st, err.Error())
			return
		}
		writeJSON(w, 200, map[string]any{"result": map[string]any{"data": wrap(out)}})
	}
}

// ------------------------------------------------------------ JSON-RPC

type rpcParam struct {
	name     string
	schema   map[string]any
	required bool
}

type rpcMethod struct {
	params     []rpcParam
	byPosition bool
	readOnly   *bool
	fn         func(r *http.Request, in map[string]any) (any, error)
}

var yes = true

var rpcMethods = map[string]rpcMethod{
	"user_get": {params: []rpcParam{{"id", intS, true}}, fn: func(r *http.Request, in map[string]any) (any, error) {
		return oneUser(r, "SELECT id, email, name, country, created_at FROM users WHERE id = ?", intField(in, "id", 0))
	}},
	"orders_listByUser": {byPosition: true, params: []rpcParam{{"userId", intS, true}, {"limit", limitS, false}}, fn: func(r *http.Request, in map[string]any) (any, error) {
		return ordersWithItems(r, int64(intField(in, "userId", 0)), intField(in, "limit", 20))
	}},
	// no read verb in the name: x-readonly marks it
	"users_recent": {readOnly: &yes, params: []rpcParam{{"limit", limitS, false}}, fn: func(r *http.Request, in map[string]any) (any, error) {
		return usersQuery(r, "SELECT id, email, name, country, created_at FROM users ORDER BY id DESC LIMIT ?", intField(in, "limit", 20))
	}},
	"orders_search": {params: []rpcParam{{"status", statusS, false}, {"limit", limitS, false}}, fn: func(r *http.Request, in map[string]any) (any, error) {
		return searchOrderPage(r, strField(in, "status"), intField(in, "limit", 20), 0)
	}},
	"user_create": {params: []rpcParam{{"email", emailS, true}, {"name", map[string]any{"type": "string"}, false}}, fn: func(r *http.Request, in map[string]any) (any, error) {
		return createUserProc(r, in)
	}},
}

func openRPCDoc() map[string]any {
	var methods []any
	for _, name := range sortedKeys(rpcMethods) {
		m := rpcMethods[name]
		var params []any
		for _, p := range m.params {
			params = append(params, map[string]any{"name": p.name, "required": p.required, "schema": p.schema})
		}
		e := map[string]any{"name": name, "params": params, "result": map[string]any{"name": "result", "schema": map[string]any{}}}
		if !m.byPosition {
			e["paramStructure"] = "by-name"
		} else {
			e["paramStructure"] = "by-position"
		}
		if m.readOnly != nil {
			e["x-readonly"] = *m.readOnly
		}
		methods = append(methods, e)
	}
	return map[string]any{"openrpc": "1.3.2", "info": map[string]any{"title": "testbed JSON-RPC", "version": "1"},
		"servers": []any{map[string]any{"url": "http://localhost/rpc"}}, "methods": methods}
}

func jsonrpcHandler(w http.ResponseWriter, r *http.Request) {
	var raw json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeJSON(w, 200, map[string]any{"jsonrpc": "2.0", "id": nil, "error": map[string]any{"code": -32700, "message": "parse error"}})
		return
	}
	authed := r.Header.Get("Authorization") == "Bearer testtoken"
	if len(raw) > 0 && raw[0] == '[' { // batch
		var calls []json.RawMessage
		_ = json.Unmarshal(raw, &calls)
		var out []any
		for _, c := range calls {
			out = append(out, jsonrpcCall(r, c, authed))
		}
		writeJSON(w, 200, out)
		return
	}
	writeJSON(w, 200, jsonrpcCall(r, raw, authed))
}

func jsonrpcCall(r *http.Request, raw json.RawMessage, authed bool) map[string]any {
	var call struct {
		JSONRPC string          `json:"jsonrpc"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
		ID      any             `json:"id"`
	}
	errResp := func(code int, msg string) map[string]any {
		return map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": code, "message": msg}}
	}
	if err := json.Unmarshal(raw, &call); err != nil || call.JSONRPC != "2.0" || call.Method == "" {
		return errResp(-32600, "invalid request")
	}
	if !authed {
		return errResp(-32001, "Unauthorized")
	}
	if call.Method == "rpc.discover" {
		return map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": openRPCDoc()}
	}
	m, ok := rpcMethods[call.Method]
	if !ok {
		return errResp(-32601, "method not found")
	}
	in := map[string]any{}
	if len(call.Params) > 0 && call.Params[0] == '[' {
		if !m.byPosition {
			return errResp(-32602, "params must be by name")
		}
		var list []any
		_ = json.Unmarshal(call.Params, &list)
		if len(list) > len(m.params) {
			return errResp(-32602, "too many params")
		}
		for i, v := range list {
			if v != nil {
				in[m.params[i].name] = v
			}
		}
	} else if len(call.Params) > 0 {
		if m.byPosition {
			return errResp(-32602, "params must be by position")
		}
		if err := json.Unmarshal(call.Params, &in); err != nil {
			return errResp(-32602, "params must be an object or array")
		}
	}
	props := map[string]any{}
	var req []string
	for _, p := range m.params {
		props[p.name] = p.schema
		if p.required {
			req = append(req, p.name)
		}
	}
	if err := validate(obj(props, req...), in, "params"); err != nil {
		return errResp(-32602, err.Error())
	}
	out, err := m.fn(r, in)
	if err != nil {
		code := -32000
		if e, ok := err.(*rpcErr); ok && e.status == 404 {
			code = 404
		}
		return errResp(code, err.Error())
	}
	return map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": out}
}

// ------------------------------------------------------------ Connect / Twirp

var connectCodes = map[codes.Code]struct {
	name string
	http int
}{codes.InvalidArgument: {"invalid_argument", 400}, codes.Unauthenticated: {"unauthenticated", 401}, codes.PermissionDenied: {"permission_denied", 403},
	codes.NotFound: {"not_found", 404}, codes.AlreadyExists: {"already_exists", 409}, codes.Internal: {"internal", 500}, codes.Unimplemented: {"unimplemented", 404}}

// protoHTTPHandler serves Shop over Connect's unary JSON protocol, or over
// Twirp's (twirp = true: error key "msg", no protocol header).
func protoHTTPHandler(twirp bool) http.HandlerFunc {
	fail := func(w http.ResponseWriter, err error) {
		st := status.Convert(err)
		c, ok := connectCodes[st.Code()]
		if !ok {
			c = connectCodes[codes.Internal]
		}
		key := "message"
		if twirp {
			key = "msg"
		}
		writeJSON(w, c.http, map[string]any{"code": c.name, key: st.Message()})
	}
	return func(w http.ResponseWriter, r *http.Request) {
		m := shopSvc.Methods().ByName(protoreflect.Name(r.PathValue("method")))
		if m == nil {
			fail(w, status.Error(codes.Unimplemented, "no such method"))
			return
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			writeJSON(w, 415, map[string]any{"code": "invalid_argument", "message": "content type " + ct})
			return
		}
		if !twirp && r.Header.Get("Connect-Protocol-Version") != "1" {
			fail(w, status.Error(codes.InvalidArgument, "missing Connect-Protocol-Version: 1"))
			return
		}
		md := metadata.MD{}
		for _, h := range []string{"authorization", "traceparent"} {
			if v := r.Header.Get(h); v != "" {
				md.Set(h, v)
			}
		}
		ctx := metadata.NewIncomingContext(r.Context(), md)
		if err := grpcAuth(ctx); err != nil {
			fail(w, err)
			return
		}
		body, _ := io.ReadAll(r.Body)
		in := dynamicpb.NewMessage(m.Input())
		if err := protojson.Unmarshal(body, in); err != nil { // strict: unknown fields fail
			fail(w, status.Error(codes.InvalidArgument, err.Error()))
			return
		}
		out := dynamicpb.NewMessage(m.Output())
		if err := shopMethods[string(m.Name())](ctx, in, out); err != nil {
			fail(w, err)
			return
		}
		b, _ := protojson.Marshal(out)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	}
}

func registerRPC(mux *http.ServeMux, addr string) {
	mux.HandleFunc("GET /trpc/{path}", trpcHandler(false))
	mux.HandleFunc("POST /trpc/{path}", trpcHandler(false))
	mux.HandleFunc("GET /trpc-sj/{path}", trpcHandler(true))
	mux.HandleFunc("POST /trpc-sj/{path}", trpcHandler(true))
	mux.HandleFunc("GET /routeperf.trpc.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(trpcManifest(""))
	})
	mux.HandleFunc("GET /superjson.trpc.json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(trpcManifest("superjson"))
	})
	mux.HandleFunc("POST /rpc", jsonrpcHandler)
	mux.HandleFunc("GET /openrpc.json", func(w http.ResponseWriter, _ *http.Request) {
		doc := openRPCDoc()
		doc["servers"] = []any{map[string]any{"url": (&url.URL{Scheme: "http", Host: "localhost" + addr, Path: "/rpc"}).String()}}
		writeJSON(w, 200, doc)
	})
	svc := string(shopSvc.FullName())
	mux.HandleFunc("POST /"+svc+"/{method}", protoHTTPHandler(false))
	mux.HandleFunc("POST /twirp/"+svc+"/{method}", protoHTTPHandler(true))
	mux.HandleFunc("GET /shop.proto", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(shopProto))
	})
}
