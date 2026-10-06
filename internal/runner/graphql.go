package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/nazishasghar/routeperf/internal/gql"
	"github.com/nazishasghar/routeperf/internal/inputs"
)

// gqlTransport posts each operation's document and variables to the endpoint.
type gqlTransport struct {
	r    *Runner
	path string
}

func (t *gqlTransport) Close() {}

func (t *gqlTransport) Send(ctx context.Context, req *inputs.Request) (int, []byte, http.Header, float64, error) {
	op := req.Op.GraphQL
	if op == nil {
		return t.r.sendHTTP(ctx, req)
	}
	hreq := &inputs.Request{Method: "POST", Path: t.path, Query: url.Values{}, Header: req.Header, ContentType: "application/json",
		Body: map[string]any{"query": op.Document, "variables": req.Vars, "operationName": op.Name}}
	status, body, hdr, ms, err := t.r.sendHTTP(ctx, hreq)
	if err != nil || status != 200 {
		return status, body, hdr, ms, err
	}
	var resp struct {
		Data   map[string]any `json:"data"`
		Errors []struct {
			Message    string         `json:"message"`
			Extensions map[string]any `json:"extensions"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &resp) == nil && len(resp.Errors) > 0 {
		code := 400 // GraphQL errors come back as 200: report them as failures
		if c, _ := resp.Errors[0].Extensions["code"].(string); c == "UNAUTHENTICATED" {
			code = 401
		} else if c == "FORBIDDEN" {
			code = 403
		} else if c == "INTERNAL_SERVER_ERROR" {
			code = 500
		}
		return code, body, hdr, ms, nil
	}
	return status, body, hdr, ms, nil
}

// graphqlEndpoint splits the configured endpoint into base URL and path.
func (r *Runner) graphqlEndpoint() (string, string) {
	c := r.cfg
	s := strings.TrimPrefix(c.Spec, "graphql:")
	if strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://") {
		if u, err := url.Parse(s); err == nil && strings.HasSuffix(strings.TrimRight(u.Path, "/"), "graphql") {
			path := u.Path
			u.Path, u.RawQuery = "", ""
			return u.String(), path
		}
	}
	return c.API.BaseURL, c.API.GraphQL
}

func (r *Runner) loadGraphQL(ctx context.Context, add addFn) bool {
	c := r.cfg
	base, path := r.graphqlEndpoint()
	if c.API.BaseURL == "" {
		c.API.BaseURL = base
	}
	src := strings.TrimPrefix(c.Spec, "graphql:")
	var sch *gql.Schema
	var err error
	if strings.HasPrefix(src, "http://") || strings.HasPrefix(src, "https://") {
		ep := strings.TrimRight(c.API.BaseURL, "/") + path
		if strings.HasSuffix(strings.TrimRight(src, "/"), "graphql") {
			ep = src
		}
		hreq := &inputs.Request{Method: "POST", Path: path, Query: url.Values{}, Header: map[string]string{}, ContentType: "application/json",
			Body: map[string]any{"query": gql.IntrospectionQuery}}
		var status int
		var body []byte
		status, body, _, _, err = r.sendHTTP(ctx, hreq)
		if err == nil && status != 200 {
			err = fmt.Errorf("introspection at %s: HTTP %d", ep, status)
		}
		if err == nil {
			sch, err = gql.FromIntrospection(body)
		}
	} else {
		var b []byte
		if b, err = os.ReadFile(src); err == nil {
			sch, err = gql.FromSDL(string(b))
		}
	}
	if err != nil {
		add("Spec", "fail", err.Error(), "point --spec at the GraphQL endpoint (introspection enabled) or a .graphql SDL file")
		return false
	}
	r.sp = sch.Operations("GraphQL API")
	r.tr = &gqlTransport{r: r, path: path}
	nq, nm := 0, 0
	for _, o := range r.sp.Ops {
		if o.Phase == "R" {
			nq++
		} else {
			nm++
		}
	}
	add("Spec", "ok", fmt.Sprintf("GraphQL schema — %d queries, %d mutations (endpoint %s)", nq, nm, path), "")
	if len(r.sp.Ops) == 0 {
		add("Spec operations", "fail", "schema has no query fields", "")
		return false
	}
	return true
}
