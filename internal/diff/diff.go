// Package diff compares two routeperf results and flags endpoints that got
// worse: a higher Big O, slower p95, more queries per request, new N+1s or a
// worse status.
package diff

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Options set what counts as a regression.
type Options struct {
	P95Pct     float64 // relative p95 increase that fails (20 = +20%)
	P95MinMs   float64 // ignore p95 changes smaller than this (noise floor)
	QueriesAbs float64 // queries/request increase that fails
	FailOn     map[string]bool
}

func DefaultOptions() Options {
	return Options{P95Pct: 20, P95MinMs: 5, QueriesAbs: 1,
		FailOn: map[string]bool{"bigo": true, "p95": true, "queries": true, "nplusone": true, "status": true, "missing": false}}
}

type op struct {
	Method     string                          `json:"method"`
	Path       string                          `json:"path"`
	Role       string                          `json:"role"`
	Status     string                          `json:"status"`
	BigO       string                          `json:"big_o"`
	NDegree    *float64                        `json:"n_degree"`
	KDegree    *float64                        `json:"k_degree"`
	Confidence string                          `json:"confidence"`
	Latency    struct{ P50, P95, P99 float64 } `json:"latency_ms"`
	QPerReq    float64                         `json:"queries_per_request"`
	DBMs       float64                         `json:"db_ms_per_request"`
	NPlusOne   []string                        `json:"n_plus_one"`
	Skipped    string                          `json:"skipped"`
}

type result struct {
	Spec    string `json:"spec_title"`
	Started string `json:"started"`
	Ops     []op   `json:"operations"`
}

// Change is one compared endpoint.
type Change struct {
	Endpoint   string
	BaseBigO   string
	NewBigO    string
	BaseP95    float64
	NewP95     float64
	BaseQ      float64
	NewQ       float64
	BaseStatus string
	NewStatus  string
	Problems   []string // regressions (fail CI)
	Notes      []string // improvements and neutral changes
}

// Report is the outcome of a comparison.
type Report struct {
	Base, New string
	Changes   []Change
	Added     []string
	Removed   []string
	Failed    bool
}

