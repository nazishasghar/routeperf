package runner

import (
	"reflect"
	"testing"

	"github.com/nazishasghar/routeperf/internal/spec"
)

func TestRPCInput(t *testing.T) {
	vars := map[string]any{"id": 7, "limit": 5}
	if v, ok := rpcInput(&spec.RPCOp{Wrap: "object"}, vars); !ok || !reflect.DeepEqual(v, map[string]any{"id": 7, "limit": 5}) {
		t.Errorf("object: %v", v)
	}
	if v, ok := rpcInput(&spec.RPCOp{Wrap: "object"}, map[string]any{}); !ok || !reflect.DeepEqual(v, map[string]any{}) {
		t.Errorf("empty object should still be sent: %v %v", v, ok)
	}
	if v, ok := rpcInput(&spec.RPCOp{Wrap: "value"}, map[string]any{"input": "a@b.c"}); !ok || v != "a@b.c" {
		t.Errorf("value: %v", v)
	}
	if _, ok := rpcInput(&spec.RPCOp{Wrap: "value"}, map[string]any{}); ok {
		t.Error("optional scalar input left out")
	}
	if _, ok := rpcInput(&spec.RPCOp{Wrap: "none"}, vars); ok {
		t.Error("none: no input")
	}
	// by position: a missing optional arg before a given one is null; trailing ones are dropped
	op := &spec.RPCOp{Wrap: "array", Args: []string{"userId", "status", "limit", "cursor"}}
	if v, _ := rpcInput(op, map[string]any{"userId": 1, "limit": 5}); !reflect.DeepEqual(v, []any{1, nil, 5}) {
		t.Errorf("array: %v", v)
	}
	if v, _ := rpcInput(op, map[string]any{}); !reflect.DeepEqual(v, []any{}) {
		t.Errorf("array without args: %v", v)
	}
}

func TestTRPCUnwrap(t *testing.T) {
	cases := []struct {
		status    int
		body      string
		superjson bool
		want      int
		wantBody  string
	}{
		{200, `{"result":{"data":{"id":5}}}`, false, 200, `{"id":5}`},
		{200, `{"result":{"data":{"json":{"id":5},"meta":{"values":{}}}}}`, true, 200, `{"id":5}`},
		{200, `{"result":{"data":{"json":{"id":5}}}}`, false, 200, `{"json":{"id":5}}`}, // no transformer: data is data
		{200, `{"result":{}}`, false, 200, `null`},
		{404, `{"error":{"message":"x","code":-32004,"data":{"code":"NOT_FOUND","httpStatus":404}}}`, false, 404, ""},
		{200, `{"error":{"json":{"message":"x","code":-32001,"data":{"code":"UNAUTHORIZED","httpStatus":401}}}}`, true, 401, ""},
		{200, `{"error":{"message":"x"}}`, false, 500, ""},
		{502, `bad gateway`, false, 502, "bad gateway"},
	}
	for _, c := range cases {
		st, b := trpcUnwrap(c.status, []byte(c.body), c.superjson)
		if st != c.want || c.wantBody != "" && string(b) != c.wantBody {
			t.Errorf("%s: got %d %s, want %d %s", c.body, st, b, c.want, c.wantBody)
		}
	}
}

func TestJSONRPCUnwrap(t *testing.T) {
	cases := []struct {
		body     string
		want     int
		wantBody string
	}{
		{`{"jsonrpc":"2.0","id":1,"result":{"id":9}}`, 200, `{"id":9}`},
		{`{"jsonrpc":"2.0","id":1,"result":null}`, 200, `null`},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32001,"message":"Unauthorized"}}`, 401, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"permission denied for tenant"}}`, 403, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32601,"message":"Method not found"}}`, 404, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32602,"message":"Invalid params"}}`, 400, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32603,"message":"Internal error"}}`, 500, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"user not found"}}`, 404, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":404,"message":"gone"}}`, 404, ""},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":1001,"message":"insufficient balance"}}`, 400, ""},
	}
	for _, c := range cases {
		st, b := jsonrpcUnwrap(200, []byte(c.body))
		if st != c.want || c.wantBody != "" && string(b) != c.wantBody {
			t.Errorf("%s: got %d %s, want %d %s", c.body, st, b, c.want, c.wantBody)
		}
	}
	if st, _ := jsonrpcUnwrap(401, []byte(`{"error":"no"}`)); st != 401 {
		t.Errorf("HTTP error status kept: %d", st)
	}
}

func TestProtocolDetection(t *testing.T) {
	for specStr, want := range map[string]string{
		"http://localhost:3000/swagger.json": "openapi",
		"src/server/api/root.ts":             "trpc",
		"src/server/api/root.ts#appRouter":   "trpc",
		"trpc:./router.mjs":                  "trpc",
		"api.trpc.json":                      "trpc",
		"jsonrpc:http://localhost:8545":      "jsonrpc",
		"openrpc.json":                       "jsonrpc",
		"https://example.test/openrpc.json":  "jsonrpc",
		"connect://localhost:8080":           "connect",
		"connect:api/shop.proto":             "connect",
		"twirp:api/shop.proto":               "twirp",
		"grpc://localhost:50051":             "grpc",
		"api/shop.proto":                     "grpc",
		"http://localhost:4000/graphql":      "graphql",
	} {
		r := &Runner{cfg: &Config{Spec: specStr}}
		if got := r.protocol(); got != want {
			t.Errorf("%s: got %s want %s", specStr, got, want)
		}
	}
	r := &Runner{cfg: &Config{Spec: "api/shop.proto"}}
	r.cfg.API.Protocol = "JSON-RPC"
	if got := r.protocol(); got != "jsonrpc" {
		t.Errorf("--protocol json-rpc: %s", got)
	}
}
