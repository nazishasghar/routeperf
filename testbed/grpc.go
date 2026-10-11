package main

import (
	"context"
	"database/sql"
	"log"
	"net"
	"net/url"

	"github.com/bufbuild/protocompile"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"
)

// shopProto is the protobuf API served over gRPC, Connect and Twirp
// (GET /shop.proto returns it for runs that load .proto files).
const shopProto = `syntax = "proto3";
package routeperf.testbed;

message GetUserRequest { int64 id = 1; }
message User { int64 id = 1; string email = 2; string name = 3; string country = 4; }
message ListUserOrdersRequest { int64 user_id = 1; int32 page_size = 2; }
message SearchOrdersRequest { string status = 1; int32 page_size = 2; }
message Order { int64 id = 1; int64 user_id = 2; string status = 3; int32 total_cents = 4; }
message OrderList { repeated Order orders = 1; }
message RecentUsersRequest { int32 page_size = 1; }
message UserList { repeated User users = 1; }
message CreateUserRequest { string email = 1; string name = 2; string country = 3; }

service Shop {
  rpc GetUser(GetUserRequest) returns (User);
  rpc ListUserOrders(ListUserOrdersRequest) returns (OrderList);
  rpc SearchOrders(SearchOrdersRequest) returns (OrderList);
  // no read verb in the name: the idempotency option marks it as a read
  rpc RecentUsers(RecentUsersRequest) returns (UserList) { option idempotency_level = NO_SIDE_EFFECTS; }
  rpc CreateUser(CreateUserRequest) returns (User);
}
`

// shopFile compiles shopProto (no protoc needed) and registers it.
func shopFile() protoreflect.FileDescriptor {
	c := protocompile.Compiler{Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{
		Accessor: protocompile.SourceAccessorFromMap(map[string]string{"routeperf/testbed.proto": shopProto})})}
	files, err := c.Compile(context.Background(), "routeperf/testbed.proto")
	must(err)
	must(protoregistry.GlobalFiles.RegisterFile(files[0]))
	return files[0]
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

type shopMethod func(ctx context.Context, in, out *dynamicpb.Message) error

var (
	shopSvc     protoreflect.ServiceDescriptor
	shopMethods map[string]shopMethod
)

func pbGet(m *dynamicpb.Message, f string) protoreflect.Value {
	return m.Get(m.Descriptor().Fields().ByName(protoreflect.Name(f)))
}

func pbSet(m *dynamicpb.Message, f string, v protoreflect.Value) {
	m.Set(m.Descriptor().Fields().ByName(protoreflect.Name(f)), v)
}

func pageSize(in *dynamicpb.Message) int {
	n := int(pbGet(in, "page_size").Int())
	if n < 1 {
		return 20
	}
	return min(n, 1000)
}

func setUser(m *dynamicpb.Message, id int64, email, name, country string) {
	pbSet(m, "id", protoreflect.ValueOfInt64(id))
	pbSet(m, "email", protoreflect.ValueOfString(email))
	pbSet(m, "name", protoreflect.ValueOfString(name))
	pbSet(m, "country", protoreflect.ValueOfString(country))
}

func orderList(out *dynamicpb.Message, rows *sql.Rows) error {
	orders, err := scanOrders(rows)
	if err != nil {
		return status.Error(codes.Internal, err.Error())
	}
	fd := out.Descriptor().Fields().ByName("orders")
	list := out.Mutable(fd).List()
	for _, o := range orders {
		m := dynamicpb.NewMessage(fd.Message())
		pbSet(m, "id", protoreflect.ValueOfInt64(o.ID))
		pbSet(m, "user_id", protoreflect.ValueOfInt64(o.UserID))
		pbSet(m, "status", protoreflect.ValueOfString(o.Status))
		pbSet(m, "total_cents", protoreflect.ValueOfInt32(int32(o.TotalCents)))
		list.Append(protoreflect.ValueOfMessage(m))
	}
	return nil
}

