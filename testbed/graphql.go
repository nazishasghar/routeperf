package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"
	"github.com/vektah/gqlparser/v2/validator"
)

const gqlSDL = `
type Query {
  users(first: Int = 20): [User!]!
  user(id: ID!): User
  orders(first: Int = 20, status: String = "paid"): [Order!]!
}
type Mutation {
  createUser(email: String!, name: String, country: String): User!
}
type User {
  id: ID!
  email: String!
  name: String
  country: String
  notes(first: Int = 5): [Note!]!
}
type Note { id: ID! body: String! }
type Order { id: ID! userId: ID! status: String! totalCents: Int! createdAt: String! }
`

var gqlSchema = gqlparser.MustLoadSchema(&ast.Source{Name: "testbed.graphql", Input: gqlSDL})

type gqlError struct {
	Message string `json:"message"`
}

func graphqlHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query         string         `json:"query"`
		Variables     map[string]any `json:"variables"`
		OperationName string         `json:"operationName"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, 400, map[string]any{"errors": []gqlError{{err.Error()}}})
		return
	}
	doc, errs := gqlparser.LoadQuery(gqlSchema, req.Query)
	if errs != nil {
		writeJSON(w, 200, map[string]any{"errors": []gqlError{{errs.Error()}}})
		return
	}
	if len(doc.Operations) == 1 && req.OperationName == "" {
		req.OperationName = doc.Operations[0].Name
	}
	op := doc.Operations.ForName(req.OperationName)
	if op == nil {
		writeJSON(w, 200, map[string]any{"errors": []gqlError{{"unknown operation"}}})
		return
	}
	if _, isIntro := introspectionOnly(op); isIntro {
		graphqlIntrospection(w, r, req.Query, req.Variables)
		return
	}
	vars, gerr := validator.VariableValues(gqlSchema, op, req.Variables)
	if gerr != nil {
		writeJSON(w, 200, map[string]any{"errors": []gqlError{{gerr.Error()}}})
		return
	}
	data := map[string]any{}
	for _, sel := range op.SelectionSet {
		f, ok := sel.(*ast.Field)
		if !ok {
			continue
		}
		v, err := gqlRoot(r, op.Operation, f.Name, f.ArgumentMap(vars))
		if err != nil {
			writeJSON(w, 200, map[string]any{"data": nil, "errors": []gqlError{{err.Error()}}})
			return
		}
		data[f.Alias] = gqlProject(r, v, f.SelectionSet, vars)
	}
	writeJSON(w, 200, map[string]any{"data": data})
}

func introspectionOnly(op *ast.OperationDefinition) (string, bool) {
	for _, sel := range op.SelectionSet {
		if f, ok := sel.(*ast.Field); ok && f.Name == "__schema" {
			return f.Name, true
		}
	}
	return "", false
}

func intArg(args map[string]any, name string, def int) int {
	switch v := args[name].(type) {
	case int64:
		return int(v)
	case int:
		return v
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	}
	return def
}

func gqlRoot(r *http.Request, kind ast.Operation, name string, args map[string]any) (any, error) {
	switch {
	case kind == ast.Query && name == "users":
		first := min(max(intArg(args, "first", 20), 1), 1000)
		rows, err := db.Query(sq(r, "SELECT id, email, name, country FROM users ORDER BY id LIMIT ?"), first)
		if err != nil {
			return nil, err
		}
		return userRows(rows)
	case kind == ast.Query && name == "user":
		rows, err := db.Query(sq(r, "SELECT id, email, name, country FROM users WHERE id = ?"), fmt.Sprint(args["id"]))
		if err != nil {
			return nil, err
		}
		us, err := userRows(rows)
		if err != nil || len(us) == 0 {
			return nil, err
		}
		return us[0], nil
	case kind == ast.Query && name == "orders": // planted: unindexed status filter
		first := min(max(intArg(args, "first", 20), 1), 1000)
		status, _ := args["status"].(string)
		rows, err := db.Query(sq(r, "SELECT id, user_id, status, total_cents, created_at FROM orders WHERE status = ? ORDER BY id DESC LIMIT ?"), status, first)
		if err != nil {
			return nil, err
		}
		orders, err := scanOrders(rows)
		var out []any
		for _, o := range orders {
			out = append(out, map[string]any{"id": o.ID, "userId": o.UserID, "status": o.Status, "totalCents": o.TotalCents, "createdAt": o.CreatedAt})
		}
		return out, err
	case kind == ast.Mutation && name == "createUser":
		email, _ := args["email"].(string)
		nm, _ := args["name"].(string)
		country, _ := args["country"].(string)
		if len(country) > 2 {
			country = country[:2]
		}
		var id int64
		var err error
		if dialect == "postgres" {
			err = db.QueryRow(sq(r, "INSERT INTO users (email, name, country) VALUES (?, ?, ?) RETURNING id"), email, nm, country).Scan(&id)
		} else {
			var res sql.Result
			if res, err = db.Exec(sc(r, "INSERT INTO users (email, name, country) VALUES (?, ?, ?)"), email, nm, country); err == nil {
				id, _ = res.LastInsertId()
			}
		}
		if err != nil {
			return nil, err
		}
		return map[string]any{"id": id, "email": email, "name": nm, "country": country}, nil
	}
	return nil, fmt.Errorf("unknown field %s", name)
}

func userRows(rows *sql.Rows) ([]any, error) {
	defer rows.Close()
	var out []any
	for rows.Next() {
		var id int64
		var email string
		var name, country sql.NullString
		if err := rows.Scan(&id, &email, &name, &country); err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "email": email, "name": name.String, "country": country.String})
	}
	return out, rows.Err()
}

// gqlProject shapes a value by the selection set; User.notes runs one query
// per user (the classic GraphQL N+1, planted).
func gqlProject(r *http.Request, v any, sel ast.SelectionSet, vars map[string]any) any {
	switch x := v.(type) {
	case []any:
		out := make([]any, 0, len(x))
		for _, it := range x {
			out = append(out, gqlProject(r, it, sel, vars))
		}
		return out
	case map[string]any:
		if len(sel) == 0 {
			return x
		}
		out := map[string]any{}
		for _, s := range sel {
			f, ok := s.(*ast.Field)
			if !ok {
				continue
			}
			switch f.Name {
			case "__typename":
				out[f.Alias] = "Object"
			case "notes":
				first := min(max(intArg(f.ArgumentMap(vars), "first", 5), 1), 100)
				rows, err := db.Query(sq(r, "SELECT id, body FROM notes WHERE user_id = ? ORDER BY created_at DESC LIMIT ?"), x["id"], first)
				if err != nil {
					out[f.Alias] = nil
					continue
				}
				var notes []any
				for rows.Next() {
					var id, body string
					if rows.Scan(&id, &body) == nil {
						notes = append(notes, map[string]any{"id": id, "body": body})
					}
				}
				rows.Close()
				if notes == nil {
					notes = []any{}
				}
				out[f.Alias] = gqlProject(r, notes, f.SelectionSet, vars)
			default:
				out[f.Alias] = x[f.Name]
			}
		}
		return out
	}
	return v
}

// graphqlIntrospection answers routeperf's introspection query from the
// parsed schema (a test fixture, not a general introspection executor).
func graphqlIntrospection(w http.ResponseWriter, _ *http.Request, _ string, _ map[string]any) {
	var ref func(t *ast.Type) map[string]any
	ref = func(t *ast.Type) map[string]any {
		if t.NonNull {
			c := *t
			c.NonNull = false
			return map[string]any{"kind": "NON_NULL", "name": nil, "ofType": ref(&c)}
		}
		if t.Elem != nil {
			return map[string]any{"kind": "LIST", "name": nil, "ofType": ref(t.Elem)}
		}
		kind := "SCALAR"
		if d := gqlSchema.Types[t.NamedType]; d != nil {
			kind = map[ast.DefinitionKind]string{ast.Scalar: "SCALAR", ast.Object: "OBJECT", ast.Interface: "INTERFACE", ast.Union: "UNION", ast.Enum: "ENUM", ast.InputObject: "INPUT_OBJECT"}[d.Kind]
		}
		return map[string]any{"kind": kind, "name": t.NamedType, "ofType": nil}
	}
	var types []any
	for name, d := range gqlSchema.Types {
		kind := map[ast.DefinitionKind]string{ast.Scalar: "SCALAR", ast.Object: "OBJECT", ast.Interface: "INTERFACE", ast.Union: "UNION", ast.Enum: "ENUM", ast.InputObject: "INPUT_OBJECT"}[d.Kind]
		t := map[string]any{"kind": kind, "name": name, "fields": nil, "inputFields": nil, "enumValues": nil}
		var fields, inputs []any
		for _, f := range d.Fields {
			var args []any
			for _, a := range f.Arguments {
				var def any
				if a.DefaultValue != nil {
					def = a.DefaultValue.String()
				}
				args = append(args, map[string]any{"name": a.Name, "type": ref(a.Type), "defaultValue": def})
			}
			if d.Kind == ast.InputObject {
				inputs = append(inputs, map[string]any{"name": f.Name, "type": ref(f.Type), "defaultValue": nil})
			} else {
				fields = append(fields, map[string]any{"name": f.Name, "args": args, "type": ref(f.Type)})
			}
		}
		var enums []any
		for _, e := range d.EnumValues {
			enums = append(enums, map[string]any{"name": e.Name})
		}
		if d.Kind == ast.Object || d.Kind == ast.Interface {
			t["fields"] = fields
		}
		if d.Kind == ast.InputObject {
			t["inputFields"] = inputs
		}
		if d.Kind == ast.Enum {
			t["enumValues"] = enums
		}
		types = append(types, t)
	}
	sch := map[string]any{"queryType": map[string]any{"name": "Query"}, "mutationType": map[string]any{"name": "Mutation"}, "types": types}
	writeJSON(w, 200, map[string]any{"data": map[string]any{"__schema": sch}})
}
