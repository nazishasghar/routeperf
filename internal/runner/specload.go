package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/nazishasghar/routeperf/internal/spec"
)

// protocol is the API style: openapi (default), graphql or grpc.
func (r *Runner) protocol() string {
	c := r.cfg
	if c.API.Protocol != "" {
		return strings.ToLower(c.API.Protocol)
	}
	s := strings.ToLower(c.Spec)
	switch {
	case strings.HasPrefix(s, "grpc://") || strings.HasPrefix(s, "grpcs://") || strings.HasSuffix(s, ".proto"):
		return "grpc"
	case strings.HasPrefix(s, "graphql:") || strings.HasSuffix(s, ".graphql") || strings.HasSuffix(s, ".graphqls") ||
		strings.HasSuffix(s, ".gql") || strings.HasSuffix(strings.TrimRight(s, "/"), "/graphql"):
		return "graphql"
	}
	return "openapi"
}

type addFn func(name, status, detail, fix string)

// loadSpec loads the API description for the configured protocol.
func (r *Runner) loadSpec(ctx context.Context, add addFn) bool {
	switch r.protocol() {
	case "graphql":
		return r.loadGraphQL(ctx, add)
	case "grpc":
		return r.loadGRPC(ctx, add)
	}
	c := r.cfg
	data, err := r.fetch(ctx, c.Spec)
	if err != nil {
		fix := "check the URL/file path; if the spec needs auth, configure auth first"
		if strings.Contains(err.Error(), "401") || strings.Contains(err.Error(), "403") {
			fix = "the spec endpoint rejected the credentials; check token/cookies/headers"
		}
		add("Spec", "fail", err.Error(), fix)
		return false
	}
	if r.sp, err = spec.Load(ctx, data); err != nil {
		add("Spec", "fail", err.Error(), "the document must be valid Swagger 2.0 or OpenAPI 3.x (JSON or YAML)")
		return false
	}
	nr, nw, links := 0, 0, 0
	for _, o := range r.sp.Ops {
		if o.Phase == "R" {
			nr++
		} else {
			nw++
		}
		links += len(o.Links)
	}
	detail := fmt.Sprintf("%q OpenAPI %s — %d operations (%d read, %d write)", r.sp.Title, r.sp.Doc.OpenAPI, len(r.sp.Ops), nr, nw)
	if links > 0 {
		detail += fmt.Sprintf(", %d response links", links)
	}
	add("Spec", "ok", detail, "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "spec has no operations", "")
		return false
	}
	return true
}
