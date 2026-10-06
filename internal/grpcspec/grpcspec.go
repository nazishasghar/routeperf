// Package grpcspec turns a gRPC service description (server reflection or
// .proto files) into routeperf operations, one per unary method, with
// request schemas the input generator can fill.
package grpcspec

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/bufbuild/protocompile"
	"github.com/getkin/kin-openapi/openapi3"
	"google.golang.org/grpc"
	rpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/nazishasghar/routeperf/internal/spec"
)

// Method is what the transport needs to call one RPC.
type Method struct {
	Input, Output protoreflect.MessageDescriptor
}

// FromReflection lists services through server reflection.
func FromReflection(ctx context.Context, conn *grpc.ClientConn) ([]protoreflect.ServiceDescriptor, error) {
	cl := rpb.NewServerReflectionClient(conn)
	stream, err := cl.ServerReflectionInfo(ctx)
	if err != nil {
		return nil, fmt.Errorf("server reflection: %w", err)
	}
	defer stream.CloseSend()
	ask := func(req *rpb.ServerReflectionRequest) (*rpb.ServerReflectionResponse, error) {
		if err := stream.Send(req); err != nil {
			return nil, err
		}
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil, fmt.Errorf("reflection stream closed")
		}
		if err != nil {
			return nil, err
		}
		if e := resp.GetErrorResponse(); e != nil {
			return nil, fmt.Errorf("reflection: %s", e.ErrorMessage)
		}
		return resp, nil
	}
	resp, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_ListServices{ListServices: ""}})
	if err != nil {
		return nil, fmt.Errorf("server reflection (enable it on the server, or pass .proto files): %w", err)
	}
	fds := map[string]*descriptorpb.FileDescriptorProto{}
	var names []string
	for _, s := range resp.GetListServicesResponse().GetService() {
		n := s.GetName()
		if strings.HasPrefix(n, "grpc.reflection.") || strings.HasPrefix(n, "grpc.health.") {
			continue
		}
		names = append(names, n)
		r, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: n}})
		if err != nil {
			return nil, err
		}
		for _, b := range r.GetFileDescriptorResponse().GetFileDescriptorProto() {
			fd := &descriptorpb.FileDescriptorProto{}
			if err := proto.Unmarshal(b, fd); err != nil {
				return nil, err
			}
			fds[fd.GetName()] = fd
		}
	}
	// fetch missing dependencies
	for changed := true; changed; {
		changed = false
		for _, fd := range fds {
			for _, dep := range fd.GetDependency() {
				if _, ok := fds[dep]; ok {
					continue
				}
				if _, err := protoregistry.GlobalFiles.FindFileByPath(dep); err == nil {
					continue
				}
				r, err := ask(&rpb.ServerReflectionRequest{MessageRequest: &rpb.ServerReflectionRequest_FileByFilename{FileByFilename: dep}})
				if err != nil {
					return nil, err
				}
				for _, b := range r.GetFileDescriptorResponse().GetFileDescriptorProto() {
					d := &descriptorpb.FileDescriptorProto{}
					if proto.Unmarshal(b, d) == nil {
						fds[d.GetName()] = d
						changed = true
					}
				}
			}
		}
	}
	set := &descriptorpb.FileDescriptorSet{}
	for _, fd := range fds {
		set.File = append(set.File, fd)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		// well-known imports (google/protobuf/*) come from the global registry
		files = new(protoregistry.Files)
		resolver := resolverChain{files, protoregistry.GlobalFiles}
		pending := set.File
		for len(pending) > 0 {
			var next []*descriptorpb.FileDescriptorProto
			for _, fd := range pending {
				f, err := protodesc.NewFile(fd, resolver)
				if err != nil {
					next = append(next, fd)
					continue
				}
				_ = files.RegisterFile(f)
			}
			if len(next) == len(pending) {
				return nil, fmt.Errorf("descriptors: %w", err)
			}
			pending = next
		}
	}
	sort.Strings(names)
	var out []protoreflect.ServiceDescriptor
	for _, n := range names {
		d, err := files.FindDescriptorByName(protoreflect.FullName(n))
		if err != nil {
			continue
		}
		if sd, ok := d.(protoreflect.ServiceDescriptor); ok {
			out = append(out, sd)
		}
	}
	return out, nil
}

type resolverChain []interface {
	FindFileByPath(string) (protoreflect.FileDescriptor, error)
	FindDescriptorByName(protoreflect.FullName) (protoreflect.Descriptor, error)
}

func (r resolverChain) FindFileByPath(p string) (protoreflect.FileDescriptor, error) {
	for _, x := range r {
		if f, err := x.FindFileByPath(p); err == nil {
			return f, nil
		}
	}
	return nil, protoregistry.NotFound
}

func (r resolverChain) FindDescriptorByName(n protoreflect.FullName) (protoreflect.Descriptor, error) {
	for _, x := range r {
		if d, err := x.FindDescriptorByName(n); err == nil {
			return d, nil
		}
	}
	return nil, protoregistry.NotFound
}

