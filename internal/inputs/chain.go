package inputs

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/nazishasghar/routeperf/internal/auth"
	"github.com/nazishasghar/routeperf/internal/spec"
)

// Encode serializes the request body for its content type.
func Encode(req *Request) ([]byte, string, error) {
	if req.Body == nil {
		return nil, "", nil
	}
	switch req.ContentType {
	case "application/x-www-form-urlencoded":
		v := url.Values{}
		flatten(v, "", req.Body)
		return []byte(v.Encode()), req.ContentType, nil
	case "multipart/form-data":
		var buf bytes.Buffer
		w := multipart.NewWriter(&buf)
		m, _ := req.Body.(map[string]any)
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if err := writePart(w, k, m[k]); err != nil {
				return nil, "", err
			}
		}
		if err := w.Close(); err != nil {
			return nil, "", err
		}
		return buf.Bytes(), w.FormDataContentType(), nil
	}
	b, err := json.Marshal(req.Body)
	return b, "application/json", err
}

func writePart(w *multipart.Writer, name string, v any) error {
	switch x := v.(type) {
	case []byte:
		fw, err := w.CreateFormFile(name, name+".txt")
		if err != nil {
			return err
		}
		_, err = fw.Write(x)
		return err
	case []any:
		for _, it := range x {
			if err := writePart(w, name, it); err != nil {
				return err
			}
		}
		return nil
	case map[string]any:
		b, _ := json.Marshal(x)
		return w.WriteField(name, string(b))
	case nil:
		return nil
	}
	return w.WriteField(name, scalar(v))
}

// flatten encodes nested values as key[sub]=v / repeated keys for arrays.
func flatten(v url.Values, prefix string, x any) {
	switch t := x.(type) {
	case map[string]any:
		for k, vv := range t {
			key := k
			if prefix != "" {
				key = prefix + "[" + k + "]"
			}
			flatten(v, key, vv)
		}
	case []any:
		for _, vv := range t {
			flatten(v, prefix, vv)
		}
	case []byte:
		v.Add(prefix, string(t))
	case nil:
	default:
		v.Add(prefix, scalar(t))
	}
}

func scalar(v any) string {
	if f, ok := v.(float64); ok && f == float64(int64(f)) {
		return strconv.FormatInt(int64(f), 10)
	}
	return fmt.Sprint(v)
}

// ---------------------------------------------------------------- links

// HasLinks reports whether earlier responses fed parameters of op.
func (r *Resolver) HasLinks(op *spec.Operation) bool {
	return len(r.linked[op.ID]) > 0 || len(r.linkBody[op.ID]) > 0
}

// LinkSource returns the operation whose links target op, and the path
// parameter they fill.
func (r *Resolver) LinkSource(op *spec.Operation) (*spec.Operation, string) {
	if r.Spec == nil {
		return nil, ""
	}
	last := spec.LastParam(op.Path)
	for _, o := range r.Spec.Ops {
		for _, l := range o.Links {
			if l.Target != op.ID {
				continue
			}
			if _, ok := l.Params[last]; ok && last != "" {
				return o, last
			}
		}
	}
	return nil, ""
}

// Linked returns the values links produced for op's parameter.
func (r *Resolver) Linked(opID, param string) []any { return r.linked[opID][param] }

// TakeLinked removes and returns the values links produced for a parameter.
func (r *Resolver) TakeLinked(opID, param string) []any {
	v := r.linked[opID][param]
	if r.linked[opID] != nil {
		delete(r.linked[opID], param)
	}
	return v
}

func statusMatches(decl string, status int) bool {
	switch {
	case decl == "default":
		return status >= 200 && status < 300
	case len(decl) == 3 && strings.HasSuffix(strings.ToUpper(decl), "XX"):
		return strconv.Itoa(status)[:1] == decl[:1]
	}
	return decl == strconv.Itoa(status)
}

// Observe records values from a response: OpenAPI link targets and the next
// page cursor.
func (r *Resolver) Observe(op *spec.Operation, req *Request, status int, body []byte, hdr http.Header) {
	if status < 200 || status >= 300 {
		return
	}
	var parsed any
	_ = json.Unmarshal(body, &parsed)
	for _, l := range op.Links {
		if !statusMatches(l.Status, status) {
			continue
		}
		for name, expr := range l.Params {
			v, ok := evalExpr(expr, req, status, parsed, hdr)
			if !ok {
				continue
			}
			if r.linked[l.Target] == nil {
				r.linked[l.Target] = map[string][]any{}
			}
			if len(r.linked[l.Target][name]) < 500 {
				r.linked[l.Target][name] = append(r.linked[l.Target][name], v)
			}
		}
		if l.Body != nil {
			if v, ok := evalExpr(l.Body, req, status, parsed, hdr); ok && len(r.linkBody[l.Target]) < 500 {
				r.linkBody[l.Target] = append(r.linkBody[l.Target], v)
			}
		}
	}
	if CursorParam(op) != "" && req.Page >= 0 {
		if next := nextCursor(parsed, hdr, CursorParam(op)); next != "" && next != r.cursor[op.ID] {
			r.cursor[op.ID] = next
			r.page[op.ID]++
			if r.page[op.ID] > r.maxPage[op.ID] {
				r.maxPage[op.ID] = r.page[op.ID]
			}
		} else { // last page: start over
			delete(r.cursor, op.ID)
			r.page[op.ID] = 0
		}
	}
}

