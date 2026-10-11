package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/nazishasghar/routeperf/internal/inputs"
	"github.com/nazishasghar/routeperf/internal/rpcspec"
	"github.com/nazishasghar/routeperf/internal/spec"
)

// rpcInput assembles a procedure's input from its "arg" values; false when
// the procedure takes none.
func rpcInput(op *spec.RPCOp, vars map[string]any) (any, bool) {
	switch op.Wrap {
	case "none":
		return nil, false
	case "value":
		v, ok := vars["input"]
		return v, ok
	case "array": // by position: omitted optional args before a given one are null
		n := 0
		for i, a := range op.Args {
			if _, ok := vars[a]; ok {
				n = i + 1
			}
		}
		out := make([]any, n)
		for i := range out {
			out[i] = vars[op.Args[i]]
		}
		return out, true
	}
	m := make(map[string]any, len(vars))
	for k, v := range vars {
		m[k] = v
	}
	return m, true
}

// ------------------------------------------------------------ tRPC

// trpcTransport calls tRPC procedures over its HTTP adapter: queries as
// GET <path>/<proc>?input=<json>, mutations as POST with a JSON body.
type trpcTransport struct {
	r         *Runner
	path      string
	superjson bool
}

func (t *trpcTransport) Close() {}

func (t *trpcTransport) Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	op := req.Op.RPC
	if op == nil {
		return t.r.sendHTTP(ctx, req)
	}
	input, has := rpcInput(op, req.Vars)
	if has && t.superjson {
		input = map[string]any{"json": input}
	}
	h := &inputs.Request{Method: "POST", Path: strings.TrimRight(t.path, "/") + "/" + op.Name, Query: url.Values{}, Header: req.Header,
		ContentType: "application/json", Role: req.Role}
	if op.Kind == "query" {
		h.Method = "GET"
		if has {
			b, err := json.Marshal(input)
			if err != nil {
				return 0, nil, nil, 0, err
			}
			h.Query.Set("input", string(b))
		}
	} else if has {
		h.Body = input
	}
	status, body, hdr, ms, err := t.r.sendHTTP(ctx, h)
	if err != nil {
		return status, body, hdr, ms, err
	}
	status, body = trpcUnwrap(status, body, t.superjson)
	return status, body, hdr, ms, nil
}

