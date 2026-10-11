package runner

import (
	"context"
	"fmt"
	"strings"

	"github.com/nazishasghar/routeperf/internal/spec"
)

// protocol is the API style: openapi (default), graphql, grpc, trpc,
// jsonrpc, connect or twirp.
func (r *Runner) protocol() string {
	c := r.cfg
	if c.API.Protocol != "" {
		p := strings.ToLower(c.API.Protocol)
		if p == "json-rpc" || p == "openrpc" {
			p = "jsonrpc"
		}
		return p
	}
	s := strings.ToLower(c.Spec)
	switch {
	case strings.HasPrefix(s, "trpc:") || isTSFile(strings.SplitN(s, "#", 2)[0]) || strings.HasSuffix(s, ".trpc.json"):
		return "trpc"
	case strings.HasPrefix(s, "jsonrpc:") || strings.HasPrefix(s, "openrpc:") || strings.HasSuffix(s, "openrpc.json"):
		return "jsonrpc"
	case strings.HasPrefix(s, "connect://") || strings.HasPrefix(s, "connects://") || strings.HasPrefix(s, "connect:"):
		return "connect"
	case strings.HasPrefix(s, "twirp:"):
		return "twirp"
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
	case "grpc", "connect", "twirp":
		return r.loadGRPC(ctx, add)
	case "trpc":
		return r.loadTRPC(ctx, add)
	case "jsonrpc":
		return r.loadJSONRPC(ctx, add)
	case "openapi":
	default:
		add("Spec", "fail", "unknown protocol "+r.protocol(), "use openapi, graphql, grpc, trpc, jsonrpc, connect or twirp")
		return false
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