// Pages returns how many cursor pages were walked for op (0 = no cursor).
func (r *Resolver) Pages(op *spec.Operation) int {
	if r.maxPage[op.ID] == 0 {
		return 0
	}
	return r.maxPage[op.ID] + 1
}

var exprRe = regexp.MustCompile(`\{(\$[^}]+)\}`)

// evalExpr evaluates an OpenAPI runtime expression ($response.body#/id,
// $response.header.Location, $request.path.id, …), constants pass through.
func evalExpr(expr any, req *Request, status int, body any, hdr http.Header) (any, bool) {
	s, ok := expr.(string)
	if !ok {
		return expr, expr != nil
	}
	if !strings.HasPrefix(s, "$") {
		if !strings.Contains(s, "{$") {
			return s, true
		}
		ok := true
		out := exprRe.ReplaceAllStringFunc(s, func(m string) string {
			v, good := evalExpr(m[1:len(m)-1], req, status, body, hdr)
			ok = ok && good
			return fmt.Sprint(v)
		})
		return out, ok
	}
	switch {
	case s == "$statusCode":
		return status, true
	case s == "$method":
		return req.Method, true
	case s == "$url":
		return req.Path, true
	case strings.HasPrefix(s, "$response.body"):
		return pointer(body, strings.TrimPrefix(strings.TrimPrefix(s, "$response.body"), "#"))
	case strings.HasPrefix(s, "$response.header."):
		v := hdr.Get(strings.TrimPrefix(s, "$response.header."))
		return v, v != ""
	case strings.HasPrefix(s, "$request.path."):
		v, ok := req.PathParams[strings.TrimPrefix(s, "$request.path.")]
		return v, ok
	case strings.HasPrefix(s, "$request.query."):
		v := req.Query.Get(strings.TrimPrefix(s, "$request.query."))
		return v, v != ""
	case strings.HasPrefix(s, "$request.header."):
		v, ok := req.Header[strings.TrimPrefix(s, "$request.header.")]
		return v, ok
	case strings.HasPrefix(s, "$request.body"):
		return pointer(req.Body, strings.TrimPrefix(strings.TrimPrefix(s, "$request.body"), "#"))
	}
	return nil, false
}

// pointer resolves an RFC 6901 JSON pointer.
func pointer(v any, p string) (any, bool) {
	if p == "" || p == "/" {
		return v, v != nil
	}
	cur := v
	for _, tok := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		tok = strings.NewReplacer("~1", "/", "~0", "~").Replace(tok)
		switch x := cur.(type) {
		case map[string]any:
			nv, ok := x[tok]
			if !ok {
				return nil, false
			}
			cur = nv
		case []any:
			i, err := strconv.Atoi(tok)
			if err != nil || i < 0 || i >= len(x) {
				return nil, false
			}
			cur = x[i]
		default:
			return nil, false
		}
	}
	if f, ok := cur.(float64); ok && f == float64(int64(f)) {
		return int64(f), true
	}
	return cur, cur != nil
}

// ---------------------------------------------------------------- cursors

var cursorNames = []string{"cursor", "after", "page_token", "pageToken", "next", "next_token", "nextToken", "starting_after",
	"startingAfter", "continuation", "continuation_token", "continuationToken", "page_cursor", "pageCursor", "next_cursor", "nextCursor"}

// CursorParam returns the query parameter that carries a pagination cursor.
func CursorParam(op *spec.Operation) string {
	for _, p := range op.Params {
		if p.In != "query" {
			continue
		}
		for _, c := range cursorNames {
			if strings.EqualFold(p.Name, c) {
				return p.Name
			}
		}
	}
	return ""
}

var linkNext = regexp.MustCompile(`<([^>]+)>\s*;\s*rel="?next"?`)

// nextCursor finds the next-page cursor in a response body or Link header.
func nextCursor(body any, hdr http.Header, param string) string {
	for _, path := range []string{"next_cursor", "nextCursor", "cursor.next", "meta.next_cursor", "meta.nextCursor", "pagination.next_cursor",
		"pagination.nextCursor", "page_info.end_cursor", "pageInfo.endCursor", "next_page_token", "nextPageToken", "meta.next", "pagination.next",
		"links.next", "next", "data.next_cursor"} {
		v, ok := auth.JSONPath(body, path)
		if !ok || v == nil {
			continue
		}
		s, isStr := v.(string)
		if !isStr || s == "" {
			if f, isNum := v.(float64); isNum {
				return strconv.FormatInt(int64(f), 10)
			}
			continue
		}
		if strings.Contains(s, "://") || strings.HasPrefix(s, "/") {
			if u, err := url.Parse(s); err == nil {
				if c := u.Query().Get(param); c != "" {
					return c
				}
			}
			continue
		}
		if path == "pageInfo.endCursor" || path == "page_info.end_cursor" {
			if hn, _ := auth.JSONPath(body, strings.SplitN(path, ".", 2)[0]+".hasNextPage"); hn == false {
				return ""
			}
		}
		return s
	}
	if m := linkNext.FindStringSubmatch(hdr.Get("Link")); m != nil {
		if u, err := url.Parse(m[1]); err == nil {
			return u.Query().Get(param)
		}
	}
	return ""
}
