// Package gql turns a GraphQL schema (introspection or SDL) into routeperf
// operations: one per Query field (read) and Mutation field (write), each
// with a generated document that selects scalars two levels deep so nested
// resolvers (the usual source of GraphQL N+1s) run.
package gql

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/vektah/gqlparser/v2"
	"github.com/vektah/gqlparser/v2/ast"

	"github.com/nazishasghar/routeperf/internal/spec"
)

type TypeRef struct {
	Kind   string   `json:"kind"`
	Name   string   `json:"name"`
	OfType *TypeRef `json:"ofType"`
}

type InputValue struct {
	Name         string  `json:"name"`
	Type         TypeRef `json:"type"`
	DefaultValue *string `json:"defaultValue"`
}

type Field struct {
	Name string       `json:"name"`
	Args []InputValue `json:"args"`
	Type TypeRef      `json:"type"`
}

type FullType struct {
	Kind        string       `json:"kind"`
	Name        string       `json:"name"`
	Fields      []Field      `json:"fields"`
	InputFields []InputValue `json:"inputFields"`
	EnumValues  []struct {
		Name string `json:"name"`
	} `json:"enumValues"`
}

type Schema struct {
	Query, Mutation string
	Types           map[string]*FullType
}

// IntrospectionQuery is the standard query (type refs 7 levels deep).
const IntrospectionQuery = `query IntrospectionQuery { __schema { queryType { name } mutationType { name }
  types { kind name fields(includeDeprecated: true) { name args { name type { ...T } defaultValue } type { ...T } }
    inputFields { name type { ...T } defaultValue } enumValues(includeDeprecated: true) { name } } } }
fragment T on __Type { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name ofType { kind name } } } } } } }`

