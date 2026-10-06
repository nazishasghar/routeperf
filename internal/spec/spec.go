// Package spec loads Swagger 2.0 / OpenAPI 3.x documents into a flat
// operation catalog.
package spec

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi2"
	"github.com/getkin/kin-openapi/openapi2conv"
	"github.com/getkin/kin-openapi/openapi3"
	"gopkg.in/yaml.v3"
)

type Param struct {
	Name     string
	In       string
	Required bool
	Schema   *openapi3.Schema
	Example  any
}

type Body struct {
	ContentType string
	Schema      *openapi3.Schema
	Example     any
}

type Operation struct {
	ID          string
	Method      string
	Path        string
	Summary     string
	Tags        []string
	Params      []Param
	Body        *Body
	Security    *openapi3.SecurityRequirements // nil → inherit global
	Resource    string
	Depth       int
	Phase       string // R or W
	Links       []Link // OpenAPI response links to other operations
	Protocol    string // "" / http | graphql | grpc
	Unsupported string // reason the operation can't be called
	GraphQL     *GraphQLOp
	GRPC        *GRPCOp
}

// Link is an OpenAPI 3 response link: values from this operation's response
// (or request) feed parameters of another operation.
type Link struct {
	Status string         // response code the link is declared on ("201", "2XX", "default")
	Target string         // operationId
	Params map[string]any // parameter name → runtime expression or constant
	Body   any            // requestBody expression or constant
}

type Spec struct {
	Doc     *openapi3.T
	Ops     []*Operation
	Schemes map[string]*openapi3.SecurityScheme
	Global  openapi3.SecurityRequirements
	Title   string
}

var paramSeg = regexp.MustCompile(`^\{.*\}$`)

func Load(ctx context.Context, data []byte) (*Spec, error) {
	var probe map[string]any
	if err := yaml.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("spec is not JSON/YAML: %w", err)
	}
	var doc *openapi3.T
	loader := openapi3.NewLoader()
	loader.IsExternalRefsAllowed = true
	loader.Context = ctx
	if v, ok := probe["swagger"]; ok && fmt.Sprint(v) == "2.0" {
		js, err := json.Marshal(probe)
		if err != nil {
			return nil, err
		}
		var d2 openapi2.T
		if err := json.Unmarshal(js, &d2); err != nil {
			return nil, fmt.Errorf("swagger 2.0: %w", err)
		}
		if doc, err = openapi2conv.ToV3(&d2); err != nil {
			return nil, fmt.Errorf("convert swagger 2.0: %w", err)
		}
		if err := loader.ResolveRefsIn(doc, nil); err != nil {
			return nil, fmt.Errorf("resolve refs: %w", err)
		}
	} else {
		var err error
		if doc, err = loader.LoadFromData(data); err != nil {
			return nil, fmt.Errorf("openapi: %w", err)
		}
	}
	s := &Spec{Doc: doc, Schemes: map[string]*openapi3.SecurityScheme{}, Global: doc.Security}
	if doc.Info != nil {
		s.Title = doc.Info.Title
	}
	if doc.Components != nil {
		for name, ref := range doc.Components.SecuritySchemes {
			if ref != nil && ref.Value != nil {
				s.Schemes[name] = ref.Value
			}
		}
	}
	if doc.Paths == nil {
		return s, nil
	}
	for path, item := range doc.Paths.Map() {
		for method, op := range item.Operations() {
			o := &Operation{Method: strings.ToUpper(method), Path: path, Summary: op.Summary, Tags: op.Tags, Security: op.Security}
			o.ID = op.OperationID
			if o.ID == "" {
				o.ID = strings.ToLower(o.Method) + regexp.MustCompile(`[^A-Za-z0-9]+`).ReplaceAllString(path, "_")
			}
			seen := map[string]bool{}
			addParams := func(ps openapi3.Parameters) {
				for _, pr := range ps {
					if pr == nil || pr.Value == nil {
						continue
					}
					p := pr.Value
					key := p.In + ":" + p.Name
					if seen[key] {
						continue
					}
					seen[key] = true
					var sc *openapi3.Schema
					if p.Schema != nil {
						sc = p.Schema.Value
					}
					ex := p.Example
					if ex == nil {
						for _, e := range p.Examples {
							if e != nil && e.Value != nil {
								ex = e.Value.Value
								break
							}
						}
					}
					o.Params = append(o.Params, Param{Name: p.Name, In: p.In, Required: p.Required || p.In == "path", Schema: sc, Example: ex})
				}
			}
			addParams(op.Parameters)
			addParams(item.Parameters)
			if op.RequestBody != nil && op.RequestBody.Value != nil {
				o.Body = pickBody(op.RequestBody.Value.Content)
				if o.Body == nil && len(op.RequestBody.Value.Content) > 0 {
					for ct := range op.RequestBody.Value.Content {
						o.Unsupported = "request body type " + ct + " is not supported"
						break
					}
				}
			}
			if op.Responses != nil {
				codes := make([]string, 0, op.Responses.Len())
				for code := range op.Responses.Map() {
					codes = append(codes, code)
				}
				sort.Strings(codes)
				for _, code := range codes {
					rr := op.Responses.Value(code)
					if rr == nil || rr.Value == nil {
						continue
					}
					names := make([]string, 0, len(rr.Value.Links))
					for n := range rr.Value.Links {
						names = append(names, n)
					}
					sort.Strings(names)
					for _, n := range names {
						l := rr.Value.Links[n]
						if l == nil || l.Value == nil {
							continue
						}
						target := l.Value.OperationID
						if target == "" && l.Value.OperationRef != "" {
							target = "ref:" + l.Value.OperationRef
						}
						if target == "" {
							continue
						}
						o.Links = append(o.Links, Link{Status: code, Target: target, Params: l.Value.Parameters, Body: l.Value.RequestBody})
					}
				}
			}
			var segs []string
			for _, sg := range strings.Split(strings.Trim(path, "/"), "/") {
				if sg != "" && !paramSeg.MatchString(sg) {
					segs = append(segs, sg)
				}
			}
			o.Resource, o.Depth = strings.Join(segs, "/"), len(segs)
			o.Phase = "W"
			if o.Method == "GET" || o.Method == "HEAD" || o.Method == "OPTIONS" {
				o.Phase = "R"
			}
			s.Ops = append(s.Ops, o)
		}
	}
	sort.Slice(s.Ops, func(i, j int) bool { return s.Ops[i].Path+s.Ops[i].Method < s.Ops[j].Path+s.Ops[j].Method })
	s.ResolveLinks()
	return s, nil
}

