package main

import (
	"context"
	"database/sql"
	"log"
	"net"
	"net/url"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

func pbField(name string, n int32, t descriptorpb.FieldDescriptorProto_Type, typeName string, repeated bool) *descriptorpb.FieldDescriptorProto {
	label := descriptorpb.FieldDescriptorProto_LABEL_OPTIONAL
	if repeated {
		label = descriptorpb.FieldDescriptorProto_LABEL_REPEATED
	}
	f := &descriptorpb.FieldDescriptorProto{Name: proto.String(name), Number: proto.Int32(n), Type: t.Enum(), Label: label.Enum()}
	if typeName != "" {
		f.TypeName = proto.String(typeName)
	}
	return f
}

func pbMsg(name string, fields ...*descriptorpb.FieldDescriptorProto) *descriptorpb.DescriptorProto {
	return &descriptorpb.DescriptorProto{Name: proto.String(name), Field: fields}
}

const (
	i64 = descriptorpb.FieldDescriptorProto_TYPE_INT64
	i32 = descriptorpb.FieldDescriptorProto_TYPE_INT32
	str = descriptorpb.FieldDescriptorProto_TYPE_STRING
	msg = descriptorpb.FieldDescriptorProto_TYPE_MESSAGE
)

// shopFile describes the gRPC API (built in code: no protoc needed).
func shopFile() protoreflect.FileDescriptor {
	method := func(name, in, out string) *descriptorpb.MethodDescriptorProto {
		return &descriptorpb.MethodDescriptorProto{Name: proto.String(name), InputType: proto.String(".routeperf.testbed." + in), OutputType: proto.String(".routeperf.testbed." + out)}
	}
	fdp := &descriptorpb.FileDescriptorProto{
		Name: proto.String("routeperf/testbed.proto"), Package: proto.String("routeperf.testbed"), Syntax: proto.String("proto3"),
		MessageType: []*descriptorpb.DescriptorProto{
			pbMsg("GetUserRequest", pbField("id", 1, i64, "", false)),
			pbMsg("User", pbField("id", 1, i64, "", false), pbField("email", 2, str, "", false), pbField("name", 3, str, "", false), pbField("country", 4, str, "", false)),
			pbMsg("ListUserOrdersRequest", pbField("user_id", 1, i64, "", false), pbField("page_size", 2, i32, "", false)),
			pbMsg("SearchOrdersRequest", pbField("status", 1, str, "", false), pbField("page_size", 2, i32, "", false)),
			pbMsg("Order", pbField("id", 1, i64, "", false), pbField("user_id", 2, i64, "", false), pbField("status", 3, str, "", false), pbField("total_cents", 4, i32, "", false)),
			pbMsg("OrderList", pbField("orders", 1, msg, ".routeperf.testbed.Order", true)),
		},
		Service: []*descriptorpb.ServiceDescriptorProto{{Name: proto.String("Shop"), Method: []*descriptorpb.MethodDescriptorProto{
			method("GetUser", "GetUserRequest", "User"),
			method("ListUserOrders", "ListUserOrdersRequest", "OrderList"),
			method("SearchOrders", "SearchOrdersRequest", "OrderList"),
		}}},
	}
	fd, err := protodesc.NewFile(fdp, protoregistry.GlobalFiles)
	must(err)
	must(protoregistry.GlobalFiles.RegisterFile(fd))
	return fd
}

// grpcSQL tags SQL with the call's traceparent (sqlcommenter).
func grpcSQL(ctx context.Context, s string) string {
	s = q(s)
	if !sqlcomment {
		return s
	}
	if md, ok := metadata.FromIncomingContext(ctx); ok && len(md.Get("traceparent")) > 0 {
		return s + " /*traceparent='" + url.QueryEscape(md.Get("traceparent")[0]) + "'*/"
	}
	return s
}

func serveGRPC(addr string) {
	fd := shopFile()
	svc := fd.Services().ByName("Shop")
	auth := func(ctx context.Context) error {
		md, _ := metadata.FromIncomingContext(ctx)
		for _, v := range md.Get("authorization") {
			if v == "Bearer testtoken" {
				return nil
			}
		}
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	handler := func(name string, fn func(ctx context.Context, in, out *dynamicpb.Message) error) grpc.MethodDesc {
		m := svc.Methods().ByName(protoreflect.Name(name))
		return grpc.MethodDesc{MethodName: name, Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			in := dynamicpb.NewMessage(m.Input())
			if err := dec(in); err != nil {
				return nil, err
			}
			if err := auth(ctx); err != nil {
				return nil, err
			}
			out := dynamicpb.NewMessage(m.Output())
			if err := fn(ctx, in, out); err != nil {
				return nil, err
			}
			return out, nil
		}}
	}
	get := func(m *dynamicpb.Message, f string) protoreflect.Value {
		return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(f)))
	}
	set := func(m *dynamicpb.Message, f string, v protoreflect.Value) {
		m.Set(m.Descriptor().Fields().ByName(protoreflect.Name(f)), v)
	}
	orderList := func(ctx context.Context, out *dynamicpb.Message, rows *sql.Rows) error {
		orders, err := scanOrders(rows)
		if err != nil {
			return status.Error(codes.Internal, err.Error())
		}
		list := out.Mutable(out.Descriptor().Fields().ByName("orders")).List()
		od := out.Descriptor().Fields().ByName("orders").Message()
		for _, o := range orders {
			m := dynamicpb.NewMessage(od)
			set(m, "id", protoreflect.ValueOfInt64(o.ID))
			set(m, "user_id", protoreflect.ValueOfInt64(o.UserID))
			set(m, "status", protoreflect.ValueOfString(o.Status))
			set(m, "total_cents", protoreflect.ValueOfInt32(int32(o.TotalCents)))
			list.Append(protoreflect.ValueOfMessage(m))
		}
		return nil
	}
	pageSize := func(in *dynamicpb.Message) int {
		n := int(get(in, "page_size").Int())
		if n < 1 {
			return 20
		}
		return min(n, 1000)
	}
	sd := grpc.ServiceDesc{ServiceName: "routeperf.testbed.Shop", HandlerType: (*any)(nil), Metadata: "routeperf/testbed.proto", Methods: []grpc.MethodDesc{
		handler("GetUser", func(ctx context.Context, in, out *dynamicpb.Message) error {
			var id int64
			var email string
			var name, country sql.NullString
			err := db.QueryRowContext(ctx, grpcSQL(ctx, "SELECT id, email, name, country FROM users WHERE id = ?"), get(in, "id").Int()).Scan(&id, &email, &name, &country)
			if err == sql.ErrNoRows {
				return status.Error(codes.NotFound, "no such user")
			} else if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			set(out, "id", protoreflect.ValueOfInt64(id))
			set(out, "email", protoreflect.ValueOfString(email))
			set(out, "name", protoreflect.ValueOfString(name.String))
			set(out, "country", protoreflect.ValueOfString(country.String))
			return nil
		}),
		// planted: orders.user_id has no index
		handler("ListUserOrders", func(ctx context.Context, in, out *dynamicpb.Message) error {
			rows, err := db.QueryContext(ctx, grpcSQL(ctx, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), get(in, "user_id").Int(), pageSize(in))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			return orderList(ctx, out, rows)
		}),
		handler("SearchOrders", func(ctx context.Context, in, out *dynamicpb.Message) error {
			st := get(in, "status").String()
			if st == "" {
				st = "paid"
			}
			rows, err := db.QueryContext(ctx, grpcSQL(ctx, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? ORDER BY created_at DESC LIMIT ?"), st, pageSize(in))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			return orderList(ctx, out, rows)
		}),
	}}
	s := grpc.NewServer()
	s.RegisterService(&sd, struct{}{})
	reflection.Register(s)
	ln, err := net.Listen("tcp", addr)
	must(err)
	log.Printf("testbed gRPC (%s) listening on %s", dialect, addr)
	go func() { must(s.Serve(ln)) }()
}