// trpcUnwrap returns a success's result.data (result.data.json with
// superjson), so IDs and cursors are read from the procedure's own output;
// an error keeps its envelope and gets the HTTP status tRPC chose for it.
func trpcUnwrap(status int, body []byte, superjson bool) (int, []byte) {
	var env struct {
		Result *struct {
			Data json.RawMessage `json:"data"`
		} `json:"result"`
		Error json.RawMessage `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return status, body
	}
	if env.Result != nil && status < 300 {
		data := env.Result.Data
		if superjson {
			var sj struct {
				JSON json.RawMessage `json:"json"`
			}
			if json.Unmarshal(data, &sj) == nil && sj.JSON != nil {
				data = sj.JSON
			}
		}
		if len(data) == 0 {
			data = []byte("null")
		}
		return status, data
	}
	if env.Error != nil && status < 400 { // shouldn't happen over HTTP, but batch-style servers answer 200
		var e struct {
			JSON *struct {
				Data struct {
					HTTPStatus int `json:"httpStatus"`
				} `json:"data"`
			} `json:"json"`
			Data struct {
				HTTPStatus int `json:"httpStatus"`
			} `json:"data"`
		}
		_ = json.Unmarshal(env.Error, &e)
		status = e.Data.HTTPStatus
		if e.JSON != nil && status == 0 {
			status = e.JSON.Data.HTTPStatus
		}
		if status < 400 {
			status = 500
		}
	}
	return status, body
}

func isTSFile(s string) bool {
	for _, ext := range []string{".ts", ".tsx", ".mts", ".cts"} {
		if strings.HasSuffix(strings.ToLower(s), ext) {
			return true
		}
	}
	return false
}

func (r *Runner) loadTRPC(ctx context.Context, add addFn) bool {
	c := r.cfg
	src := strings.TrimPrefix(c.Spec, "trpc:")
	var data []byte
	var err error
	file, _, _ := strings.Cut(src, "#")
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") || strings.HasSuffix(strings.ToLower(file), ".json") {
		data, err = r.fetch(ctx, src)
	} else {
		lctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		data, err = rpcspec.ExtractRouter(lctx, src, c.API.TRPC.Command)
	}
	var m *rpcspec.Manifest
	if err == nil {
		m, err = rpcspec.ParseManifest(data)
	}
	if err != nil {
		add("Spec", "fail", err.Error(), "point --spec at the file that exports your tRPC app router (e.g. src/server/api/root.ts, needs Node; "+
			"name the export with file.ts#appRouter) or at a manifest from `routeperf trpc-manifest`; set api.trpc.command if tsx isn't the right loader")
		return false
	}
	tf := strings.ToLower(c.API.TRPC.Transformer)
	if tf == "" {
		tf = m.TRPC.Transformer
	}
	switch tf {
	case "", "none", "superjson":
	default:
		add("Spec", "fail", fmt.Sprintf("the router uses a %s data transformer", tf), "routeperf speaks plain JSON and superjson; set api.trpc.transformer: superjson if yours is superjson-compatible")
		return false
	}
	r.sp = rpcspec.TRPCOperations(m, "tRPC router")
	r.tr = &trpcTransport{r: r, path: c.API.TRPC.Path, superjson: tf == "superjson"}
	nq, nm, ns := 0, 0, 0
	for _, o := range r.sp.Ops {
		switch {
		case o.Unsupported != "":
			ns++
		case o.Phase == "R":
			nq++
		default:
			nm++
		}
	}
	detail := fmt.Sprintf("tRPC router — %d queries, %d mutations (endpoint %s", nq, nm, c.API.TRPC.Path)
	if tf == "superjson" {
		detail += ", superjson"
	}
	detail += ")"
	if ns > 0 {
		detail += fmt.Sprintf("; %d subscriptions skipped", ns)
	}
	add("Spec", "ok", detail, "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "router has no procedures", "")
		return false
	}
	return true
}

// ------------------------------------------------------------ JSON-RPC

// jsonrpcTransport posts JSON-RPC 2.0 calls to one endpoint.
type jsonrpcTransport struct {
	r    *Runner
	path string
	seq  atomic.Int64
}

func (t *jsonrpcTransport) Close() {}

func (t *jsonrpcTransport) Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	op := req.Op.RPC
	if op == nil {
		return t.r.sendHTTP(ctx, req)
	}
	call := map[string]any{"jsonrpc": "2.0", "method": op.Name, "id": t.seq.Add(1)}
	if params, has := rpcInput(op, req.Vars); has {
		call["params"] = params
	}
	h := &inputs.Request{Method: "POST", Path: t.path, Query: url.Values{}, Header: req.Header, ContentType: "application/json", Role: req.Role, Body: call}
	status, body, hdr, ms, err := t.r.sendHTTP(ctx, h)
	if err != nil {
		return status, body, hdr, ms, err
	}
	status, body = jsonrpcUnwrap(status, body)
	return status, body, hdr, ms, nil
}

var (
	authErr   = regexp.MustCompile(`(?i)unauthori[sz]ed|unauthenticated|not authenticated|invalid token|login required`)
	forbidErr = regexp.MustCompile(`(?i)forbidden|permission denied|not allowed|access denied`)
	notFound  = regexp.MustCompile(`(?i)not found|no such|does not exist`)
)

// jsonrpcUnwrap returns a success's result, and maps an error object to the
// HTTP status it means: JSON-RPC errors arrive as 200.
func jsonrpcUnwrap(status int, body []byte) (int, []byte) {
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &env) != nil {
		return status, body
	}
	if env.Error == nil {
		if status < 300 && env.Result != nil {
			return status, env.Result
		}
		return status, body
	}
	if status >= 400 {
		return status, body
	}
	e := env.Error
	switch {
	case e.Code == 401 || e.Code == 403 || e.Code == 404 || e.Code == 409 || e.Code == 429:
		return e.Code, body
	case authErr.MatchString(e.Message):
		return 401, body
	case forbidErr.MatchString(e.Message):
		return 403, body
	case e.Code == -32601: // method not found
		return 404, body
	case e.Code == -32700 || e.Code == -32600 || e.Code == -32602: // parse error, invalid request, invalid params
		return 400, body
	case e.Code == -32603 || e.Code <= -32000 && e.Code >= -32099: // internal / server errors
		if notFound.MatchString(e.Message) {
			return 404, body
		}
		return 500, body
	case notFound.MatchString(e.Message):
		return 404, body
	}
	return 400, body // application error
}

// jsonrpcEndpoint splits a jsonrpc:http://host/rpc spec into base URL and path.
func (r *Runner) jsonrpcEndpoint() (string, string) {
	c := r.cfg
	s := strings.TrimPrefix(strings.TrimPrefix(c.Spec, "jsonrpc:"), "openrpc:")
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if u, err := url.Parse(s); err == nil && !strings.HasSuffix(strings.ToLower(u.Path), ".json") {
			path := u.Path
			u.Path, u.RawQuery = "", ""
			if c.API.JSONRPC.Path != "" {
				path = c.API.JSONRPC.Path
			}
			return u.String(), path
		}
	}
	return c.API.BaseURL, c.API.JSONRPC.Path
}

func (r *Runner) loadJSONRPC(ctx context.Context, add addFn) bool {
	c := r.cfg
	_, path := r.jsonrpcEndpoint()
	src := strings.TrimPrefix(strings.TrimPrefix(c.Spec, "jsonrpc:"), "openrpc:")
	var data []byte
	var err error
	how := "OpenRPC document"
	if strings.HasSuffix(strings.ToLower(src), ".json") || !strings.Contains(src, "://") {
		data, err = r.fetch(ctx, src)
	} else { // the endpoint itself: OpenRPC service discovery
		how = "rpc.discover"
		var status int
		status, data, _, _, err = r.sendHTTP(ctx, &inputs.Request{Method: "POST", Path: path, Query: url.Values{}, Header: map[string]string{},
			ContentType: "application/json", Role: r.specRole(), Body: rpcspec.DiscoverRequest})
		if err == nil && status != 200 {
			err = fmt.Errorf("rpc.discover at %s: HTTP %d", path, status)
		}
	}
	var doc *rpcspec.OpenRPC
	if err == nil {
		doc, err = rpcspec.ParseOpenRPC(data, c.API.JSONRPC.ParamsBy)
	}
	if err != nil {
		add("Spec", "fail", err.Error(), "point --spec at the JSON-RPC endpoint (jsonrpc:http://host/rpc, serving rpc.discover) or at an OpenRPC document (openrpc.json)")
		return false
	}
	if path == "" {
		path = "/"
		if len(doc.Servers) > 0 {
			if u, err := url.Parse(doc.Servers[0]); err == nil && u.Path != "" {
				path = u.Path
			}
		}
	}
	r.sp = doc.Ops
	if r.sp.Title == "" {
		r.sp.Title = "JSON-RPC API"
	}
	r.tr = &jsonrpcTransport{r: r, path: path}
	nr := 0
	for _, o := range r.sp.Ops {
		if o.Phase == "R" {
			nr++
		}
	}
	add("Spec", "ok", fmt.Sprintf("JSON-RPC %s — %d methods (%d read, %d write; endpoint %s)", how, len(r.sp.Ops), nr, len(r.sp.Ops)-nr, path), "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "the document lists no methods", "")
		return false
	}
	return true
}

// ------------------------------------------------------------ Connect / Twirp

// protoHTTPTransport calls unary protobuf methods over HTTP with JSON
// bodies: Connect (POST /pkg.Service/Method) and Twirp (POST
// /twirp/pkg.Service/Method).
type protoHTTPTransport struct {
	r      *Runner
	prefix string
	header map[string]string
}

func (t *protoHTTPTransport) Close() {}

func (t *protoHTTPTransport) Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	op := req.Op.GRPC
	if op == nil {
		return t.r.sendHTTP(ctx, req)
	}
	// round-trip through the message type: protojson field names and enum
	// spellings the server accepts
	in := dynamicpb.NewMessage(op.Input.(protoreflect.MessageDescriptor))
	if req.Body != nil {
		raw, _, err := inputs.Encode(&inputs.Request{Body: req.Body})
		if err != nil {
			return 0, nil, nil, 0, err
		}
		if err := (protojson.UnmarshalOptions{DiscardUnknown: true}).Unmarshal(raw, in); err != nil {
			return 400, []byte(err.Error()), nil, 0, nil
		}
	}
	body, err := protojson.Marshal(in)
	if err != nil {
		return 0, nil, nil, 0, err
	}
	hdr := make(map[string]string, len(req.Header)+len(t.header))
	for k, v := range t.header {
		hdr[k] = v
	}
	for k, v := range req.Header {
		hdr[k] = v
	}
	return t.r.sendHTTP(ctx, &inputs.Request{Method: "POST", Path: strings.TrimRight(t.prefix, "/") + op.FullMethod, Query: url.Values{},
		Header: hdr, ContentType: "application/json", Role: req.Role, Body: json.RawMessage(body)})
}