// pickBody chooses the request body encoding: JSON first, then form
// encodings; nil when none is supported.
func pickBody(content openapi3.Content) *Body {
	for _, ct := range []string{"application/json", "application/*+json", "application/x-www-form-urlencoded", "multipart/form-data", "*/*"} {
		mt := content.Get(ct)
		if mt == nil {
			continue
		}
		real := ct
		if ct == "*/*" || strings.Contains(ct, "json") {
			real = "application/json"
		}
		b := &Body{ContentType: real, Example: mt.Example}
		if mt.Schema != nil {
			b.Schema = mt.Schema.Value
		}
		if b.Example == nil {
			for _, e := range mt.Examples {
				if e != nil && e.Value != nil {
					b.Example = e.Value.Value
					break
				}
			}
		}
		return b
	}
	for ct, mt := range content { // vendor JSON types
		if strings.Contains(ct, "json") {
			b := &Body{ContentType: "application/json", Example: mt.Example}
			if mt.Schema != nil {
				b.Schema = mt.Schema.Value
			}
			return b
		}
	}
	return nil
}

// ResolveLinks maps operationRef link targets (#/paths/~1users~1{id}/get) to
// operation IDs.
func (s *Spec) ResolveLinks() {
	for _, o := range s.Ops {
		for i, l := range o.Links {
			ref, ok := strings.CutPrefix(l.Target, "ref:")
			if !ok {
				continue
			}
			_, frag, _ := strings.Cut(ref, "#")
			parts := strings.Split(strings.TrimPrefix(frag, "/"), "/")
			if len(parts) != 3 || parts[0] != "paths" {
				continue
			}
			path := strings.NewReplacer("~1", "/", "~0", "~").Replace(parts[1])
			for _, t := range s.Ops {
				if t.Path == path && strings.EqualFold(t.Method, parts[2]) {
					o.Links[i].Target = t.ID
				}
			}
		}
	}
}

// ByID returns the operation with the given ID.
func (s *Spec) ByID(id string) *Operation {
	for _, o := range s.Ops {
		if o.ID == id {
			return o
		}
	}
	return nil
}

// OrderWrites returns write ops in lifecycle order: POST (parents first),
// then PUT/PATCH, then DELETE (children first).
func OrderWrites(ops []*Operation) []*Operation {
	rank := map[string]int{"POST": 0, "PUT": 1, "PATCH": 1, "DELETE": 2}
	w := append([]*Operation(nil), ops...)
	sort.SliceStable(w, func(i, j int) bool {
		a, b := w[i], w[j]
		if rank[a.Method] != rank[b.Method] {
			return rank[a.Method] < rank[b.Method]
		}
		if a.Method == "DELETE" {
			return a.Depth > b.Depth
		}
		return a.Depth < b.Depth
	})
	return w
}

// CollectionPath strips a trailing path parameter: /users/{id} → /users.
func CollectionPath(p string) string {
	segs := strings.Split(strings.TrimRight(p, "/"), "/")
	if len(segs) > 1 && paramSeg.MatchString(segs[len(segs)-1]) {
		return strings.Join(segs[:len(segs)-1], "/")
	}
	return p
}

// LastParam returns the trailing path parameter name ("" if none).
func LastParam(p string) string {
	segs := strings.Split(strings.TrimRight(p, "/"), "/")
	if last := segs[len(segs)-1]; paramSeg.MatchString(last) {
		return strings.Trim(last, "{}")
	}
	return ""
}

var dangerRe = regexp.MustCompile(`(?i)reset|purge|truncate|drop|wipe|destroy|nuke|flush`)

// Dangerous flags collection-level deletes and destructive-looking paths.
func Dangerous(o *Operation) bool {
	if dangerRe.MatchString(o.Path) && o.Method != "GET" {
		return true
	}
	return o.Method == "DELETE" && LastParam(o.Path) == ""
}

// GraphQLOp describes a GraphQL root field call.
type GraphQLOp struct {
	Kind     string // query | mutation
	Field    string
	Document string // query text with $variables
	Name     string // operationName
}

// GRPCOp describes a unary gRPC method.
type GRPCOp struct {
	FullMethod string // /pkg.Service/Method
	Input      any    // protoreflect.MessageDescriptor (kept untyped to avoid importing protobuf here)
	Output     any
}
