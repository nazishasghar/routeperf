// Package report renders run results to the terminal, JSON, Markdown and HTML.
package report

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/auth"
	"github.com/nazishasghar/routeperf/internal/runner"
)

var Color = true

func c(code, s string) string {
	if !Color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func statusColor(s string) string {
	switch s {
	case "ok", "OK":
		return c("32", s)
	case "warn", "WARN":
		return c("33", s)
	case "fail", "FAIL":
		return c("31", s)
	}
	return c("90", s)
}

func Checks(w io.Writer, checks []runner.Check) {
	icon := map[string]string{"ok": c("32", "✓"), "warn": c("33", "!"), "fail": c("31", "✗"), "skip": c("90", "-")}
	for _, ch := range checks {
		fmt.Fprintf(w, "  %s %-22s %s\n", icon[ch.Status], ch.Name, ch.Detail)
		if ch.Fix != "" && ch.Status != "ok" {
			fmt.Fprintf(w, "    %s %s\n", c("90", "fix:"), ch.Fix)
		}
	}
}

func trunc(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len([]rune(s)) > n {
		return string([]rune(s)[:n-1]) + "…"
	}
	return s
}

func hasCold(res *runner.Result) bool {
	for _, o := range res.Ops {
		if o.Cold != nil {
			return true
		}
	}
	return false
}

func Terminal(w io.Writer, res *runner.Result) {
	fmt.Fprintf(w, "\n%s  %s · %s %s · %.0fs", c("1", "routeperf"), res.SpecTitle, res.Dialect, res.DBVersion, res.Seconds)
	if res.DBTime != "" {
		fmt.Fprintf(w, " · DB time: %s", res.DBTime)
	}
	fmt.Fprint(w, "\n")
	cold := hasCold(res)
	coldHdr := ""
	if cold {
		coldHdr = fmt.Sprintf(" %8s", "COLD")
	}
	fmt.Fprintf(w, "%s\n", c("90", "latency is warm-cache"+map[bool]string{true: "; COLD = first hit after cache eviction (" + res.ColdMethod + ")", false: ""}[cold]))
	fmt.Fprintf(w, "%-7s %-34s %8s %8s%s %8s %-8s %10s %-40s %-6s %s\n", "METHOD", "ROUTE", "P50", "P95", coldHdr, "DB", "Q/REQ", "ROWS/REQ", "BIG O", "CONF", "STATUS")
	for _, o := range res.Ops {
		reason := ""
		if len(o.Reasons) > 0 {
			reason = " " + c("90", trunc(o.Reasons[0], 48))
		}
		coldCol := ""
		if cold {
			if o.Cold != nil {
				coldCol = fmt.Sprintf(" %6.1fms", o.Cold.P50)
			} else {
				coldCol = fmt.Sprintf(" %8s", "—")
			}
		}
		fmt.Fprintf(w, "%-7s %-34s %6.1fms %6.1fms%s %6.2fms %-8s %10s %-40s %-6s %s%s\n", o.Method, trunc(o.Path, 34), o.Latency.P50, o.Latency.P95, coldCol, o.DBMs,
			trunc(o.QModel, 8), human(o.RowsPerReq), trunc(o.BigO, 40), o.Confidence, statusColor(o.Status), reason)
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintf(w, "\n%s\n", c("90", fmt.Sprintf("skipped %d operation(s):", len(res.Skipped))))
		for _, s := range res.Skipped {
			fmt.Fprintf(w, "  %s %s %s\n", c("90", s.Method), s.Path, c("90", "— "+s.Skipped))
		}
	}
	var findings []string
	for _, o := range res.Ops {
		if o.Status == "OK" {
			continue
		}
		for i, a := range o.Advice {
			if i >= 3 {
				break
			}
			line := fmt.Sprintf("  %s %s %s — %s", statusColor(o.Status), o.Method, o.Path, a.Message)
			if a.SQL != "" {
				line += "\n      " + c("36", a.SQL)
			}
			if a.Verified != "" {
				line += "\n      " + c("32", "✓ "+a.Verified)
			}
			findings = append(findings, line)
		}
	}
	if len(findings) > 0 {
		fmt.Fprintf(w, "\n%s\n%s\n", c("1", "Findings"), strings.Join(findings, "\n"))
	}
	var load []string
	for _, o := range res.Ops {
		if len(o.Load) == 0 {
			continue
		}
		last := o.Load[len(o.Load)-1]
		load = append(load, fmt.Sprintf("  %-6s %-34s %5.0f req/s at c=%-3d p95 %6.1fms  %s", o.Method, trunc(o.Path, 34), last.RPS, last.Concurrency, last.P95, c("90", o.LoadNote)))
	}
	if len(load) > 0 {
		fmt.Fprintf(w, "\n%s\n%s\n", c("1", "Under load"), strings.Join(load, "\n"))
	}
	if res.Snapshot != "none" {
		v := c("32", "verified")
		if !res.Verified {
			v = c("31", "NOT verified")
		}
		fmt.Fprintf(w, "\nDB restored (%s): %s — %s\n", res.Snapshot, strings.Join(res.Restored, ", "), v)
	}
	if res.AdviceProof == "unavailable" && res.Dialect == "postgres" {
		fmt.Fprintf(w, "%s index advice is unproven: install HypoPG (CREATE EXTENSION hypopg) to have each suggestion checked by the planner\n", c("90", "note:"))
	}
	for _, wn := range res.Warnings {
		fmt.Fprintf(w, "%s %s\n", c("33", "warning:"), wn)
	}
}

func human(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.1fM", v/1e6)
	case v >= 1e4:
		return fmt.Sprintf("%.0fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func redacted(res *runner.Result, data []byte) []byte {
	return []byte(auth.Redact(string(data), res.Secrets()))
}

func WriteJSON(path string, res *runner.Result) error {
	b, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, redacted(res, b), 0o644)
}

func headerLines(res *runner.Result) []string {
	lines := []string{
		fmt.Sprintf("API: `%s`", res.API),
		fmt.Sprintf("Database: %s %s (`%s`)", res.Dialect, res.DBVersion, res.Database),
		fmt.Sprintf("Started: %s (%.0fs)", res.Started.Format("2006-01-02 15:04:05"), res.Seconds),
		fmt.Sprintf("Data-scale steps: %v", res.DataSteps),
	}
	if res.Capture != "" {
		lines = append(lines, fmt.Sprintf("SQL captured from: %s", res.Capture))
	}
	if res.DBTime != "" {
		lines = append(lines, fmt.Sprintf("DB time source: %s", res.DBTime))
	}
	cache := "warm (every number is a warm-cache measurement)"
	if res.ColdMethod != "" {
		cache = "warm, plus cold first hits (" + res.ColdMethod + ")"
	}
	lines = append(lines, "Cache: "+cache)
	if res.Correlated {
		lines = append(lines, "SQL attribution: by traceparent (sqlcommenter tags)")
	}
	switch res.AdviceProof {
	case "hypopg":
		lines = append(lines, "Index advice: proven with HypoPG hypothetical indexes")
	case "unavailable":
		lines = append(lines, "Index advice: unproven (HypoPG not available)")
	}
	if res.Masked {
		lines = append(lines, "Literal values: masked")
	}
	snap := "Snapshot: " + res.Snapshot
	if res.Snapshot != "none" {
		snap += fmt.Sprintf(" (restored: %s, verified: %v)", strings.Join(res.Restored, ", "), res.Verified)
	}
	return append(lines, snap)
}

func WriteMarkdown(path string, res *runner.Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# routeperf report: %s\n\n", res.SpecTitle)
	for _, l := range headerLines(res) {
		fmt.Fprintf(&b, "- %s\n", l)
	}
	cold := hasCold(res)
	b.WriteString("\n## Endpoints\n\n| Status | Method | Endpoint | p50 | p95 | p99 |")
	if cold {
		b.WriteString(" cold p50 |")
	}
	b.WriteString(" DB time/req | Queries/req | Rows examined/req | Big O | Confidence | Projected p50 @10× data |\n|---|---|---|---|---|---|")
	if cold {
		b.WriteString("---|")
	}
	b.WriteString("---|---|---|---|---|---|\n")
	for _, o := range res.Ops {
		proj := "—"
		if v, ok := o.Projection["10x"]; ok {
			proj = fmt.Sprintf("%.0f ms", v)
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %.1f ms | %.1f ms | %.1f ms |", o.Status, o.Method, o.Path, o.Latency.P50, o.Latency.P95, o.Latency.P99)
		if cold {
			if o.Cold != nil {
				fmt.Fprintf(&b, " %.1f ms |", o.Cold.P50)
			} else {
				b.WriteString(" — |")
			}
		}
		fmt.Fprintf(&b, " %.1f ms | %s | %s | `%s` | %s | %s |\n", o.DBMs, o.QModel, human(o.RowsPerReq), o.BigO, o.Confidence, proj)
	}
	if len(res.Skipped) > 0 {
		b.WriteString("\n### Skipped\n\n")
		for _, s := range res.Skipped {
			fmt.Fprintf(&b, "- `%s %s` — %s\n", s.Method, s.Path, s.Skipped)
		}
	}
	b.WriteString("\n## Route details\n")
	for _, o := range res.Ops {
		fmt.Fprintf(&b, "\n### %s `%s %s`\n\n", o.Status, o.Method, o.Path)
		fmt.Fprintf(&b, "- **Big O:** `%s` (confidence %s)", o.BigO, o.Confidence)
		if o.Dominant != "" {
			fmt.Fprintf(&b, ", dominant table `%s`", o.Dominant)
		}
		fmt.Fprintf(&b, "\n- **Latency (warm):** p50 %.1f ms · p95 %.1f ms · p99 %.1f ms (n=%d)\n", o.Latency.P50, o.Latency.P95, o.Latency.P99, o.Latency.N)
		if o.Cold != nil {
			fmt.Fprintf(&b, "- **Latency (cold, first hit after eviction):** p50 %.1f ms · p95 %.1f ms (n=%d) · DB %.1f ms/request\n", o.Cold.P50, o.Cold.P95, o.Cold.N, o.ColdDBMs)
		}
		fmt.Fprintf(&b, "- **DB time/request:** %.1f ms · **rows examined/request:** %s\n", o.DBMs, human(o.RowsPerReq))
		fmt.Fprintf(&b, "- **Queries/request:** %s", o.QModel)
		if len(o.NPlusOne) > 0 {
			fmt.Fprintf(&b, " — N+1: %s", strings.Join(o.NPlusOne, ", "))
		}
		b.WriteString("\n")
		if o.Pages > 0 {
			fmt.Fprintf(&b, "- **Cursor pagination:** timed requests walked %d pages\n", o.Pages)
		}
		if len(o.Projection) > 0 {
			keys := make([]string, 0, len(o.Projection))
			for k := range o.Projection {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			var ps []string
			for _, k := range keys {
				ps = append(ps, fmt.Sprintf("%s data → p50 ≈ %.0f ms", k, o.Projection[k]))
			}
			fmt.Fprintf(&b, "- **Projected:** %s\n", strings.Join(ps, " · "))
		}
		if o.KParam != "" && len(o.KLatency) > 0 {
			var ks []int
			for k := range o.KLatency {
				ks = append(ks, k)
			}
			sort.Ints(ks)
			var ls []string
			for _, k := range ks {
				ls = append(ls, fmt.Sprintf("k=%d: %.1f ms", k, o.KLatency[k]))
			}
			fmt.Fprintf(&b, "- **Output scaling (`%s`):** %s", o.KParam, strings.Join(ls, ", "))
			if o.AppSlope > 0 {
				fmt.Fprintf(&b, "; app-side time grows with log-log slope %s", analyze.FmtSlope(o.AppSlope, o.AppSlopeCI))
				if o.AppKExp >= 2 {
					fmt.Fprintf(&b, " (k² term %.0f%% of app time at the largest k)", o.AppShare*100)
				}
			}
			b.WriteString("\n")
		}
		for _, r := range o.Reasons {
			fmt.Fprintf(&b, "- ⚠ %s\n", r)
		}
		if len(o.Load) > 0 {
			b.WriteString("\n**Under load**")
			if o.LoadNote != "" {
				fmt.Fprintf(&b, ": %s", o.LoadNote)
			}
			b.WriteString("\n\n| concurrency | req/s | p50 | p95 | p99 | errors | DB ms/req | queries/req | DB conns (max) | lock waits (max) |\n|---|---|---|---|---|---|---|---|---|---|\n")
			for _, l := range o.Load {
				dbms, qpr := "—", "—"
				if l.QPerReq > 0 {
					dbms, qpr = fmt.Sprintf("%.1f", l.DBMs), fmt.Sprintf("%.0f", l.QPerReq)
				}
				fmt.Fprintf(&b, "| %d | %.0f | %.1f ms | %.1f ms | %.1f ms | %d | %s | %s | %d | %d |\n", l.Concurrency, l.RPS, l.P50, l.P95, l.P99, l.Errors, dbms, qpr, l.MaxConns, l.LockWaits)
			}
		}
		if len(o.Advice) > 0 {
			b.WriteString("\n**Advice**\n\n")
			for _, a := range o.Advice {
				fmt.Fprintf(&b, "- **%s**", a.Rule)
				if a.Query != "" {
					fmt.Fprintf(&b, " (%s)", a.Query)
				}
				fmt.Fprintf(&b, ": %s", a.Message)
				if a.SQL != "" {
					fmt.Fprintf(&b, " → `%s`", a.SQL)
				}
				if a.Expected != "" {
					fmt.Fprintf(&b, " (expected %s)", a.Expected)
				}
				if a.Verified != "" {
					fmt.Fprintf(&b, " — ✓ %s", a.Verified)
				}
				b.WriteString("\n")
			}
		}
		for _, q := range o.Queries {
			fmt.Fprintf(&b, "\n<details><summary><b>%s</b> ×%.0f/req · <code>%s</code> · %s</summary>\n\n", q.ID, q.CountPerReq, q.BigO, trunc(stripTags(q.SQL), 90))
			fmt.Fprintf(&b, "```sql\n%s\n```\n\n", q.Example.SQL)
			if q.Error != "" {
				fmt.Fprintf(&b, "Replay error: `%s`\n\n", q.Error)
			}
			fmt.Fprintf(&b, "- static estimate `%s`, confidence %s", q.Static, q.Confidence)
			if q.WorkFit != nil && q.WorkFit.OK {
				fmt.Fprintf(&b, "; rows-examined fit %s (slope %s, R² %.3f, %.1f decades, %d points)", q.WorkFit.ClassName, analyze.FmtSlope(q.WorkFit.Slope, q.WorkFit.SlopeCI), q.WorkFit.R2, q.WorkFit.Decades, q.WorkFit.Points)
			}
			if q.TimeFit != nil && q.TimeFit.OK {
				fmt.Fprintf(&b, "; time fit %s (slope %s)", q.TimeFit.ClassName, analyze.FmtSlope(q.TimeFit.Slope, q.TimeFit.SlopeCI))
			}
			if q.KSlope > 0 {
				fmt.Fprintf(&b, "; rows examined vs k slope %s", analyze.FmtSlope(q.KSlope, q.KSlopeCI))
			}
			b.WriteString("\n")
			if q.ColdMs > 0 {
				fmt.Fprintf(&b, "- cold replay: %.2f ms (%.0f pages read from outside the buffer cache) vs warm %.2f ms\n", q.ColdMs, q.ColdReads, q.BaseMs)
			}
			for _, n := range q.Notes {
				if !strings.HasPrefix(n, "fk-child:") {
					fmt.Fprintf(&b, "- note: %s\n", n)
				}
			}
			if q.PlanFlip != "" {
				fmt.Fprintf(&b, "- plan flip: %s\n", q.PlanFlip)
			}
			for _, a := range q.Rejected {
				fmt.Fprintf(&b, "- withdrawn advice: `%s` — %s\n", a.SQL, a.Verified)
			}
			if len(q.Scale) > 0 {
				b.WriteString("\n| data | rows (dominant) | rows examined | time | plan |\n|---|---|---|---|---|\n")
				for _, p := range q.Scale {
					fmt.Fprintf(&b, "| %.0f%% | %.0f | %.0f | %.2f ms | %s |\n", p.Step*100, p.N, p.Work, p.Ms, p.Shape)
				}
			}
			if len(q.PerTable) > 0 {
				b.WriteString("\n| shrunk table (others at 100%) | rows | rows examined | slope (95% CI) |\n|---|---|---|---|\n")
				for _, g := range q.PerTable {
					for i, p := range g.Points {
						slope := ""
						if i == len(g.Points)-1 {
							slope = analyze.FmtSlope(g.Slope, g.CI)
						}
						fmt.Fprintf(&b, "| `%s` %.0f%% | %.0f | %.0f | %s |\n", g.Table, p.Step*100, p.N, p.Work, slope)
					}
				}
			}
			if len(q.KPoints) > 0 {
				b.WriteString("\n| k | count/req | rows examined | time |\n|---|---|---|---|\n")
				for _, p := range q.KPoints {
					fmt.Fprintf(&b, "| %d | %.0f | %.0f | %.2f ms |\n", p.K, p.Count, p.Work, p.Ms)
				}
			}
			if q.Base != nil {
				fmt.Fprintf(&b, "\n```\n%s```\n", q.Base.Text)
			}
			b.WriteString("\n</details>\n")
		}
	}
	if len(res.Unresolved) > 0 {
		b.WriteString("\n## Generated inputs (add to fixtures.yaml for realistic values)\n\n")
		ops := make([]string, 0, len(res.Unresolved))
		for op := range res.Unresolved {
			ops = append(ops, op)
		}
		sort.Strings(ops)
		for _, op := range ops {
			fmt.Fprintf(&b, "- `%s`: %s\n", op, strings.Join(res.Unresolved[op], ", "))
		}
	}
	if len(res.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	b.WriteString("\n---\nBig O is an empirical estimate: degree from rows-examined growth across nested 1%→100% data subsets (and one table at a time for multi-table queries), log factors from the plan, N+1 from queries-per-request growth with page size. Slopes are shown with their 95% confidence interval. App-only CPU work is visible only through the k-axis app-time fit.\n")
	return os.WriteFile(path, redacted(res, []byte(b.String())), 0o644)
}

// WriteAll writes results.json, report.md and (optionally) report.html.
func WriteAll(dir string, res *runner.Result, html bool) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	j, m := filepath.Join(dir, "results.json"), filepath.Join(dir, "report.md")
	if err := WriteJSON(j, res); err != nil {
		return nil, err
	}
	if err := WriteMarkdown(m, res); err != nil {
		return nil, err
	}
	files := []string{j, m}
	if html {
		h := filepath.Join(dir, "report.html")
		if err := WriteHTML(h, res); err != nil {
			return nil, err
		}
		files = append(files, h)
	}
	return files, nil
}