// initShop builds the Shop methods shared by gRPC, Connect and Twirp.
func initShop() {
	shopSvc = shopFile().Services().ByName("Shop")
	shopMethods = map[string]shopMethod{
		"GetUser": func(ctx context.Context, in, out *dynamicpb.Message) error {
			var id int64
			var email string
			var name, country sql.NullString
			err := db.QueryRowContext(ctx, grpcSQL(ctx, "SELECT id, email, name, country FROM users WHERE id = ?"), pbGet(in, "id").Int()).Scan(&id, &email, &name, &country)
			if err == sql.ErrNoRows {
				return status.Error(codes.NotFound, "no such user")
			} else if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			setUser(out, id, email, name.String, country.String)
			return nil
		},
		// planted: orders.user_id has no index
		"ListUserOrders": func(ctx context.Context, in, out *dynamicpb.Message) error {
			rows, err := db.QueryContext(ctx, grpcSQL(ctx, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), pbGet(in, "user_id").Int(), pageSize(in))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			return orderList(out, rows)
		},
		"SearchOrders": func(ctx context.Context, in, out *dynamicpb.Message) error {
			st := pbGet(in, "status").String()
			if st == "" {
				st = "paid"
			}
			rows, err := db.QueryContext(ctx, grpcSQL(ctx, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? ORDER BY created_at DESC LIMIT ?"), st, pageSize(in))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			return orderList(out, rows)
		},
		"RecentUsers": func(ctx context.Context, in, out *dynamicpb.Message) error {
			rows, err := db.QueryContext(ctx, grpcSQL(ctx, "SELECT id, email, name, country FROM users ORDER BY id DESC LIMIT ?"), pageSize(in))
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			defer rows.Close()
			fd := out.Descriptor().Fields().ByName("users")
			list := out.Mutable(fd).List()
			for rows.Next() {
				var id int64
				var email string
				var name, country sql.NullString
				if err := rows.Scan(&id, &email, &name, &country); err != nil {
					return status.Error(codes.Internal, err.Error())
				}
				m := dynamicpb.NewMessage(fd.Message())
				setUser(m, id, email, name.String, country.String)
				list.Append(protoreflect.ValueOfMessage(m))
			}
			return nil
		},
		"CreateUser": func(ctx context.Context, in, out *dynamicpb.Message) error {
			email, name, country := pbGet(in, "email").String(), pbGet(in, "name").String(), pbGet(in, "country").String()
			if email == "" {
				return status.Error(codes.InvalidArgument, "email required")
			}
			id, err := insertUser(grpcSQL(ctx, ""), email, name, country)
			if err != nil {
				return status.Error(codes.Internal, err.Error())
			}
			setUser(out, id, email, name, truncate(country, 2))
			return nil
		},
	}
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// insertUser adds a user; tag is a trailing SQL comment (sqlcommenter).
func insertUser(tag, email, name, country string) (int64, error) {
	country = truncate(country, 2)
	if dialect == "postgres" {
		var id int64
		err := db.QueryRow(q("INSERT INTO users (email, name, country) VALUES (?, ?, ?) RETURNING id")+tag, email, name, country).Scan(&id)
		return id, err
	}
	res, err := db.Exec("INSERT INTO users (email, name, country) VALUES (?, ?, ?)"+tag, email, name, country)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func grpcAuth(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	for _, v := range md.Get("authorization") {
		if v == "Bearer testtoken" {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "unauthorized")
}

func serveGRPC(addr string) {
	var methods []grpc.MethodDesc
	for i := 0; i < shopSvc.Methods().Len(); i++ {
		m := shopSvc.Methods().Get(i)
		fn := shopMethods[string(m.Name())]
		methods = append(methods, grpc.MethodDesc{MethodName: string(m.Name()), Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			in := dynamicpb.NewMessage(m.Input())
			if err := dec(in); err != nil {
				return nil, err
			}
			if err := grpcAuth(ctx); err != nil {
				return nil, err
			}
			out := dynamicpb.NewMessage(m.Output())
			if err := fn(ctx, in, out); err != nil {
				return nil, err
			}
			return out, nil
		}})
	}
	sd := grpc.ServiceDesc{ServiceName: string(shopSvc.FullName()), HandlerType: (*any)(nil), Metadata: "routeperf/testbed.proto", Methods: methods}
	s := grpc.NewServer()
	s.RegisterService(&sd, struct{}{})
	reflection.Register(s)
	ln, err := net.Listen("tcp", addr)
	must(err)
	log.Printf("testbed gRPC (%s) listening on %s", dialect, addr)
	go func() { must(s.Serve(ln)) }()
}
