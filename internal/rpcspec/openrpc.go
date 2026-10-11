package rpcspec

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/spec"
)

// OpenRPC is the subset of an OpenRPC document routeperf reads.
type OpenRPC struct {
	Title   string
	Servers []string
	Ops     *spec.Spec
}

// DiscoverRequest asks a JSON-RPC server for its OpenRPC document.
var DiscoverRequest = map[string]any{"jsonrpc": "2.0", "method": "rpc.discover", "id": 1}

// ParseOpenRPC reads an OpenRPC document (or an rpc.discover response) into
// one operation per method. Params are sent by name when the method says
// paramStructure "by-name", else by position (what "either" servers, and
// most Ethereum-style ones, accept); paramsBy "by-name" or "by-position"
// overrides that ("" = as declared).
func ParseOpenRPC(b []byte, paramsBy string) (*OpenRPC, error) {
	var root map[string]any
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("OpenRPC document: %w", err)
	}
	if res, ok := root["result"].(map[string]any); ok && root["jsonrpc"] != nil {
		root = res // rpc.discover response
	} else if e, ok := root["error"].(map[string]any); ok {
		return nil, fmt.Errorf("rpc.discover: %v", e["message"])
	}
	if root["openrpc"] == nil {
		return nil, fmt.Errorf(`not an OpenRPC document (no "openrpc" field)`)
	}
	doc := &OpenRPC{}
	if info, ok := root["info"].(map[string]any); ok {
		doc.Title, _ = info["title"].(string)
	}
	for _, s := range asList(root["servers"]) {
		if m, ok := s.(map[string]any); ok {
			if u, ok := m["url"].(string); ok {
				doc.Servers = append(doc.Servers, u)
			}
		}
	}
	sp := &spec.Spec{Title: doc.Title}
	deref := func(v any) map[string]any {
		for i := 0; i < 8; i++ {
			m, ok := v.(map[string]any)
			if !ok {
				return nil
			}
			ref, isRef := m["$ref"].(string)
			if !isRef {
				return m
			}
			v = resolve(root, ref)
		}
		return nil
	}
	for _, mv := range asList(root["methods"]) {
		m := deref(mv)
		name, _ := m["name"].(string)
		if name == "" || strings.HasPrefix(name, "rpc.") {
			continue
		}
		o := &spec.Operation{ID: name, Method: "RPC", Path: name, Protocol: "jsonrpc", Phase: "W", Resource: Resource(name), Depth: 1,
			RPC: &spec.RPCOp{Name: name, Wrap: "array"}}
		o.Summary, _ = m["summary"].(string)
		if ro, declared := readOnly(m); ro || !declared && IsRead(name) {
			o.Phase = "R"
		}
		if structure, _ := m["paramStructure"].(string); paramsBy == "by-name" || paramsBy == "" && structure == "by-name" {
			o.RPC.Wrap = "object"
		}
		for _, t := range asList(m["tags"]) {
			if tm := deref(t); tm != nil {
				if n, ok := tm["name"].(string); ok {
					o.Tags = append(o.Tags, n)
				}
			}
		}
		examples := map[string]any{}
		if ex := asList(m["examples"]); len(ex) > 0 {
			if em := deref(ex[0]); em != nil {
				for _, p := range asList(em["params"]) {
					if pm := deref(p); pm != nil {
						if n, ok := pm["name"].(string); ok {
							examples[n] = pm["value"]
						}
					}
				}
			}
		}
		for _, pv := range asList(m["params"]) {
			p := deref(pv)
			pname, _ := p["name"].(string)
			if pname == "" {
				continue
			}
			req, _ := p["required"].(bool)
			sch := Schema(p["schema"], root)
			ex := examples[pname]
			if ex == nil {
				ex = sch.Example
			}
			o.Params = append(o.Params, spec.Param{Name: pname, In: "arg", Required: req, Schema: sch, Example: ex})
			o.RPC.Args = append(o.RPC.Args, pname)
		}
		sp.Ops = append(sp.Ops, o)
	}
	sort.Slice(sp.Ops, func(i, j int) bool { return sp.Ops[i].Path < sp.Ops[j].Path })
	doc.Ops = sp
	return doc, nil
}

// readOnly reads x-readonly / x-read-only / x-side-effects (declared =
// false when the method has none: its name decides).
func readOnly(m map[string]any) (ro, declared bool) {
	for _, k := range []string{"x-readonly", "x-read-only", "x-readOnly"} {
		if b, ok := m[k].(bool); ok {
			return b, true
		}
	}
	if b, ok := m["x-side-effects"].(bool); ok {
		return !b, true
	}
	return false, false
}
