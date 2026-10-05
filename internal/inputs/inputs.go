// Package inputs builds concrete requests for operations: fixtures, spec
// examples, DB-sampled anchor IDs and schema-driven generated values.
package inputs

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/nazishasghar/routeperf/internal/db"
	"github.com/nazishasghar/routeperf/internal/spec"
)

type Fixture struct {
	Path   map[string]any `yaml:"path"`
	Query  map[string]any `yaml:"query"`
	Header map[string]any `yaml:"header"`
	Body   any            `yaml:"body"`
}

type Request struct {
	Method string
	Path   string // with params substituted
	Query  url.Values
	Header map[string]string
	Body   any
}

type KInfo struct {
	Name     string // query param name, or body array property
	InBody   bool
	Min, Max int
}

type Resolver struct {
	DB         db.DB
	Tables     map[string]*db.Table
	Fixtures   map[string]Fixture
	KParams    []string
	RunID      string
	anchors    map[string][]string
	Unresolved map[string][]string // opID → params generated without a source
	FKs        []db.FK
}

func New(d db.DB, tables map[string]*db.Table, fixtures map[string]Fixture, kparams []string) *Resolver {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return &Resolver{DB: d, Tables: tables, Fixtures: fixtures, KParams: kparams, RunID: hex.EncodeToString(b),
		anchors: map[string][]string{}, Unresolved: map[string][]string{}}
}

var idName = regexp.MustCompile(`(?i)^(\w+?)_?id$`)

// tableFor guesses the table referenced by a parameter.
func (r *Resolver) tableFor(name, path string) *db.Table {
	try := func(base string) *db.Table {
		base = strings.ToLower(base)
		for _, c := range []string{base, base + "s", base + "es", strings.TrimSuffix(base, "y") + "ies", strings.TrimSuffix(base, "s")} {
			if t := r.Tables[c]; t != nil && t.PK != "" {
				return t
			}
		}
		return nil
	}
	if m := idName.FindStringSubmatch(name); m != nil && m[1] != "" {
		if t := try(m[1]); t != nil {
			return t
		}
	}
	if strings.EqualFold(name, "id") || idName.MatchString(name) {
		segs := strings.Split(strings.Trim(path, "/"), "/")
		for i, s := range segs {
			if strings.Trim(s, "{}") == name && i > 0 {
				if t := try(segs[i-1]); t != nil {
					return t
				}
			}
		}
	}
	return nil
}

// Anchors returns PK values from the 1% nested subset so the same IDs exist
// at every data-scale step.
func (r *Resolver) Anchors(ctx context.Context, t *db.Table) []string {
	key := t.Name
	if v, ok := r.anchors[key]; ok {
		return v
	}
	q := fmt.Sprintf("SELECT s.%s FROM %s s WHERE %s ORDER BY s.%s LIMIT 50", t.PK, t.Name, r.anchorPred(t, "s", 0), t.PK)
	vals, err := r.DB.FirstValues(ctx, q)
	if err != nil || len(vals) == 0 {
		vals, _ = r.DB.FirstValues(ctx, fmt.Sprintf("SELECT %s FROM %s ORDER BY %s LIMIT 50", t.PK, t.Name, t.PK))
	}
	r.anchors[key] = vals
	return vals
}

// anchorPred mirrors subset membership: root tables by PK hash, child tables
// by "all FK parents are anchors" — so anchors exist in every nested subset.
func (r *Resolver) anchorPred(t *db.Table, alias string, depth int) string {
	var conds []string
	if depth < 6 {
		for _, f := range r.FKs {
			p := r.Tables[strings.ToLower(f.Parent)]
			if f.Child != t.Name || f.Parent == t.Name || p == nil {
				continue
			}
			pa := fmt.Sprintf("p%d", depth)
			conds = append(conds, fmt.Sprintf("(%s.%s IS NULL OR %s.%s IN (SELECT %s.%s FROM %s %s WHERE %s))",
				alias, f.ChildCol, alias, f.ChildCol, pa, f.ParentCol, p.Name, pa, r.anchorPred(p, pa, depth+1)))
		}
	}
	if len(conds) > 0 {
		return strings.Join(conds, " AND ")
	}
	return r.DB.HashPred(alias+"."+t.PK, 0.01)
}