// FromProtoFiles compiles .proto files.
func FromProtoFiles(ctx context.Context, files []string, importPaths []string) ([]protoreflect.ServiceDescriptor, error) {
	paths := append([]string(nil), importPaths...)
	var rel []string
	for _, f := range files {
		dir := filepath.Dir(f)
		paths = append(paths, dir)
		rel = append(rel, filepath.Base(f))
	}
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: paths})}
	compiled, err := c.Compile(ctx, rel...)
	if err != nil {
		return nil, err
	}
	var out []protoreflect.ServiceDescriptor
	for _, f := range compiled {
		svcs := f.Services()
		for i := 0; i < svcs.Len(); i++ {
			out = append(out, svcs.Get(i))
		}
	}
	return out, nil
}

var readVerb = regexp.MustCompile(`^(Get|List|Search|Find|Read|Fetch|Query|Lookup|Count|Describe|BatchGet|Watch|Check|Stream)`)

var verbs = regexp.MustCompile(`^(Get|List|Search|Find|Read|Fetch|Query|Lookup|Count|Describe|BatchGet|Create|Update|Delete|Remove|Add|Set|Put|Patch|Upsert|Insert)`)

// Operations builds one operation per unary method; methods maps the full
// method path to its descriptors.
func Operations(services []protoreflect.ServiceDescriptor, title string) (*spec.Spec, map[string]Method) {
	sp := &spec.Spec{Title: title}
	methods := map[string]Method{}
	for _, sd := range services {
		ms := sd.Methods()
		for i := 0; i < ms.Len(); i++ {
			m := ms.Get(i)
			full := fmt.Sprintf("/%s/%s", sd.FullName(), m.Name())
			o := &spec.Operation{ID: string(m.Name()), Method: "RPC", Path: fmt.Sprintf("%s/%s", sd.Name(), m.Name()), Protocol: "grpc", Phase: "W", Depth: 1}
			if readVerb.MatchString(string(m.Name())) {
				o.Phase = "R"
			}
			o.Resource = strings.ToLower(verbs.ReplaceAllString(string(m.Name()), ""))
			if m.IsStreamingClient() || m.IsStreamingServer() {
				o.Unsupported = "streaming RPC"
			}
			o.Body = &spec.Body{ContentType: "application/grpc+json", Schema: Schema(m.Input(), 0)}
			o.GRPC = &spec.GRPCOp{FullMethod: full, Input: m.Input(), Output: m.Output()}
			methods[full] = Method{Input: m.Input(), Output: m.Output()}
			sp.Ops = append(sp.Ops, o)
		}
	}
	sort.Slice(sp.Ops, func(i, j int) bool { return sp.Ops[i].Path < sp.Ops[j].Path })
	return sp, methods
}

// Schema converts a message descriptor to an OpenAPI schema using protojson
// field names.
func Schema(md protoreflect.MessageDescriptor, depth int) *openapi3.Schema {
	switch md.FullName() {
	case "google.protobuf.Timestamp":
		s := openapi3.NewStringSchema()
		s.Format = "date-time"
		return s
	case "google.protobuf.Duration":
		s := openapi3.NewStringSchema()
		s.Example = "1s"
		return s
	case "google.protobuf.Int64Value", "google.protobuf.Int32Value", "google.protobuf.UInt64Value", "google.protobuf.UInt32Value":
		return openapi3.NewIntegerSchema()
	case "google.protobuf.StringValue":
		return openapi3.NewStringSchema()
	case "google.protobuf.BoolValue":
		return openapi3.NewBoolSchema()
	case "google.protobuf.DoubleValue", "google.protobuf.FloatValue":
		return openapi3.NewFloat64Schema()
	case "google.protobuf.Struct", "google.protobuf.Value", "google.protobuf.Any":
		return openapi3.NewObjectSchema()
	}
	s := openapi3.NewObjectSchema()
	if depth > 4 {
		return s
	}
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		f := fs.Get(i)
		if f.ContainingOneof() != nil && f.ContainingOneof().Fields().Get(0) != f && !f.HasOptionalKeyword() {
			continue // first member of each oneof only
		}
		var fsch *openapi3.Schema
		switch f.Kind() {
		case protoreflect.BoolKind:
			fsch = openapi3.NewBoolSchema()
		case protoreflect.EnumKind:
			fsch = openapi3.NewStringSchema()
			vals := f.Enum().Values()
			for j := 0; j < vals.Len(); j++ {
				if j == 0 && vals.Len() > 1 && strings.HasSuffix(string(vals.Get(j).Name()), "UNSPECIFIED") {
					continue
				}
				fsch.Enum = append(fsch.Enum, string(vals.Get(j).Name()))
			}
		case protoreflect.FloatKind, protoreflect.DoubleKind:
			fsch = openapi3.NewFloat64Schema()
		case protoreflect.StringKind:
			fsch = openapi3.NewStringSchema()
		case protoreflect.BytesKind:
			fsch = openapi3.NewStringSchema()
			fsch.Format = "byte"
		case protoreflect.MessageKind, protoreflect.GroupKind:
			if f.IsMap() {
				fsch = openapi3.NewObjectSchema()
			} else {
				fsch = Schema(f.Message(), depth+1)
			}
		default: // all integer kinds
			fsch = openapi3.NewIntegerSchema()
		}
		if f.IsList() {
			fsch = &openapi3.Schema{Type: &openapi3.Types{"array"}, Items: openapi3.NewSchemaRef("", fsch)}
		}
		s.Properties[f.JSONName()] = openapi3.NewSchemaRef("", fsch)
	}
	return s
}
