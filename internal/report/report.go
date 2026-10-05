// Package report renders run results to the terminal, JSON and Markdown.
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

func Terminal(w io.Writer, res *runner.Result) {
	fmt.Fprintf(w, "\n%s  %s · %s %s · %.0fs\n\n", c("1", "routeperf"), res.SpecTitle, res.Dialect, res.DBVersion, res.Seconds)
	fmt.Fprintf(w, "%-7s %-34s %8s %8s %8s %-8s %10s %-40s %-6s %s\n", "METHOD", "ROUTE", "P50", "P95", "DB", "Q/REQ", "ROWS/REQ", "BIG O", "CONF", "STATUS")
	ops := append([]*analyze.OpResult(nil), res.Ops...)
	for _, o := range ops {
		reason := ""
		if len(o.Reasons) > 0 {
			reason = " " + c("90", trunc(o.Reasons[0], 48))
		}
		fmt.Fprintf(w, "%-7s %-34s %6.1fms %6.1fms %6.2fms %-8s %10s %-40s %-6s %s%s\n", o.Method, trunc(o.Path, 34), o.Latency.P50, o.Latency.P95, o.DBMs,
			trunc(o.QModel, 8), human(o.RowsPerReq), trunc(o.BigO, 40), o.Confidence, statusColor(o.Status), reason)
	}
	if len(res.Skipped) > 0 {
		fmt.Fprintf(w, "\n%s\n", c("90", fmt.Sprintf("skipped %d operation(s):", len(res.Skipped))))
		for _, s := range res.Skipped {
			fmt.Fprintf(w, "  %s %s %s\n", c("90", s.Method), s.Path, c("90", "— "+s.Skipped))
		}
	}
	// top findings
	var findings []string
	for _, o := range ops {
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
			findings = append(findings, line)
		}
	}
	if len(findings) > 0 {
		fmt.Fprintf(w, "\n%s\n%s\n", c("1", "Findings"), strings.Join(findings, "\n"))
	}
	if res.Snapshot != "none" {
		v := c("32", "verified")
		if !res.Verified {
			v = c("31", "NOT verified")
		}
		fmt.Fprintf(w, "\nDB restored (%s): %s — %s\n", res.Snapshot, strings.Join(res.Restored, ", "), v)
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

func WriteMarkdown(path string, res *runner.Result) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# routeperf report: %s\n\n", res.SpecTitle)
	fmt.Fprintf(&b, "- API: `%s`\n- Database: %s %s (`%s`)\n- Started: %s (%.0fs)\n- Data-scale steps: %v\n- Snapshot: %s",
		res.API, res.Dialect, res.DBVersion, res.Database, res.Started.Format("2006-01-02 15:04:05"), res.Seconds, res.DataSteps, res.Snapshot)
	if res.Snapshot != "none" {
		fmt.Fprintf(&b, " (restored: %s, verified: %v)", strings.Join(res.Restored, ", "), res.Verified)
	}
	b.WriteString("\n\n## Endpoints\n\n| Status | Method | Endpoint | p50 | p95 | p99 | DB time/req | Queries/req | Rows examined/req | Big O | Confidence | Projected p50 @10× data |\n|---|---|---|---|---|---|---|---|---|---|---|---|\n")
	for _, o := range res.Ops {
		proj := "—"
		if v, ok := o.Projection["10x"]; ok {
			proj = fmt.Sprintf("%.0f ms", v)
		}
		fmt.Fprintf(&b, "| %s | %s | `%s` | %.1f ms | %.1f ms | %.1f ms | %.1f ms | %s | %s | `%s` | %s | %s |\n", o.Status, o.Method, o.Path,
			o.Latency.P50, o.Latency.P95, o.Latency.P99, o.DBMs, o.QModel, human(o.RowsPerReq), o.BigO, o.Confidence, proj)
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
		fmt.Fprintf(&b, "\n- **Latency:** p50 %.1f ms · p95 %.1f ms · p99 %.1f ms (n=%d)\n", o.Latency.P50, o.Latency.P95, o.Latency.P99, o.Latency.N)
		fmt.Fprintf(&b, "- **DB time/request:** %.1f ms · **rows examined/request:** %s\n", o.DBMs, human(o.RowsPerReq))
		fmt.Fprintf(&b, "- **Queries/request:** %s", o.QModel)
		if len(o.NPlusOne) > 0 {
			fmt.Fprintf(&b, " — N+1: %s", strings.Join(o.NPlusOne, ", "))
		}
		b.WriteString("\n")
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
			if o.AppFit != nil && o.AppFit.OK {
				fmt.Fprintf(&b, "; app-side time fits %s (R² %.2f)", o.AppFit.ClassName, o.AppFit.R2)
			}
			b.WriteString("\n")
		}
		for _, r := range o.Reasons {
			fmt.Fprintf(&b, "- ⚠ %s\n", r)
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
				b.WriteString("\n")
			}
		}
		for _, q := range o.Queries {
			fmt.Fprintf(&b, "\n<details><summary><b>%s</b> ×%.0f/req · <code>%s</code> · %s</summary>\n\n", q.ID, q.CountPerReq, q.BigO, trunc(q.SQL, 90))
			fmt.Fprintf(&b, "```sql\n%s\n```\n\n", q.Example.SQL)
			if q.Error != "" {
				fmt.Fprintf(&b, "Replay error: `%s`\n\n", q.Error)
			}
			fmt.Fprintf(&b, "- static estimate `%s`, confidence %s", q.Static, q.Confidence)
			if q.WorkFit != nil && q.WorkFit.OK {
				fmt.Fprintf(&b, "; rows-examined fit %s (slope %.2f, R² %.3f, %.1f decades, %d points)", q.WorkFit.ClassName, q.WorkFit.Slope, q.WorkFit.R2, q.WorkFit.Decades, q.WorkFit.Points)
			}
			if q.TimeFit != nil && q.TimeFit.OK {
				fmt.Fprintf(&b, "; time fit %s", q.TimeFit.ClassName)
			}
			b.WriteString("\n")
			for _, n := range q.Notes {
				if !strings.HasPrefix(n, "fk-child:") {
					fmt.Fprintf(&b, "- note: %s\n", n)
				}
			}
			if q.PlanFlip != "" {
				fmt.Fprintf(&b, "- plan flip: %s\n", q.PlanFlip)
			}
			if len(q.Scale) > 0 {
				b.WriteString("\n| data | rows (dominant) | rows examined | time | plan |\n|---|---|---|---|---|\n")
				for _, p := range q.Scale {
					fmt.Fprintf(&b, "| %.0f%% | %.0f | %.0f | %.2f ms | %s |\n", p.Step*100, p.N, p.Work, p.Ms, p.Shape)
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
		for op, ps := range res.Unresolved {
			fmt.Fprintf(&b, "- `%s`: %s\n", op, strings.Join(ps, ", "))
		}
	}
	if len(res.Warnings) > 0 {
		b.WriteString("\n## Warnings\n\n")
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, "- %s\n", w)
		}
	}
	b.WriteString("\n---\nBig O is an empirical estimate: degree from rows-examined growth across nested 1%→100% data subsets, log factors from the plan, N+1 from queries-per-request growth with page size. App-only CPU work is visible only through the k-axis app-time fit.\n")
	return os.WriteFile(path, redacted(res, []byte(b.String())), 0o644)
}

func WriteAll(dir string, res *runner.Result) ([]string, error) {
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
	return []string{j, m}, nil
}
