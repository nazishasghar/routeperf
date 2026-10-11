// Package rpcspec turns procedure-style API descriptions into routeperf
// operations: tRPC routers, JSON-RPC services (OpenRPC) and the names that
// map procedures to tables. Connect and Twirp reuse grpcspec.
package rpcspec

import (
	"net/url"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"
)

// Schema converts a JSON Schema (draft 4 to 2020-12, as emitted by zod,
// valibot or an OpenRPC document) to the OpenAPI schema routeperf's input
// generator fills. $refs into root are inlined; recursion stops at depth 8.
func Schema(v, root any) *openapi3.Schema { return conv(v, root, 0) }

func conv(v, root any, depth int) *openapi3.Schema {
	m, ok := v.(map[string]any)
	if !ok || depth > 8 { // true, {} or too deep: anything
		return &openapi3.Schema{}
	}
	if ref, ok := m["$ref"].(string); ok {
		if t := resolve(root, ref); t != nil {
			return conv(t, root, depth+1)
		}
		return &openapi3.Schema{}
	}
	s := &openapi3.Schema{}
	for _, t := range typesOf(m["type"]) {
		if t == "null" {
			s.Nullable = true
		} else if s.Type == nil {
			s.Type = &openapi3.Types{t}
		}
	}
	s.Format, _ = m["format"].(string)
	s.Description, _ = m["description"].(string)
	if e, ok := m["enum"].([]any); ok {
		for _, x := range e {
			if x != nil {
				s.Enum = append(s.Enum, x)
			}
		}
	}
	if c, ok := m["const"]; ok && c != nil {
		s.Enum = []any{c}
	}
	s.Default = m["default"]
	if ex, ok := m["examples"].([]any); ok && len(ex) > 0 {
		s.Example = ex[0]
	} else if ex, ok := m["example"]; ok {
		s.Example = ex
	}
	s.Min, s.Max = num(m["minimum"]), num(m["maximum"])
	// exclusive bounds: a number (2019+) or a flag on minimum (draft 4)
	if x := num(m["exclusiveMinimum"]); x != nil {
		s.Min = ptr(*x + 1)
	} else if b, _ := m["exclusiveMinimum"].(bool); b && s.Min != nil {
		s.Min = ptr(*s.Min + 1)
	}
	if x := num(m["exclusiveMaximum"]); x != nil {
		s.Max = ptr(*x - 1)
	} else if b, _ := m["exclusiveMaximum"].(bool); b && s.Max != nil {
		s.Max = ptr(*s.Max - 1)
	}
	if x := num(m["minLength"]); x != nil {
		s.MinLength = uint64(*x)
	}
	if x := num(m["maxLength"]); x != nil {
		s.MaxLength = uptr(*x)
	}
	if x := num(m["minItems"]); x != nil {
		s.MinItems = uint64(*x)
	}
	if x := num(m["maxItems"]); x != nil {
		s.MaxItems = uptr(*x)
	}
	if props, ok := m["properties"].(map[string]any); ok {
		s.Properties = openapi3.Schemas{}
		for k, pv := range props {
			s.Properties[k] = openapi3.NewSchemaRef("", conv(pv, root, depth+1))
		}
		if s.Type == nil {
			s.Type = &openapi3.Types{"object"}
		}
	}
	for _, r := range asList(m["required"]) {
		if n, ok := r.(string); ok {
			s.Required = append(s.Required, n)
		}
	}
	switch it := m["items"].(type) {
	case map[string]any:
		s.Items = openapi3.NewSchemaRef("", conv(it, root, depth+1))
	case []any: // draft 4 tuple
		if len(it) > 0 {
			s.Items = openapi3.NewSchemaRef("", conv(it[0], root, depth+1))
		}
	}
	if pi := asList(m["prefixItems"]); s.Items == nil && len(pi) > 0 {
		s.Items = openapi3.NewSchemaRef("", conv(pi[0], root, depth+1))
	}
	if s.Items != nil && s.Type == nil {
		s.Type = &openapi3.Types{"array"}
	}
	for _, all := range asList(m["allOf"]) {
		s.AllOf = append(s.AllOf, openapi3.NewSchemaRef("", conv(all, root, depth+1)))
	}
	// anyOf/oneOf: drop the null branch (nullable); one branch left is the type
	for _, key := range []string{"anyOf", "oneOf"} {
		var alts []any
		for _, a := range asList(m[key]) {
			if am, ok := a.(map[string]any); ok && len(am) == 1 && am["type"] == "null" {
				s.Nullable = true
				continue
			}
			alts = append(alts, a)
		}
		if len(alts) == 1 && s.Type == nil && len(s.Properties) == 0 {
			inner := conv(alts[0], root, depth+1)
			inner.Nullable = inner.Nullable || s.Nullable
			if inner.Default == nil {
				inner.Default = s.Default
			}
			if inner.Example == nil {
				inner.Example = s.Example
			}
			return inner
		}
		for _, a := range alts {
			ref := openapi3.NewSchemaRef("", conv(a, root, depth+1))
			if key == "anyOf" {
				s.AnyOf = append(s.AnyOf, ref)
			} else {
				s.OneOf = append(s.OneOf, ref)
			}
		}
	}
	return s
}

// resolve follows a local JSON pointer ref (#/components/schemas/User).
func resolve(root any, ref string) any {
	frag, ok := strings.CutPrefix(ref, "#")
	if !ok {
		return nil
	}
	cur := root
	for _, p := range strings.Split(strings.TrimPrefix(frag, "/"), "/") {
		if p == "" {
			continue
		}
		if u, err := url.PathUnescape(p); err == nil {
			p = u
		}
		p = strings.NewReplacer("~1", "/", "~0", "~").Replace(p)
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		if cur, ok = m[p]; !ok {
			return nil
		}
	}
	return cur
}

func typesOf(v any) []string {
	switch t := v.(type) {
	case string:
		return []string{t}
	case []any:
		var out []string
		for _, x := range t {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func asList(v any) []any {
	l, _ := v.([]any)
	return l
}

func num(v any) *float64 {
	if f, ok := v.(float64); ok {
		return &f
	}
	return nil
}

func ptr(f float64) *float64 { return &f }

func uptr(f float64) *uint64 {
	u := uint64(f)
	return &u
}