func (r *Resolver) fixtureValue(ctx context.Context, v any, iter int) any {
	s, ok := v.(string)
	if !ok || !strings.HasPrefix(s, "sql:") {
		return v
	}
	q := strings.TrimSpace(strings.TrimPrefix(s, "sql:"))
	if strings.Contains(q, "{anchor}") {
		f := strings.Fields(q)
		col := "id"
		if len(f) > 1 {
			col = strings.TrimSuffix(f[1], ",")
		}
		q = strings.ReplaceAll(q, "{anchor}", r.DB.HashPred(col, 0.01))
	}
	vals, err := r.DB.FirstValues(ctx, q)
	if err != nil || len(vals) == 0 {
		return nil
	}
	return vals[iter%len(vals)]
}

func isKName(name string, list []string) bool {
	for _, k := range list {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// KParam detects the output-size knob: a pagination query param or a body array.
func (r *Resolver) KParam(op *spec.Operation) *KInfo {
	for _, p := range op.Params {
		if p.In == "query" && isKName(p.Name, r.KParams) && p.Schema != nil && (p.Schema.Type.Is("integer") || p.Schema.Type.Is("number")) {
			k := &KInfo{Name: p.Name, Min: 1, Max: 1000}
			if p.Schema.Min != nil {
				k.Min = int(*p.Schema.Min)
			}
			if p.Schema.Max != nil {
				k.Max = int(*p.Schema.Max)
			}
			return k
		}
	}
	if op.Body != nil && op.Body.Schema != nil && op.Method != "GET" {
		names := sortedProps(op.Body.Schema)
		for _, n := range names {
			ps := op.Body.Schema.Properties[n].Value
			if ps != nil && ps.Type.Is("array") && ps.Items != nil && ps.Items.Value != nil && ps.Items.Value.Type.Is("object") {
				k := &KInfo{Name: n, InBody: true, Min: int(ps.MinItems), Max: 1000}
				if k.Min < 1 {
					k.Min = 1
				}
				if ps.MaxItems != nil {
					k.Max = int(*ps.MaxItems)
				}
				return k
			}
		}
	}
	return nil
}

func sortedProps(s *openapi3.Schema) []string {
	var n []string
	for k := range s.Properties {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// Build creates the request for iteration iter. k < 0 means "default size".
// overrides force path param values (ID chaining for write lifecycles).
func (r *Resolver) Build(ctx context.Context, op *spec.Operation, iter, k int, ki *KInfo, overrides map[string]string) (*Request, error) {
	fx := r.Fixtures[op.ID]
	req := &Request{Method: op.Method, Path: op.Path, Query: url.Values{}, Header: map[string]string{}}
	unresolved := map[string]bool{}
	for _, p := range op.Params {
		var val any
		var have bool
		if ov, ok := overrides[p.Name]; ok && p.In == "path" {
			val, have = ov, true
		}
		if !have {
			var src map[string]any
			switch p.In {
			case "path":
				src = fx.Path
			case "query":
				src = fx.Query
			case "header":
				src = fx.Header
			}
			if v, ok := src[p.Name]; ok {
				val, have = r.fixtureValue(ctx, v, iter), true
			}
		}
		if ki != nil && !ki.InBody && p.Name == ki.Name && k >= 0 {
			val, have = k, true
		}
		if !have && (p.In == "path" || idName.MatchString(p.Name)) {
			if t := r.tableFor(p.Name, op.Path); t != nil {
				if a := r.Anchors(ctx, t); len(a) > 0 {
					val, have = a[iter%len(a)], true
				}
			}
		}
		if !have {
			if p.Example != nil {
				val, have = p.Example, true
			} else if p.Schema != nil && p.Schema.Example != nil {
				val, have = p.Schema.Example, true
			} else if p.Schema != nil && p.Schema.Default != nil {
				val, have = p.Schema.Default, true
			} else if p.Schema != nil && len(p.Schema.Enum) > 0 {
				val, have = p.Schema.Enum[0], true
			}
		}
		if !have && p.Required {
			val, have = r.gen(ctx, p.Schema, p.Name, op.Path, iter, 0), true
			unresolved[p.In+":"+p.Name] = true
		}
		if !have {
			continue
		}
		s := fmt.Sprint(val)
		switch p.In {
		case "path":
			req.Path = strings.ReplaceAll(req.Path, "{"+p.Name+"}", url.PathEscape(s))
		case "query":
			req.Query.Set(p.Name, s)
		case "header":
			req.Header[p.Name] = s
		case "cookie":
			req.Header["Cookie"] = strings.TrimPrefix(req.Header["Cookie"]+"; "+p.Name+"="+s, "; ")
		}
	}
	if op.Body != nil {
		var body any
		switch {
		case fx.Body != nil:
			body = r.resolveFixtureBody(ctx, deepCopy(fx.Body), iter)
		case op.Body.Example != nil && authPath.MatchString(op.Path):
			body = deepCopy(op.Body.Example) // credentials must stay exact
		case op.Body.Example != nil:
			body = r.refreshIDs(ctx, uniquify(deepCopy(op.Body.Example), r.RunID, iter, ""), op.Path, iter)
		case op.Body.Schema != nil:
			body = r.gen(ctx, op.Body.Schema, "", op.Path, iter, 0)
		}
		if ki != nil && ki.InBody && k >= 0 {
			if m, ok := body.(map[string]any); ok {
				var first any
				if arr, ok := m[ki.Name].([]any); ok && len(arr) > 0 {
					first = arr[0]
				} else if ps := op.Body.Schema.Properties[ki.Name]; ps != nil && ps.Value != nil && ps.Value.Items != nil {
					first = r.gen(ctx, ps.Value.Items.Value, ki.Name, op.Path, iter, 1)
				}
				arr := make([]any, k)
				for i := range arr {
					arr[i] = uniquify(deepCopy(first), r.RunID, iter*10000+i, "")
				}
				m[ki.Name] = arr
			}
		}
		req.Body = body
	}
	if len(unresolved) > 0 {
		var u []string
		for k := range unresolved {
			u = append(u, k)
		}
		sort.Strings(u)
		r.Unresolved[op.ID] = u
	}
	return req, nil
}

func (r *Resolver) resolveFixtureBody(ctx context.Context, v any, iter int) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = r.resolveFixtureBody(ctx, vv, iter)
		}
		return x
	case []any:
		for i, vv := range x {
			x[i] = r.resolveFixtureBody(ctx, vv, iter)
		}
		return x
	case string:
		return r.fixtureValue(ctx, x, iter)
	}
	return v
}

// refreshIDs replaces *_id / *Id fields with anchor IDs of matching tables.
func (r *Resolver) refreshIDs(ctx context.Context, v any, path string, iter int) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			if _, isObj := vv.(map[string]any); !isObj && idName.MatchString(k) && !strings.EqualFold(k, "id") {
				if t := r.tableFor(k, path); t != nil {
					if a := r.Anchors(ctx, t); len(a) > 0 {
						x[k] = numOrString(a[iter%len(a)])
						continue
					}
				}
			}
			x[k] = r.refreshIDs(ctx, vv, path, iter)
		}
	case []any:
		for i := range x {
			x[i] = r.refreshIDs(ctx, x[i], path, iter)
		}
	}
	return v
}

