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
	for attempt := 0; attempt < 2; attempt++ {
		md := metadata.MD{}
		am := t.r.am
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
		if attempt == 0 && t.r.am.Refresh(ctx, code) {
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

// grpcTarget returns host:port and whether to use TLS.
func (r *Runner) grpcTarget() (string, bool) {
	c := r.cfg
	for _, s := range []string{c.API.GRPC.Target, c.API.BaseURL, c.Spec} {
		if s == "" {
			continue
		}
		if u, err := url.Parse(s); err == nil && (u.Scheme == "grpc" || u.Scheme == "grpcs" || u.Scheme == "http" || u.Scheme == "https") && u.Host != "" {
			return u.Host, u.Scheme == "grpcs" || u.Scheme == "https" || c.API.GRPC.TLS
		}
		if !strings.Contains(s, "://") && strings.Contains(s, ":") && !strings.HasSuffix(s, ".proto") {
			return s, c.API.GRPC.TLS
		}
	}
	return "", false
}

func (r *Runner) loadGRPC(ctx context.Context, add addFn) bool {
	c := r.cfg
	target, useTLS := r.grpcTarget()
	if target == "" {
		add("Spec", "fail", "no gRPC server address", "use --spec grpc://host:port (server reflection) or --spec api.proto --api-url grpc://host:port")
		return false
	}
	creds := insecure.NewCredentials()
	if useTLS {
		creds = credentials.NewTLS(&tls.Config{})
	}
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(creds))
	if err != nil {
		add("Spec", "fail", err.Error(), "")
		return false
	}
	var svcs []protoreflect.ServiceDescriptor
	var protos []string
	for _, s := range strings.Split(c.Spec, ",") {
		if strings.HasSuffix(strings.TrimSpace(s), ".proto") {
			protos = append(protos, strings.TrimSpace(s))
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if len(protos) > 0 {
		svcs, err = grpcspec.FromProtoFiles(lctx, protos, c.API.GRPC.ImportPaths)
	} else {
		svcs, err = grpcspec.FromReflection(lctx, conn)
	}
	if err != nil {
		conn.Close()
		add("Spec", "fail", err.Error(), "enable server reflection, or pass the .proto files with --spec a.proto,b.proto and --api-url grpc://host:port")
		return false
	}
	r.sp, _ = grpcspec.Operations(svcs, "gRPC "+target)
	r.tr = &grpcTransport{r: r, conn: conn}
	unary := 0
	for _, o := range r.sp.Ops {
		if o.Unsupported == "" {
			unary++
		}
	}
	add("Spec", "ok", fmt.Sprintf("gRPC %s — %d services, %d unary methods (streaming methods are skipped)", target, len(svcs), unary), "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "no methods found", "")
		return false
	}
	return true
}
