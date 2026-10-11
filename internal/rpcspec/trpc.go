package rpcspec

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/getkin/kin-openapi/openapi3"

	"github.com/nazishasghar/routeperf/internal/spec"
)

// Manifest lists a tRPC router's procedures. tRPC has no runtime
// introspection, so routeperf builds it by loading the router module with
// Node (ExtractRouter), or reads one written by hand or by
// `routeperf trpc-manifest`.
type Manifest struct {
	TRPC struct {
		Version     int         `json:"version,omitempty"`
		Transformer string      `json:"transformer,omitempty"` // "" (none) | superjson
		Procedures  []Procedure `json:"procedures"`
	} `json:"trpc"`
}

type Procedure struct {
	Path  string         `json:"path"` // user.byId
	Type  string         `json:"type"` // query | mutation | subscription
	Input map[string]any `json:"input,omitempty"`
	// NoInput is true when the procedure declares no input parser; Input nil
	// with NoInput false means a parser routeperf couldn't convert.
	NoInput bool           `json:"no_input,omitempty"`
	Meta    map[string]any `json:"meta,omitempty"`
}

// ParseManifest reads a manifest.
func ParseManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("tRPC manifest: %w", err)
	}
	if m.TRPC.Procedures == nil {
		return nil, fmt.Errorf(`tRPC manifest: no "trpc.procedures" list`)
	}
	return &m, nil
}

//go:embed trpc_extract.mjs
var extractScript string

// ExtractRouter loads a tRPC router module with Node and returns its
// manifest. file may end in #exportName; ts/tsx/mts/cts files run through
// tsx. command overrides the runner (e.g. "pnpm exec tsx"). The module is
// imported, so top-level code in it (DB clients, env checks) runs.
func ExtractRouter(ctx context.Context, file, command string) ([]byte, error) {
	file, export, _ := strings.Cut(file, "#")
	abs, err := filepath.Abs(file)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("tRPC router %s: %w", file, err)
	}
	dir, err := os.MkdirTemp("", "routeperf-trpc-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	script, out := filepath.Join(dir, "extract.mjs"), filepath.Join(dir, "manifest.json")
	if err := os.WriteFile(script, []byte(extractScript), 0o600); err != nil {
		return nil, err
	}
	argv := strings.Fields(command)
	if len(argv) == 0 {
		argv = []string{"node"}
		switch strings.ToLower(filepath.Ext(abs)) {
		case ".ts", ".tsx", ".mts", ".cts":
			argv = []string{"npx", "--yes", "tsx"}
		}
	}
	argv = append(argv, script, abs, out, export)
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "SKIP_ENV_VALIDATION=1") // create-t3-app's env check
	b, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(b))
		if len(msg) > 1500 {
			msg = "…" + msg[len(msg)-1500:]
		}
		return nil, fmt.Errorf("loading the tRPC router with `%s`: %v\n%s", strings.Join(argv[:len(argv)-4], " "), err, msg)
	}
	return os.ReadFile(out)
}

// TRPCOperations builds one operation per query and mutation; subscriptions
// are listed as unsupported.
func TRPCOperations(m *Manifest, title string) *spec.Spec {
	sp := &spec.Spec{Title: title}
	procs := append([]Procedure(nil), m.TRPC.Procedures...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].Path < procs[j].Path })
	for _, p := range procs {
		o := &spec.Operation{ID: p.Path, Method: strings.ToUpper(p.Type), Path: p.Path, Protocol: "trpc", Phase: "W", Resource: Resource(p.Path), Depth: 1,
			RPC: &spec.RPCOp{Name: p.Path, Kind: p.Type}}
		switch p.Type {
		case "query":
			o.Phase = "R"
		case "mutation":
		case "subscription":
			o.Unsupported = "tRPC subscription"
		default:
			o.Unsupported = "unknown procedure type " + p.Type
		}
		if tags, ok := p.Meta["tags"].([]any); ok {
			for _, t := range tags {
				o.Tags = append(o.Tags, fmt.Sprint(t))
			}
		}
		if len(p.Path) > 0 && strings.Contains(p.Path, ".") {
			o.Tags = append(o.Tags, p.Path[:strings.LastIndex(p.Path, ".")]) // router namespace: tag:user
		}
		switch {
		case p.NoInput:
			o.RPC.Wrap = "none"
		case p.Input == nil:
			o.RPC.Wrap = "object" // unknown validator: send {}
		default:
			optional, _ := p.Input["x-optional"].(bool)
			setArgs(o, Schema(p.Input, p.Input), !optional)
		}
		sp.Ops = append(sp.Ops, o)
	}
	return sp
}

// setArgs turns an input schema into "arg" params: an object's properties
// become one arg each (only required ones are generated, so optional filters
// stay off); any other input is one arg named "input".
func setArgs(o *spec.Operation, s *openapi3.Schema, required bool) {
	if s.Type.Is("object") && len(s.Properties) > 0 {
		o.RPC.Wrap = "object"
		req := map[string]bool{}
		for _, n := range s.Required {
			req[n] = true
		}
		names := make([]string, 0, len(s.Properties))
		for n := range s.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			ps := s.Properties[n].Value
			o.Params = append(o.Params, spec.Param{Name: n, In: "arg", Required: req[n] && ps.Default == nil, Schema: ps, Example: ps.Example})
		}
		return
	}
	if s.Type.Is("object") {
		o.RPC.Wrap = "object"
		return
	}
	o.RPC.Wrap = "value"
	o.Params = append(o.Params, spec.Param{Name: "input", In: "arg", Required: required && !s.Nullable, Schema: s, Example: s.Example})
}