func load(path string) (*result, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r result
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// Compare loads two results.json files and compares them endpoint by endpoint.
func Compare(basePath, newPath string, o Options) (*Report, error) {
	base, err := load(basePath)
	if err != nil {
		return nil, err
	}
	cur, err := load(newPath)
	if err != nil {
		return nil, err
	}
	rep := &Report{Base: basePath, New: newPath}
	key := func(x op) string {
		if x.Role != "" {
			return x.Method + " " + x.Path + " [" + x.Role + "]"
		}
		return x.Method + " " + x.Path
	}
	bm := map[string]op{}
	for _, x := range base.Ops {
		bm[key(x)] = x
	}
	seen := map[string]bool{}
	for _, n := range cur.Ops {
		k := key(n)
		seen[k] = true
		b, ok := bm[k]
		if !ok {
			rep.Added = append(rep.Added, k)
			continue
		}
		c := Change{Endpoint: k, BaseBigO: b.BigO, NewBigO: n.BigO, BaseP95: b.Latency.P95, NewP95: n.Latency.P95,
			BaseQ: b.QPerReq, NewQ: n.QPerReq, BaseStatus: b.Status, NewStatus: n.Status}
		fail := func(kind, msg string) {
			if o.FailOn[kind] {
				c.Problems = append(c.Problems, msg)
			} else {
				c.Notes = append(c.Notes, msg)
			}
		}
		bd, nd := degrees(b), degrees(n)
		switch cmp := compareDeg(bd, nd); {
		case cmp > 0:
			fail("bigo", fmt.Sprintf("Big O worse: %s → %s", b.BigO, n.BigO))
		case cmp < 0:
			c.Notes = append(c.Notes, fmt.Sprintf("Big O better: %s → %s", b.BigO, n.BigO))
		}
		if d := n.Latency.P95 - b.Latency.P95; b.Latency.P95 > 0 && d >= o.P95MinMs && d/b.Latency.P95*100 >= o.P95Pct {
			fail("p95", fmt.Sprintf("p95 +%.0f%% (%.1f → %.1f ms)", d/b.Latency.P95*100, b.Latency.P95, n.Latency.P95))
		} else if b.Latency.P95 > 0 && -d >= o.P95MinMs && -d/b.Latency.P95*100 >= o.P95Pct {
			c.Notes = append(c.Notes, fmt.Sprintf("p95 %.0f%% (%.1f → %.1f ms)", d/b.Latency.P95*100, b.Latency.P95, n.Latency.P95))
		}
		if n.QPerReq-b.QPerReq >= o.QueriesAbs {
			fail("queries", fmt.Sprintf("queries/request %.0f → %.0f", b.QPerReq, n.QPerReq))
		}
		if len(n.NPlusOne) > 0 && len(b.NPlusOne) == 0 {
			fail("nplusone", "new N+1 query")
		}
		rank := map[string]int{"OK": 0, "WARN": 1, "FAIL": 2}
		if rank[n.Status] > rank[b.Status] {
			fail("status", fmt.Sprintf("status %s → %s", b.Status, n.Status))
		} else if rank[n.Status] < rank[b.Status] {
			c.Notes = append(c.Notes, fmt.Sprintf("status %s → %s", b.Status, n.Status))
		}
		if len(c.Problems) > 0 {
			rep.Failed = true
		}
		rep.Changes = append(rep.Changes, c)
	}
	for k := range bm {
		if !seen[k] {
			rep.Removed = append(rep.Removed, k)
		}
	}
	sort.Strings(rep.Removed)
	if o.FailOn["missing"] && len(rep.Removed) > 0 {
		rep.Failed = true
	}
	sort.SliceStable(rep.Changes, func(i, j int) bool { return len(rep.Changes[i].Problems) > len(rep.Changes[j].Problems) })
	return rep, nil
}

// deg is a Big O's growth in table size and in k, with log factors.
type deg struct{ n, nlog, k, klog float64 }

func degrees(x op) deg {
	d := parseBigO(x.BigO)
	if x.NDegree != nil && x.KDegree != nil { // exact values from newer results
		d.n, d.k = *x.NDegree, *x.KDegree
	}
	return d
}

var factorRe = regexp.MustCompile(`^(log(?:\^([\d.]+))? )?(k|n_[\w.]+)(?:\^([\d.]+))?$`)

// parseBigO reads "O(k·log n_items + n_orders^2)" into the dominant degrees.
func parseBigO(s string) deg {
	s = strings.TrimSuffix(strings.TrimPrefix(strings.TrimSpace(s), "O("), ")")
	var best deg
	for _, term := range strings.Split(s, " + ") {
		var t deg
		for _, f := range strings.Split(term, "·") {
			m := factorRe.FindStringSubmatch(strings.TrimSpace(f))
			if m == nil {
				continue
			}
			p := 1.0
			if m[1] != "" { // log factor
				if m[2] != "" {
					p, _ = strconv.ParseFloat(m[2], 64)
				}
				if m[3] == "k" {
					t.klog += p
				} else {
					t.nlog += p
				}
				continue
			}
			if m[4] != "" {
				p, _ = strconv.ParseFloat(m[4], 64)
			}
			if m[3] == "k" {
				t.k += p
			} else {
				t.n += p
			}
		}
		if t.n > best.n || t.n == best.n && t.nlog > best.nlog { // dominant term per dimension
			best.n, best.nlog = t.n, t.nlog
		}
		if t.k > best.k || t.k == best.k && t.klog > best.klog {
			best.k, best.klog = t.k, t.klog
		}
	}
	return best
}

// compareDeg: >0 when b is worse than a (in either dimension), <0 when better.
func compareDeg(a, b deg) int {
	worse := b.n > a.n+0.25 || b.k > a.k+0.25 || b.n == a.n && b.nlog > a.nlog || b.k == a.k && b.klog > a.klog
	better := b.n < a.n-0.25 || b.k < a.k-0.25 || b.n == a.n && b.nlog < a.nlog || b.k == a.k && b.klog < a.klog
	switch {
	case worse:
		return 1
	case better:
		return -1
	}
	return 0
}

// Print writes the comparison for the terminal.
func (r *Report) Print(w io.Writer) {
	bad := 0
	for _, c := range r.Changes {
		if len(c.Problems) > 0 {
			bad++
		}
	}
	fmt.Fprintf(w, "routeperf diff  %s → %s\n\n", r.Base, r.New)
	fmt.Fprintf(w, "%-44s %-28s %-28s %16s %10s\n", "ENDPOINT", "BIG O (base)", "BIG O (new)", "P95 ms", "Q/REQ")
	for _, c := range r.Changes {
		if len(c.Problems) == 0 && len(c.Notes) == 0 {
			continue
		}
		fmt.Fprintf(w, "%-44s %-28s %-28s %7.1f→%-7.1f %4.0f→%-4.0f\n", trunc(c.Endpoint, 44), trunc(c.BaseBigO, 28), trunc(c.NewBigO, 28), c.BaseP95, c.NewP95, c.BaseQ, c.NewQ)
		for _, p := range c.Problems {
			fmt.Fprintf(w, "    ✗ %s\n", p)
		}
		for _, n := range c.Notes {
			fmt.Fprintf(w, "    · %s\n", n)
		}
	}
	for _, a := range r.Added {
		fmt.Fprintf(w, "  + %s (new endpoint)\n", a)
	}
	for _, a := range r.Removed {
		fmt.Fprintf(w, "  - %s (no longer measured)\n", a)
	}
	if bad == 0 {
		fmt.Fprintf(w, "\n✓ no regressions across %d endpoints\n", len(r.Changes))
	} else {
		fmt.Fprintf(w, "\n✗ %d of %d endpoints regressed\n", bad, len(r.Changes))
	}
}

// Markdown renders the comparison for a PR comment.
func (r *Report) Markdown() string {
	var b strings.Builder
	b.WriteString("## routeperf diff\n\n")
	b.WriteString("| | Endpoint | Big O | p95 | Queries/req | Change |\n|---|---|---|---|---|---|\n")
	for _, c := range r.Changes {
		if len(c.Problems) == 0 && len(c.Notes) == 0 {
			continue
		}
		mark, msgs := "✅", c.Notes
		if len(c.Problems) > 0 {
			mark, msgs = "❌", append(append([]string{}, c.Problems...), c.Notes...)
		}
		bigo := "`" + c.NewBigO + "`"
		if c.BaseBigO != c.NewBigO {
			bigo = "`" + c.BaseBigO + "` → `" + c.NewBigO + "`"
		}
		fmt.Fprintf(&b, "| %s | `%s` | %s | %.1f → %.1f ms | %.0f → %.0f | %s |\n", mark, c.Endpoint, bigo, c.BaseP95, c.NewP95, c.BaseQ, c.NewQ, strings.Join(msgs, "; "))
	}
	for _, a := range r.Added {
		fmt.Fprintf(&b, "| ➕ | `%s` | | | | new endpoint |\n", a)
	}
	for _, a := range r.Removed {
		fmt.Fprintf(&b, "| ➖ | `%s` | | | | no longer measured |\n", a)
	}
	if !r.Failed {
		fmt.Fprintf(&b, "\nNo regressions across %d endpoints.\n", len(r.Changes))
	}
	return b.String()
}

func trunc(s string, n int) string {
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}