// FromIntrospection parses an introspection response ({"data":{"__schema":…}}).
func FromIntrospection(b []byte) (*Schema, error) {
	var resp struct {
		Data struct {
			Schema struct {
				QueryType    *struct{ Name string } `json:"queryType"`
				MutationType *struct{ Name string } `json:"mutationType"`
				Types        []*FullType            `json:"types"`
			} `json:"__schema"`
		} `json:"data"`
		Errors []struct{ Message string } `json:"errors"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, fmt.Errorf("introspection response: %w", err)
	}
	if len(resp.Errors) > 0 {
		return nil, fmt.Errorf("introspection: %s", resp.Errors[0].Message)
	}
	s := &Schema{Types: map[string]*FullType{}}
	if q := resp.Data.Schema.QueryType; q != nil {
		s.Query = q.Name
	}
	if m := resp.Data.Schema.MutationType; m != nil {
		s.Mutation = m.Name
	}
	for _, t := range resp.Data.Schema.Types {
		s.Types[t.Name] = t
	}
	if s.Query == "" {
		return nil, fmt.Errorf("introspection returned no query type (is introspection disabled?)")
	}
	return s, nil
}

func refFromAST(t *ast.Type) TypeRef {
	if t == nil {
		return TypeRef{}
	}
	if t.NonNull {
		c := *t
		c.NonNull = false
		in := refFromAST(&c)
		return TypeRef{Kind: "NON_NULL", OfType: &in}
	}
	if t.Elem != nil {
		in := refFromAST(t.Elem)
		return TypeRef{Kind: "LIST", OfType: &in}
	}
	return TypeRef{Kind: "NAMED", Name: t.NamedType}
}

// FromSDL parses schema definition language.
func FromSDL(sdl string) (*Schema, error) {
	sch, err := gqlparser.LoadSchema(&ast.Source{Name: "schema.graphql", Input: sdl})
	if err != nil {
		return nil, err
	}
	s := &Schema{Types: map[string]*FullType{}}
	if sch.Query != nil {
		s.Query = sch.Query.Name
	}
	if sch.Mutation != nil {
		s.Mutation = sch.Mutation.Name
	}
	kinds := map[ast.DefinitionKind]string{ast.Scalar: "SCALAR", ast.Object: "OBJECT", ast.Interface: "INTERFACE", ast.Union: "UNION", ast.Enum: "ENUM", ast.InputObject: "INPUT_OBJECT"}
	for name, d := range sch.Types {
		ft := &FullType{Kind: kinds[d.Kind], Name: name}
		for _, f := range d.Fields {
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			fd := Field{Name: f.Name, Type: refFromAST(f.Type)}
			for _, a := range f.Arguments {
				iv := InputValue{Name: a.Name, Type: refFromAST(a.Type)}
				if a.DefaultValue != nil {
					v := a.DefaultValue.String()
					iv.DefaultValue = &v
				}
				fd.Args = append(fd.Args, iv)
			}
			if d.Kind == ast.InputObject {
				iv := InputValue{Name: f.Name, Type: refFromAST(f.Type)}
				if f.DefaultValue != nil {
					v := f.DefaultValue.String()
					iv.DefaultValue = &v
				}
				ft.InputFields = append(ft.InputFields, iv)
			} else {
				ft.Fields = append(ft.Fields, fd)
			}
		}
		for _, e := range d.EnumValues {
			ft.EnumValues = append(ft.EnumValues, struct {
				Name string `json:"name"`
			}{e.Name})
		}
		s.Types[name] = ft
	}
	return s, nil
}

func named(t TypeRef) string {
	for t.OfType != nil {
		t = *t.OfType
	}
	return t.Name
}

func (s *Schema) kind(t TypeRef) string {
	if ft := s.Types[named(t)]; ft != nil {
		return ft.Kind
	}
	switch named(t) {
	case "Int", "Float", "String", "Boolean", "ID":
		return "SCALAR"
	}
	return "SCALAR"
}

// typeString renders a type reference in GraphQL syntax (Int!, [ID!]).
func typeString(t TypeRef) string {
	switch t.Kind {
	case "NON_NULL":
		return typeString(*t.OfType) + "!"
	case "LIST":
		return "[" + typeString(*t.OfType) + "]"
	}
	return t.Name
}

func required(args []InputValue) bool {
	for _, a := range args {
		if a.Type.Kind == "NON_NULL" && a.DefaultValue == nil {
			return true
		}
	}
	return false
}

// selection selects scalar fields, and object fields `depth` levels down.
func (s *Schema) selection(t TypeRef, depth int, seen map[string]bool) string {
	ft := s.Types[named(t)]
	if ft == nil {
		return ""
	}
	switch ft.Kind {
	case "SCALAR", "ENUM":
		return ""
	case "UNION":
		return "{ __typename }"
	}
	if seen[ft.Name] {
		return ""
	}
	seen = copySet(seen, ft.Name)
	var parts []string
	nested := 0
	for _, f := range ft.Fields {
		if required(f.Args) || strings.HasPrefix(f.Name, "__") {
			continue
		}
		switch s.kind(f.Type) {
		case "SCALAR", "ENUM":
			if len(parts) < 25 {
				parts = append(parts, f.Name)
			}
		default:
			if depth > 0 && nested < 5 {
				if sub := s.selection(f.Type, depth-1, seen); sub != "" {
					parts = append(parts, f.Name+" "+sub)
					nested++
				}
			}
		}
	}
	if len(parts) == 0 {
		parts = []string{"__typename"}
	}
	return "{ " + strings.Join(parts, " ") + " }"
}

func copySet(m map[string]bool, add string) map[string]bool {
	out := map[string]bool{add: true}
	for k := range m {
		out[k] = true
	}
	return out
}

// oapi converts a GraphQL input type to an OpenAPI schema so routeperf's
// input generator can fill it.
func (s *Schema) oapi(t TypeRef, depth int) *openapi3.Schema {
	switch t.Kind {
	case "NON_NULL":
		return s.oapi(*t.OfType, depth)
	case "LIST":
		return &openapi3.Schema{Type: &openapi3.Types{"array"}, Items: openapi3.NewSchemaRef("", s.oapi(*t.OfType, depth+1))}
	}
	switch t.Name {
	case "Int":
		return openapi3.NewIntegerSchema()
	case "Float":
		return openapi3.NewFloat64Schema()
	case "Boolean":
		return openapi3.NewBoolSchema()
	case "String", "ID":
		return openapi3.NewStringSchema()
	}
	ft := s.Types[t.Name]
	if ft == nil {
		return openapi3.NewStringSchema()
	}
	switch ft.Kind {
	case "ENUM":
		sc := openapi3.NewStringSchema()
		for _, e := range ft.EnumValues {
			sc.Enum = append(sc.Enum, e.Name)
		}
		return sc
	case "INPUT_OBJECT":
		sc := openapi3.NewObjectSchema()
		if depth > 4 {
			return sc
		}
		for _, f := range ft.InputFields {
			sc.Properties[f.Name] = openapi3.NewSchemaRef("", s.oapi(f.Type, depth+1))
			if f.Type.Kind == "NON_NULL" && f.DefaultValue == nil {
				sc.Required = append(sc.Required, f.Name)
			}
		}
		return sc
	}
	return openapi3.NewStringSchema() // custom scalars
}

// Operations builds one routeperf operation per root field.
func (s *Schema) Operations(title string) *spec.Spec {
	sp := &spec.Spec{Title: title}
	add := func(root, kind, phase string) {
		ft := s.Types[root]
		if ft == nil {
			return
		}
		fields := append([]Field(nil), ft.Fields...)
		sort.Slice(fields, func(i, j int) bool { return fields[i].Name < fields[j].Name })
		for _, f := range fields {
			if strings.HasPrefix(f.Name, "__") {
				continue
			}
			o := &spec.Operation{ID: f.Name, Method: strings.ToUpper(kind), Path: f.Name, Phase: phase, Protocol: "graphql", Resource: f.Name, Depth: 1}
			if kind == "mutation" {
				o.ID = "mutation_" + f.Name
			}
			var decl, call []string
			for _, a := range f.Args {
				decl = append(decl, "$"+a.Name+": "+typeString(a.Type))
				call = append(call, a.Name+": $"+a.Name)
				p := spec.Param{Name: a.Name, In: "arg", Required: a.Type.Kind == "NON_NULL" && a.DefaultValue == nil, Schema: s.oapi(a.Type, 0)}
				o.Params = append(o.Params, p)
			}
			doc := kind + " rp_" + f.Name
			if len(decl) > 0 {
				doc += "(" + strings.Join(decl, ", ") + ")"
			}
			doc += " { " + f.Name
			if len(call) > 0 {
				doc += "(" + strings.Join(call, ", ") + ")"
			}
			if sel := s.selection(f.Type, 1, nil); sel != "" {
				doc += " " + sel
			}
			doc += " }"
			o.GraphQL = &spec.GraphQLOp{Kind: kind, Field: f.Name, Document: doc, Name: "rp_" + f.Name}
			sp.Ops = append(sp.Ops, o)
		}
	}
	add(s.Query, "query", "R")
	if s.Mutation != "" {
		add(s.Mutation, "mutation", "W")
	}
	return sp
}
