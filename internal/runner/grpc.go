package runner

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/nazishasghar/routeperf/internal/grpcspec"
	"github.com/nazishasghar/routeperf/internal/inputs"
)

// grpcTransport invokes unary methods with dynamic messages built from the
// generated JSON request.
type grpcTransport struct {
	r    *Runner
	conn *grpc.ClientConn
}

func (t *grpcTransport) Close() { t.conn.Close() }

var grpcHTTP = map[codes.Code]int{codes.OK: 200, codes.InvalidArgument: 400, codes.FailedPrecondition: 400, codes.OutOfRange: 400,
	codes.Unauthenticated: 401, codes.PermissionDenied: 403, codes.NotFound: 404, codes.AlreadyExists: 409, codes.Aborted: 409,
	codes.ResourceExhausted: 429, codes.Canceled: 499, codes.Unimplemented: 501, codes.Unavailable: 503, codes.DeadlineExceeded: 504}

func (t *grpcTransport) Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	op := req.Op.GRPC
	if op == nil {
		return 0, nil, nil, 0, fmt.Errorf("not a gRPC operation")
	}
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
	am := t.r.amFor(req.Role)
	for attempt := 0; attempt < 2; attempt++ {
		md := metadata.MD{}
		if b := am.Bearer(); b != "" {
			md.Set("authorization", "Bearer "+b)
		}
		for k, v := range am.HeaderMap() {
			md.Set(strings.ToLower(k), v)
		}
		if ck := am.CookieHeader(); ck != "" {
			md.Set("cookie", ck)
		}
		for k, v := range req.Header {
			md.Set(strings.ToLower(k), v)
		}
		out := dynamicpb.NewMessage(op.Output.(protoreflect.MessageDescriptor))
		cctx, cancel := context.WithTimeout(metadata.NewOutgoingContext(ctx, md), t.r.cfg.timeout())
		var hdr metadata.MD
		t0 := time.Now()
		err := t.conn.Invoke(cctx, op.FullMethod, in, out, grpc.Header(&hdr))
		ms := float64(time.Since(t0).Microseconds()) / 1000
		cancel()
		st := status.Convert(err)
		code, ok := grpcHTTP[st.Code()]
		if !ok {
			code = 500
		}
		if attempt == 0 && am.Refresh(ctx, code) {
			continue
		}
		h := http.Header{}
		for k, v := range hdr {
			for _, x := range v {
				h.Add(k, x)
			}
		}
		if err != nil {
			return code, []byte(st.Message()), h, ms, nil
		}
		body, _ := protojson.Marshal(out)
		return code, body, h, ms, nil
	}
	return 0, nil, nil, 0, fmt.Errorf("unreachable")
}

// grpcTarget returns host:port and whether to use TLS: the gRPC server, or
// for Connect/Twirp the server reflection is asked (the spec's address
// before the HTTP API URL).
func (r *Runner) grpcTarget() (string, bool) {
	c := r.cfg
	srcs := []string{c.API.GRPC.Target, c.API.BaseURL, c.Spec}
	if r.protocol() != "grpc" {
		srcs = []string{c.API.GRPC.Target, c.Spec, c.API.BaseURL}
	}
	for _, s := range srcs {
		if s == "" {
			continue
		}
		if u, err := url.Parse(s); err == nil && u.Host != "" {
			switch u.Scheme {
			case "grpc", "http", "connect":
				return u.Host, c.API.GRPC.TLS
			case "grpcs", "https", "connects":
				return u.Host, true
			}
		}
		if !strings.Contains(s, "://") && strings.Contains(s, ":") && !strings.HasSuffix(s, ".proto") {
			return s, c.API.GRPC.TLS
		}
	}
	return "", false
}

func (r *Runner) loadGRPC(ctx context.Context, add addFn) bool {
	c := r.cfg
	proto := r.protocol()
	var protos []string
	for _, s := range strings.Split(strings.TrimPrefix(c.Spec, proto+":"), ",") {
		if strings.HasSuffix(strings.TrimSpace(s), ".proto") {
			protos = append(protos, strings.TrimSpace(s))
		}
	}
	target, useTLS := r.grpcTarget()
	if target == "" && (proto == "grpc" || len(protos) == 0) {
		fix := "use --spec grpc://host:port (server reflection) or --spec api.proto --api-url grpc://host:port"
		if proto != "grpc" {
			fix = "pass the .proto files: --spec api.proto --protocol " + proto + " --api-url http://host:port"
		}
		add("Spec", "fail", "no "+proto+" service description", fix)
		return false
	}
	var conn *grpc.ClientConn
	if target != "" {
		creds := insecure.NewCredentials()
		if useTLS {
			creds = credentials.NewTLS(&tls.Config{})
		}
		var err error
		if conn, err = grpc.NewClient(target, grpc.WithTransportCredentials(creds)); err != nil {
			add("Spec", "fail", err.Error(), "")
			return false
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	var svcs []protoreflect.ServiceDescriptor
	var err error
	if len(protos) > 0 {
		svcs, err = grpcspec.FromProtoFiles(lctx, protos, c.API.GRPC.ImportPaths)
	} else {
		svcs, err = grpcspec.FromReflection(lctx, conn)
	}
	if err != nil {
		if conn != nil {
			conn.Close()
		}
		fix := "enable server reflection, or pass the .proto files with --spec a.proto,b.proto and --api-url grpc://host:port"
		if proto != "grpc" {
			fix = "pass the .proto files with --spec a.proto,b.proto (or serve gRPC reflection, e.g. connectrpc.com/grpcreflect, at api.grpc.target)"
		}
		add("Spec", "fail", err.Error(), fix)
		return false
	}
	where := target
	if proto != "grpc" {
		where = c.API.BaseURL
	}
	names := map[string]string{"grpc": "gRPC", "connect": "Connect", "twirp": "Twirp"}
	r.sp, _ = grpcspec.Operations(svcs, names[proto]+" "+where)
	if proto == "grpc" {
		r.tr = &grpcTransport{r: r, conn: conn}
	} else {
		if conn != nil {
			conn.Close()
		}
		prefix := c.API.RPCPrefix
		hdr := map[string]string{}
		if proto == "twirp" && prefix == "" {
			prefix = "/twirp"
		}
		if proto == "connect" {
			hdr["Connect-Protocol-Version"] = "1"
		}
		for _, o := range r.sp.Ops {
			o.Protocol = proto
		}
		r.tr = &protoHTTPTransport{r: r, prefix: prefix, header: hdr}
	}
	unary := 0
	for _, o := range r.sp.Ops {
		if o.Unsupported == "" {
			unary++
		}
	}
	add("Spec", "ok", fmt.Sprintf("%s %s — %d services, %d unary methods (streaming methods are skipped)", names[proto], where, len(svcs), unary), "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "no methods found", "")
		return false
	}
	return true
}