var authPath = regexp.MustCompile(`(?i)login|signin|sign-in|auth|token|session`)

var uniqueField = regexp.MustCompile(`(?i)email|username|user_name|login|handle|slug|^code$|token`)

// uniquify makes unique-looking string fields distinct per iteration.
func uniquify(v any, run string, iter int, key string) any {
	switch x := v.(type) {
	case map[string]any:
		for k, vv := range x {
			x[k] = uniquify(vv, run, iter, k)
		}
		return x
	case []any:
		for i := range x {
			x[i] = uniquify(x[i], run, iter, key)
		}
		return x
	case string:
		if uniqueField.MatchString(key) {
			if i := strings.Index(x, "@"); i > 0 {
				return fmt.Sprintf("%s+rp%s%d%s", x[:i], run, iter, x[i:])
			}
			return fmt.Sprintf("%s-rp%s%d", x, run, iter)
		}
	}
	return v
}

func (r *Resolver) gen(ctx context.Context, s *openapi3.Schema, name, path string, iter, depth int) any {
	if s == nil {
		return "rp"
	}
	if s.Example != nil {
		return uniquify(deepCopy(s.Example), r.RunID, iter, name)
	}
	if len(s.Enum) > 0 {
		return s.Enum[0]
	}
	if s.Default != nil {
		return s.Default
	}
	if len(s.AllOf) > 0 {
		merged := map[string]any{}
		for _, sub := range s.AllOf {
			if sub.Value != nil {
				if m, ok := r.gen(ctx, sub.Value, name, path, iter, depth+1).(map[string]any); ok {
					for k, v := range m {
						merged[k] = v
					}
				}
			}
		}
		return merged
	}
	for _, alts := range []openapi3.SchemaRefs{s.OneOf, s.AnyOf} {
		if len(alts) > 0 && alts[0].Value != nil {
			return r.gen(ctx, alts[0].Value, name, path, iter, depth+1)
		}
	}
	switch {
	case s.Type.Is("integer"), s.Type.Is("number"):
		if idName.MatchString(name) && !strings.EqualFold(name, "id") {
			if t := r.tableFor(name, path); t != nil {
				if a := r.Anchors(ctx, t); len(a) > 0 {
					return numOrString(a[iter%len(a)])
				}
			}
		}
		if s.Min != nil {
			return *s.Min
		}
		return 1
	case s.Type.Is("boolean"):
		return true
	case s.Type.Is("array"):
		n := int(s.MinItems)
		if n < 1 {
			n = 1
		}
		var out []any
		for i := 0; i < n; i++ {
			var it *openapi3.Schema
			if s.Items != nil {
				it = s.Items.Value
			}
			out = append(out, r.gen(ctx, it, name, path, iter, depth+1))
		}
		return out
	case s.Type.Is("object") || len(s.Properties) > 0:
		m := map[string]any{}
		if depth > 5 {
			return m
		}
		for _, k := range sortedProps(s) {
			if ps := s.Properties[k]; ps != nil && ps.Value != nil && !ps.Value.ReadOnly {
				m[k] = r.gen(ctx, ps.Value, k, path, iter, depth+1)
			}
		}
		return m
	}
	// strings
	var v string
	switch s.Format {
	case "email":
		v = fmt.Sprintf("rp%s%d@example.test", r.RunID, iter)
	case "date":
		v = time.Now().Format("2006-01-02")
	case "date-time":
		v = time.Now().UTC().Format(time.RFC3339)
	case "uuid":
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
		v = fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
	default:
		if strings.Contains(strings.ToLower(name), "email") {
			v = fmt.Sprintf("rp%s%d@example.test", r.RunID, iter)
		} else {
			v = fmt.Sprintf("rp-%s-%d", name, iter)
		}
	}
	if s.MaxLength != nil && uint64(len(v)) > *s.MaxLength {
		v = v[:*s.MaxLength]
	}
	for uint64(len(v)) < s.MinLength {
		v += "x"
	}
	return v
}

func numOrString(s string) any {
	var n int64
	if _, err := fmt.Sscan(s, &n); err == nil && fmt.Sprint(n) == s {
		return n
	}
	return s
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, vv := range x {
			m[k] = deepCopy(vv)
		}
		return m
	case []any:
		a := make([]any, len(x))
		for i, vv := range x {
			a[i] = deepCopy(vv)
		}
		return a
	}
	return v
}
