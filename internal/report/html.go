package report

import (
	"encoding/json"
	"fmt"
	"html"
	"math"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/nazishasghar/routeperf/internal/analyze"
	"github.com/nazishasghar/routeperf/internal/runner"
)

type pt struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type series struct {
	Name string `json:"name"`
	Pts  []pt   `json:"pts"`
}

type chartSpec struct {
	Title, XLabel, YLabel, XUnit, YUnit string
	LogX, LogY                          bool
	Series                              []series
}

const (
	cw, ch           = 560.0, 230.0
	padL, padR, padT = 58.0, 96.0, 14.0
	padB             = 40.0
)

func fmtNum(v float64) string {
	a := math.Abs(v)
	switch {
	case a >= 1e9:
		return trimZero(fmt.Sprintf("%.1f", v/1e9)) + "B"
	case a >= 1e6:
		return trimZero(fmt.Sprintf("%.1f", v/1e6)) + "M"
	case a >= 1e3:
		return trimZero(fmt.Sprintf("%.1f", v/1e3)) + "k"
	case a >= 10 || v == math.Trunc(v):
		return fmt.Sprintf("%.0f", v)
	case a >= 1:
		return trimZero(fmt.Sprintf("%.1f", v))
	}
	return trimZero(fmt.Sprintf("%.2f", v))
}

func trimZero(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	return s
}

type scale struct {
	lo, hi   float64
	log      bool
	from, to float64 // pixel range
}

func (s scale) at(v float64) float64 {
	a, b, x := s.lo, s.hi, v
	if s.log {
		a, b, x = math.Log10(a), math.Log10(b), math.Log10(math.Max(v, s.lo))
	}
	if b == a {
		return (s.from + s.to) / 2
	}
	return s.from + (x-a)/(b-a)*(s.to-s.from)
}

func (s scale) ticks() []float64 {
	var out []float64
	if s.log {
		for _, steps := range [][]float64{{1}, {1, 3}, {1, 2, 5}} { // fewest ticks that still give ≥2
			out = nil
			for e := math.Floor(math.Log10(s.lo)); e <= math.Ceil(math.Log10(s.hi)); e++ {
				for _, m := range steps {
					v := m * math.Pow(10, e)
					if v >= s.lo*0.999 && v <= s.hi*1.001 {
						out = append(out, v)
					}
				}
			}
			if len(out) >= 3 || len(out) >= 2 && len(steps) == 3 {
				return out
			}
		}
		return []float64{s.lo, s.hi}
	}
	span := s.hi - s.lo
	if span <= 0 {
		return []float64{s.lo}
	}
	step := math.Pow(10, math.Floor(math.Log10(span/4)))
	for _, m := range []float64{1, 2, 5, 10} {
		if span/(step*m) <= 5 {
			step *= m
			break
		}
	}
	for v := math.Ceil(s.lo/step) * step; v <= s.hi+step*1e-9; v += step {
		out = append(out, v)
	}
	return out
}

func newScale(vals []float64, log bool, from, to float64, zero bool) scale {
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, v := range vals {
		if log && v <= 0 {
			continue
		}
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if math.IsInf(lo, 0) {
		lo, hi = 1, 10
	}
	if log {
		lo, hi = lo/1.15, hi*1.15 // a little air, not whole decades
		if hi/lo < 3 {
			lo, hi = lo/1.5, hi*1.5
		}
	} else {
		if zero {
			lo = math.Min(0, lo)
		}
		if hi == lo {
			hi = lo + 1
		}
		hi += (hi - lo) * 0.05
	}
	return scale{lo: lo, hi: hi, log: log, from: from, to: to}
}

var chartN int

// svgChart renders a line chart as inline SVG; data rides along as JSON for
// the hover crosshair, and a table view sits under it.
func svgChart(c chartSpec) string {
	var xs, ys []float64
	var ss []series
	for _, s := range c.Series {
		var keep []pt
		for _, p := range s.Pts {
			if (c.LogX && p.X <= 0) || (c.LogY && p.Y <= 0) || math.IsNaN(p.Y) {
				continue
			}
			keep = append(keep, p)
			xs, ys = append(xs, p.X), append(ys, p.Y)
		}
		sort.Slice(keep, func(i, j int) bool { return keep[i].X < keep[j].X })
		if len(keep) > 0 {
			ss = append(ss, series{s.Name, keep})
		}
	}
	if len(ss) == 0 || len(ss) > 4 {
		return ""
	}
	chartN++
	sx := newScale(xs, c.LogX, padL, cw-padR, false)
	sy := newScale(ys, c.LogY, ch-padB, padT, true)
	var b strings.Builder
	data, _ := json.Marshal(map[string]any{"series": ss, "xu": c.XUnit, "yu": c.YUnit})
	fmt.Fprintf(&b, `<figure class="chart"><figcaption>%s</figcaption>`, html.EscapeString(c.Title))
	if len(ss) > 1 {
		b.WriteString(`<div class="legend">`)
		for i, s := range ss {
			fmt.Fprintf(&b, `<span><i class="key s%d"></i>%s</span>`, i+1, html.EscapeString(s.Name))
		}
		b.WriteString(`</div>`)
	}
	fmt.Fprintf(&b, `<div class="plot"><svg viewBox="0 0 %.0f %.0f" role="img" aria-label="%s" data-chart='%s' data-sx='%s' data-sy='%s'>`,
		cw, ch, html.EscapeString(c.Title), html.EscapeString(string(data)), scaleJSON(sx), scaleJSON(sy))
	for _, t := range sy.ticks() {
		y := sy.at(t)
		fmt.Fprintf(&b, `<line class="grid" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/><text class="tick" x="%.1f" y="%.1f" text-anchor="end">%s</text>`,
			padL, cw-padR, y, y, padL-8, y+4, fmtNum(t))
	}
	for _, t := range sx.ticks() {
		x := sx.at(t)
		fmt.Fprintf(&b, `<text class="tick" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, x, ch-padB+16, fmtNum(t))
	}
	fmt.Fprintf(&b, `<line class="axis" x1="%.1f" x2="%.1f" y1="%.1f" y2="%.1f"/>`, padL, cw-padR, ch-padB, ch-padB)
	fmt.Fprintf(&b, `<text class="label" x="%.1f" y="%.1f" text-anchor="middle">%s</text>`, (padL+cw-padR)/2, ch-4, html.EscapeString(c.XLabel))
	fmt.Fprintf(&b, `<text class="label" transform="translate(12 %.1f) rotate(-90)" text-anchor="middle">%s</text>`, (padT+ch-padB)/2, html.EscapeString(c.YLabel))
	for i, s := range ss {
		var d strings.Builder
		for j, p := range s.Pts {
			cmd := "L"
			if j == 0 {
				cmd = "M"
			}
			fmt.Fprintf(&d, "%s%.1f %.1f ", cmd, sx.at(p.X), sy.at(p.Y))
		}
		fmt.Fprintf(&b, `<path class="line s%d" d="%s"/>`, i+1, strings.TrimSpace(d.String()))
		last := s.Pts[len(s.Pts)-1]
		fmt.Fprintf(&b, `<circle class="dot s%d" cx="%.1f" cy="%.1f" r="4"/>`, i+1, sx.at(last.X), sy.at(last.Y))
	}
	// direct end labels unless they would collide
	type lab struct {
		y    float64
		text string
	}
	var labs []lab
	for _, s := range ss {
		last := s.Pts[len(s.Pts)-1]
		labs = append(labs, lab{sy.at(last.Y), fmtNum(last.Y) + c.YUnit})
	}
	collide := false
	for i := range labs {
		for j := i + 1; j < len(labs); j++ {
			if math.Abs(labs[i].y-labs[j].y) < 13 {
				collide = true
			}
		}
	}
	if !collide {
		for _, l := range labs {
			fmt.Fprintf(&b, `<text class="end" x="%.1f" y="%.1f">%s</text>`, cw-padR+8, l.y+4, html.EscapeString(l.text))
		}
	}
	fmt.Fprintf(&b, `<line class="cross" x1="0" x2="0" y1="%.1f" y2="%.1f"/><rect class="hit" x="%.1f" y="%.1f" width="%.1f" height="%.1f" tabindex="0"/>`,
		padT, ch-padB, padL, padT, cw-padR-padL, ch-padB-padT)
	b.WriteString(`</svg><div class="tip" hidden></div></div>`)
	// table view
	b.WriteString(`<details class="tv"><summary>Table</summary><table><thead><tr><th>series</th><th>` + html.EscapeString(c.XLabel) + `</th><th>` + html.EscapeString(c.YLabel) + `</th></tr></thead><tbody>`)
	for _, s := range ss {
		for _, p := range s.Pts {
			fmt.Fprintf(&b, `<tr><td>%s</td><td class="num">%s</td><td class="num">%s</td></tr>`, html.EscapeString(s.Name), fmtNum(p.X), fmtNum(p.Y))
		}
	}
	b.WriteString(`</tbody></table></details></figure>`)
	return b.String()
}

func scaleJSON(s scale) string {
	b, _ := json.Marshal(map[string]any{"lo": s.lo, "hi": s.hi, "log": s.log, "from": s.from, "to": s.to})
	return html.EscapeString(string(b))
}

var tagComment = regexp.MustCompile(`\s*/\*[^*]*traceparent[^*]*\*/`)

// stripTags drops sqlcommenter comments from SQL shown in summaries.
func stripTags(s string) string { return tagComment.ReplaceAllString(s, "") }

func statusChip(s string) string {
	icon := map[string]string{"OK": "✓", "WARN": "▲", "FAIL": "✕"}[s]
	if icon == "" {
		icon = "–"
	}
	return fmt.Sprintf(`<span class="chip %s"><i></i>%s %s</span>`, strings.ToLower(s), icon, html.EscapeString(s))
}

func esc(s string) string { return html.EscapeString(s) }

func opCharts(o *analyze.OpResult) string {
	var b strings.Builder
	if len(o.KLatency) >= 2 {
		var p []pt
		for k, v := range o.KLatency {
			p = append(p, pt{float64(k), v})
		}
		b.WriteString(svgChart(chartSpec{Title: fmt.Sprintf("Latency vs output size (%s)", o.KParam), XLabel: o.KParam + " (k)", YLabel: "p50 latency (ms)",
			YUnit: " ms", LogX: true, LogY: true, Series: []series{{"p50 latency", p}}}))
	}
	if len(o.Load) >= 2 {
		var rps, p95 []pt
		for _, l := range o.Load {
			rps = append(rps, pt{float64(l.Concurrency), l.RPS})
			p95 = append(p95, pt{float64(l.Concurrency), l.P95})
		}
		b.WriteString(svgChart(chartSpec{Title: "Throughput vs concurrency", XLabel: "concurrent clients", YLabel: "requests / s", YUnit: " rps", LogX: true, Series: []series{{"throughput", rps}}}))
		b.WriteString(svgChart(chartSpec{Title: "p95 latency vs concurrency", XLabel: "concurrent clients", YLabel: "p95 (ms)", YUnit: " ms", LogX: true, Series: []series{{"p95", p95}}}))
	}
	if b.Len() == 0 {
		return ""
	}
	return `<div class="charts">` + b.String() + `</div>`
}

func queryChart(q *analyze.QueryResult) string {
	var ss []series
	if len(q.Scale) >= 2 {
		var p []pt
		for _, s := range q.Scale {
			p = append(p, pt{s.N, math.Max(s.Work, 1)})
		}
		name := "all tables shrink together"
		if q.Dominant != "" {
			name += " (x = " + q.Dominant + ")"
		}
		ss = append(ss, series{name, p})
	}
	for _, g := range q.PerTable {
		if len(ss) == 4 {
			break
		}
		var p []pt
		for _, s := range g.Points {
			p = append(p, pt{s.N, math.Max(s.Work, 1)})
		}
		ss = append(ss, series{"only " + g.Table + " shrinks", p})
	}
	if len(ss) == 0 {
		return ""
	}
	return `<div class="charts">` + svgChart(chartSpec{Title: "Rows examined vs table size", XLabel: "rows in the shrunk table", YLabel: "rows examined", LogX: true, LogY: true, Series: ss}) + `</div>`
}

// WriteHTML writes a single self-contained, shareable report.
func WriteHTML(path string, res *runner.Result) error {
	chartN = 0
	var b strings.Builder
	counts := map[string]int{}
	for _, o := range res.Ops {
		counts[o.Status]++
	}
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>routeperf · %s</title><style>%s</style></head><body><main>`,
		esc(res.SpecTitle), htmlCSS)
	fmt.Fprintf(&b, `<header><p class="eyebrow">routeperf report</p><h1>%s</h1><ul class="meta">`, esc(res.SpecTitle))
	for _, l := range headerLines(res) {
		fmt.Fprintf(&b, `<li>%s</li>`, esc(strings.ReplaceAll(l, "`", "")))
	}
	b.WriteString(`</ul></header>`)
	fmt.Fprintf(&b, `<section class="tiles"><div class="tile"><span>Endpoints measured</span><strong>%d</strong></div><div class="tile"><span>%s</span><strong>%d</strong></div><div class="tile"><span>%s</span><strong>%d</strong></div><div class="tile"><span>%s</span><strong>%d</strong></div></section>`,
		len(res.Ops), statusChip("FAIL"), counts["FAIL"], statusChip("WARN"), counts["WARN"], statusChip("OK"), counts["OK"])
	b.WriteString(`<div class="filters"><input id="q" type="search" placeholder="Filter endpoints…" aria-label="Filter endpoints"><div class="seg" role="group" aria-label="Status">`)
	for _, s := range []string{"All", "FAIL", "WARN", "OK"} {
		pressed := "false"
		if s == "All" {
			pressed = "true"
		}
		fmt.Fprintf(&b, `<button type="button" data-status="%s" aria-pressed="%s">%s</button>`, s, pressed, s)
	}
	b.WriteString(`</div></div>`)
	cold := hasCold(res)
	b.WriteString(`<div class="tablewrap"><table id="ops"><thead><tr><th data-k="s">Status</th><th data-k="t">Endpoint</th><th data-k="n" class="num">p50 ms</th><th data-k="n" class="num">p95 ms</th>`)
	if cold {
		b.WriteString(`<th data-k="n" class="num">Cold p50 ms</th>`)
	}
	b.WriteString(`<th data-k="n" class="num">DB ms/req</th><th data-k="t">Queries/req</th><th data-k="n" class="num">Rows exam./req</th><th data-k="t">Big O</th><th data-k="t">Conf.</th></tr></thead><tbody>`)
	rank := map[string]int{"FAIL": 0, "WARN": 1, "OK": 2}
	for i, o := range res.Ops {
		fmt.Fprintf(&b, `<tr data-status="%s"><td data-v="%d">%s</td><td data-v="%s"><a href="#op%d"><b>%s</b> %s</a></td><td class="num" data-v="%f">%.1f</td><td class="num" data-v="%f">%.1f</td>`,
			esc(o.Status), rank[o.Status], statusChip(o.Status), esc(o.Path), i, esc(o.Method), esc(o.Path), o.Latency.P50, o.Latency.P50, o.Latency.P95, o.Latency.P95)
		if cold {
			if o.Cold != nil {
				fmt.Fprintf(&b, `<td class="num" data-v="%f">%.1f</td>`, o.Cold.P50, o.Cold.P50)
			} else {
				b.WriteString(`<td class="num" data-v="-1">—</td>`)
			}
		}
		fmt.Fprintf(&b, `<td class="num" data-v="%f">%.2f</td><td data-v="%s">%s</td><td class="num" data-v="%f">%s</td><td data-v="%s"><code>%s</code></td><td data-v="%s">%s</td></tr>`,
			o.DBMs, o.DBMs, esc(o.QModel), esc(o.QModel), o.RowsPerReq, human(o.RowsPerReq), esc(o.BigO), esc(o.BigO), esc(o.Confidence), esc(o.Confidence))
	}
	b.WriteString(`</tbody></table></div>`)
	b.WriteString(`<section class="details">`)
	for i, o := range res.Ops {
		fmt.Fprintf(&b, `<details class="op" id="op%d" data-status="%s"><summary>%s <b>%s</b> <span class="path">%s</span> <code>%s</code></summary><div class="body">`,
			i, esc(o.Status), statusChip(o.Status), esc(o.Method), esc(o.Path), esc(o.BigO))
		b.WriteString(`<dl class="facts">`)
		fact := func(k, v string) { fmt.Fprintf(&b, `<div><dt>%s</dt><dd>%s</dd></div>`, esc(k), v) }
		fact("Latency (warm)", fmt.Sprintf("p50 %.1f · p95 %.1f · p99 %.1f ms (n=%d)", o.Latency.P50, o.Latency.P95, o.Latency.P99, o.Latency.N))
		if o.Cold != nil {
			fact("Latency (cold first hit)", fmt.Sprintf("p50 %.1f · p95 %.1f ms (n=%d)", o.Cold.P50, o.Cold.P95, o.Cold.N))
		}
		fact("DB time / request", fmt.Sprintf("%.2f ms", o.DBMs))
		fact("Queries / request", esc(o.QModel))
		fact("Rows examined / request", human(o.RowsPerReq))
		fact("Big O", fmt.Sprintf("<code>%s</code> (%s confidence)", esc(o.BigO), esc(o.Confidence)))
		if o.AppSlope > 0 {
			fact("App-side growth with k", "slope "+esc(analyze.FmtSlope(o.AppSlope, o.AppSlopeCI)))
		}
		if v, ok := o.Projection["10x"]; ok {
			fact("Projected p50 at 10× data", fmt.Sprintf("%.0f ms", v))
		}
		b.WriteString(`</dl>`)
		if len(o.Reasons) > 0 {
			b.WriteString(`<ul class="reasons">`)
			for _, r := range o.Reasons {
				fmt.Fprintf(&b, `<li>%s</li>`, esc(r))
			}
			b.WriteString(`</ul>`)
		}
		if len(o.Advice) > 0 {
			b.WriteString(`<h3>What to fix</h3><ul class="advice">`)
			for _, a := range o.Advice {
				fmt.Fprintf(&b, `<li><span class="rule">%s</span> %s`, esc(a.Rule), esc(a.Message))
				if a.SQL != "" {
					fmt.Fprintf(&b, `<div class="sql"><code>%s</code><button type="button" class="copy" data-copy="%s">Copy</button></div>`, esc(a.SQL), esc(a.SQL))
				}
				if a.Verified != "" {
					fmt.Fprintf(&b, `<p class="proof">✓ %s</p>`, esc(a.Verified))
				}
				b.WriteString(`</li>`)
			}
			b.WriteString(`</ul>`)
		}
		if o.LoadNote != "" {
			fmt.Fprintf(&b, `<h3>Under load</h3><p>%s</p>`, esc(o.LoadNote))
		}
		b.WriteString(opCharts(o))
		for _, q := range o.Queries {
			fmt.Fprintf(&b, `<details class="q"><summary><b>%s</b> ×%.0f/req <code>%s</code> <span class="muted">%s</span></summary><div class="body">`, esc(q.ID), q.CountPerReq, esc(q.BigO), esc(trunc(stripTags(q.SQL), 110)))
			fmt.Fprintf(&b, `<pre class="sqltext">%s</pre>`, esc(q.Example.SQL))
			if q.Error != "" {
				fmt.Fprintf(&b, `<p class="err">Replay error: %s</p>`, esc(q.Error))
			}
			var facts []string
			if q.WorkFit != nil && q.WorkFit.OK {
				facts = append(facts, fmt.Sprintf("rows-examined fit %s, slope %s, R² %.3f", q.WorkFit.ClassName, analyze.FmtSlope(q.WorkFit.Slope, q.WorkFit.SlopeCI), q.WorkFit.R2))
			}
			if q.TimeFit != nil && q.TimeFit.OK {
				facts = append(facts, fmt.Sprintf("time fit %s, slope %s", q.TimeFit.ClassName, analyze.FmtSlope(q.TimeFit.Slope, q.TimeFit.SlopeCI)))
			}
			if q.ColdMs > 0 {
				facts = append(facts, fmt.Sprintf("cold %.2f ms (%.0f pages read) vs warm %.2f ms", q.ColdMs, q.ColdReads, q.BaseMs))
			}
			for _, n := range q.Notes {
				if !strings.HasPrefix(n, "fk-child:") {
					facts = append(facts, n)
				}
			}
			for _, a := range q.Rejected {
				facts = append(facts, "withdrawn: "+a.SQL+" — "+a.Verified)
			}
			if len(facts) > 0 {
				b.WriteString(`<ul class="notes">`)
				for _, f := range facts {
					fmt.Fprintf(&b, `<li>%s</li>`, esc(f))
				}
				b.WriteString(`</ul>`)
			}
			b.WriteString(queryChart(q))
			if q.Base != nil {
				fmt.Fprintf(&b, `<details class="plan"><summary>Plan at 100%% data</summary><pre>%s</pre></details>`, esc(q.Base.Text))
			}
			b.WriteString(`</div></details>`)
		}
		b.WriteString(`</div></details>`)
	}
	b.WriteString(`</section>`)
	if len(res.Skipped) > 0 {
		b.WriteString(`<section><h2>Skipped</h2><ul>`)
		for _, s := range res.Skipped {
			fmt.Fprintf(&b, `<li><code>%s %s</code> — %s</li>`, esc(s.Method), esc(s.Path), esc(s.Skipped))
		}
		b.WriteString(`</ul></section>`)
	}
	if len(res.Warnings) > 0 {
		b.WriteString(`<section><h2>Warnings</h2><ul>`)
		for _, w := range res.Warnings {
			fmt.Fprintf(&b, `<li>%s</li>`, esc(w))
		}
		b.WriteString(`</ul></section>`)
	}
	b.WriteString(`<footer>Big O is an empirical estimate: degree from rows-examined growth across nested 1%→100% data subsets (and one table at a time for multi-table queries), log factors from the plan, N+1 from queries per request growing with page size. Slopes carry their 95% confidence interval.</footer>`)
	b.WriteString(`</main><script>` + htmlJS + `</script></body></html>`)
	return os.WriteFile(path, redacted(res, []byte(b.String())), 0o644)
}

const htmlCSS = `
:root{color-scheme:light;--page:#f9f9f7;--surface:#fcfcfb;--ink:#0b0b0b;--ink2:#52514e;--muted:#898781;--grid:#e1e0d9;--axis:#c3c2b7;--ring:rgba(11,11,11,.10);
--s1:#2a78d6;--s2:#eb6834;--s3:#1baf7a;--s4:#eda100;--good:#0ca30c;--warn:#fab219;--crit:#d03b3b;--code:#f0efec}
@media (prefers-color-scheme:dark){:root:not([data-theme="light"]){color-scheme:dark;--page:#0d0d0d;--surface:#1a1a19;--ink:#fff;--ink2:#c3c2b7;--muted:#898781;--grid:#2c2c2a;--axis:#383835;--ring:rgba(255,255,255,.10);
--s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--code:#262624}}
:root[data-theme="dark"]{color-scheme:dark;--page:#0d0d0d;--surface:#1a1a19;--ink:#fff;--ink2:#c3c2b7;--muted:#898781;--grid:#2c2c2a;--axis:#383835;--ring:rgba(255,255,255,.10);
--s1:#3987e5;--s2:#d95926;--s3:#199e70;--s4:#c98500;--code:#262624}
*{box-sizing:border-box}html{-webkit-text-size-adjust:100%}
body{margin:0;background:var(--page);color:var(--ink);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:1180px;margin:0 auto;padding:24px 16px 48px}
h1{font-size:26px;margin:2px 0 8px;font-weight:650}h2{font-size:18px;margin:28px 0 8px}h3{font-size:14px;margin:16px 0 6px}
.eyebrow{margin:0;color:var(--muted);text-transform:uppercase;letter-spacing:.08em;font-size:12px}
.meta{list-style:none;padding:0;margin:0;display:flex;flex-wrap:wrap;gap:4px 18px;color:var(--ink2);font-size:13px}
code,pre{font:12.5px/1.45 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
code{background:var(--code);padding:1px 5px;border-radius:4px}
pre{background:var(--code);padding:10px 12px;border-radius:8px;overflow:auto;white-space:pre}
.tiles{display:grid;grid-template-columns:repeat(auto-fit,minmax(160px,1fr));gap:12px;margin:20px 0}
.tile{background:var(--surface);border:1px solid var(--ring);border-radius:10px;padding:12px 14px}
.tile span{display:block;color:var(--ink2);font-size:13px}.tile strong{font-size:28px;font-weight:600}
.chip{display:inline-flex;align-items:center;gap:6px;font-weight:600;font-size:12px;white-space:nowrap}
.chip i{width:8px;height:8px;border-radius:50%;background:var(--muted)}
.chip.ok i{background:var(--good)}.chip.warn i{background:var(--warn)}.chip.fail i{background:var(--crit)}
.filters{display:flex;flex-wrap:wrap;gap:10px;align-items:center;margin:8px 0 12px}
.filters input{flex:1 1 220px;min-width:0;padding:8px 10px;border-radius:8px;border:1px solid var(--axis);background:var(--surface);color:var(--ink);font:inherit}
.seg{display:inline-flex;border:1px solid var(--axis);border-radius:8px;overflow:hidden}
.seg button{border:0;background:var(--surface);color:var(--ink2);padding:7px 12px;font:inherit;cursor:pointer}
.seg button[aria-pressed="true"]{background:var(--code);color:var(--ink);font-weight:600}
.tablewrap{overflow-x:auto;background:var(--surface);border:1px solid var(--ring);border-radius:10px}
table{border-collapse:collapse;width:100%}th,td{padding:7px 10px;border-bottom:1px solid var(--grid);text-align:left;vertical-align:top}
th{font-size:12px;color:var(--ink2);font-weight:600;cursor:pointer;white-space:nowrap;user-select:none}
th[aria-sort="ascending"]::after{content:" ▲"}th[aria-sort="descending"]::after{content:" ▼"}
td.num,th.num{text-align:right;font-variant-numeric:tabular-nums}
#ops a{color:inherit;text-decoration:none}#ops a:hover{text-decoration:underline}
#ops td:nth-child(2){min-width:200px}
.details{margin-top:24px;display:grid;gap:10px}
details.op{background:var(--surface);border:1px solid var(--ring);border-radius:10px}
details.op>summary{padding:10px 14px;cursor:pointer;display:flex;flex-wrap:wrap;gap:8px;align-items:center}
details.op .path{font-weight:500}.body{padding:2px 14px 14px}
.facts{display:grid;grid-template-columns:repeat(auto-fit,minmax(210px,1fr));gap:8px 18px;margin:6px 0}
.facts dt{color:var(--ink2);font-size:12px}.facts dd{margin:0}
.reasons{color:var(--ink2)}.advice{padding-left:18px}.advice li{margin:8px 0}
.rule{font:600 12px ui-monospace,Menlo,monospace;color:var(--ink2)}
.sql{display:flex;gap:8px;align-items:flex-start;margin-top:4px;flex-wrap:wrap}.sql code{white-space:pre-wrap;word-break:break-word}
.copy{border:1px solid var(--axis);background:var(--surface);color:var(--ink2);border-radius:6px;padding:2px 8px;font:inherit;font-size:12px;cursor:pointer}
.proof{margin:4px 0 0;color:var(--ink2)}.proof::first-letter{color:var(--good)}
details.q{border-top:1px solid var(--grid);margin-top:8px}details.q>summary{padding:8px 0;cursor:pointer}
.muted{color:var(--muted)}.err{color:var(--crit)}.notes{color:var(--ink2);padding-left:18px}
.sqltext{white-space:pre-wrap;word-break:break-word}
.charts{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,560px));gap:14px;margin:10px 0}
figure.chart{margin:0;background:var(--surface);border:1px solid var(--ring);border-radius:10px;padding:10px 12px}
figcaption{font-weight:600;font-size:13px}
.legend{display:flex;flex-wrap:wrap;gap:4px 14px;font-size:12px;color:var(--ink2);margin:4px 0}
.legend span{display:inline-flex;align-items:center;gap:6px}.key{display:inline-block;width:14px;height:2px;border-radius:1px}
.key.s1{background:var(--s1)}.key.s2{background:var(--s2)}.key.s3{background:var(--s3)}.key.s4{background:var(--s4)}
.plot{position:relative}svg{width:100%;height:auto;display:block;overflow:visible}
.grid{stroke:var(--grid);stroke-width:1}.axis{stroke:var(--axis);stroke-width:1}
.tick,.end,.label{fill:var(--muted);font-size:11px;font-variant-numeric:tabular-nums}.end{fill:var(--ink2)}
.line{fill:none;stroke-width:2;stroke-linejoin:round;stroke-linecap:round}
.line.s1{stroke:var(--s1)}.line.s2{stroke:var(--s2)}.line.s3{stroke:var(--s3)}.line.s4{stroke:var(--s4)}
.dot{stroke:var(--surface);stroke-width:2}.dot.s1{fill:var(--s1)}.dot.s2{fill:var(--s2)}.dot.s3{fill:var(--s3)}.dot.s4{fill:var(--s4)}
.cross{stroke:var(--axis);stroke-width:1;visibility:hidden}.hit{fill:transparent;cursor:crosshair}.hit:focus{outline:none}
.tip{position:absolute;pointer-events:none;background:var(--surface);border:1px solid var(--ring);border-radius:8px;padding:6px 8px;font-size:12px;box-shadow:0 4px 14px rgba(0,0,0,.12);min-width:120px}
.tip .x{color:var(--muted)}.tip .row{display:flex;align-items:center;gap:6px}.tip .row b{font-weight:600}
.tv summary{cursor:pointer;color:var(--ink2);font-size:12px;margin-top:4px}.tv table{font-size:12px}
details.plan summary{cursor:pointer;color:var(--ink2);font-size:12px;margin:6px 0}
footer{margin-top:32px;color:var(--muted);font-size:12px}
@media (max-width:640px){main{padding:16px}h1{font-size:22px}}
`

const htmlJS = `
(function(){
var rows=[].slice.call(document.querySelectorAll('#ops tbody tr'));var status='All';var q=document.getElementById('q');
function apply(){var t=q.value.toLowerCase();rows.forEach(function(r){var ok=(status==='All'||r.dataset.status===status)&&r.textContent.toLowerCase().indexOf(t)>=0;r.hidden=!ok});
document.querySelectorAll('details.op').forEach(function(d){d.hidden=!(status==='All'||d.dataset.status===status)})}
q.addEventListener('input',apply);
document.querySelectorAll('.seg button').forEach(function(b){b.addEventListener('click',function(){status=b.dataset.status;document.querySelectorAll('.seg button').forEach(function(x){x.setAttribute('aria-pressed',x===b)});apply()})});
document.querySelectorAll('#ops th').forEach(function(th,i){th.addEventListener('click',function(){var dir=th.getAttribute('aria-sort')==='ascending'?-1:1;
document.querySelectorAll('#ops th').forEach(function(x){x.removeAttribute('aria-sort')});th.setAttribute('aria-sort',dir>0?'ascending':'descending');
var num=th.dataset.k!=='t';var tb=document.querySelector('#ops tbody');rows.sort(function(a,b){var x=a.children[i].dataset.v,y=b.children[i].dataset.v;
if(num){x=parseFloat(x);y=parseFloat(y);return (x-y)*dir}return x<y?-dir:x>y?dir:0});rows.forEach(function(r){tb.appendChild(r)})})});
document.querySelectorAll('.copy').forEach(function(b){b.addEventListener('click',function(){if(navigator.clipboard){navigator.clipboard.writeText(b.dataset.copy).then(function(){b.textContent='Copied';setTimeout(function(){b.textContent='Copy'},1200)})}})});
function inv(s,px){var a=s.lo,b=s.hi;if(s.log){a=Math.log10(a);b=Math.log10(b)}var v=a+(px-s.from)/(s.to-s.from)*(b-a);return s.log?Math.pow(10,v):v}
function at(s,v){var a=s.lo,b=s.hi,x=v;if(s.log){a=Math.log10(a);b=Math.log10(b);x=Math.log10(Math.max(v,s.lo))}return s.from+(x-a)/(b-a)*(s.to-s.from)}
function fmt(v){var a=Math.abs(v);if(a>=1e6)return (v/1e6).toFixed(1).replace(/\.0$/,'')+'M';if(a>=1e3)return (v/1e3).toFixed(1).replace(/\.0$/,'')+'k';if(a>=10||v===Math.round(v))return v.toFixed(0);if(a>=1)return v.toFixed(1);return v.toFixed(2)}
document.querySelectorAll('svg[data-chart]').forEach(function(svg){var d=JSON.parse(svg.dataset.chart),sx=JSON.parse(svg.dataset.sx),sy=JSON.parse(svg.dataset.sy);
var hit=svg.querySelector('.hit'),cross=svg.querySelector('.cross'),tip=svg.parentNode.querySelector('.tip');var xs=[];
d.series.forEach(function(s){s.pts.forEach(function(p){if(xs.indexOf(p.x)<0)xs.push(p.x)})});xs.sort(function(a,b){return a-b});var cur=-1;
function show(i){if(i<0||i>=xs.length)return;cur=i;var x=xs[i],px=at(sx,x);cross.setAttribute('x1',px);cross.setAttribute('x2',px);cross.style.visibility='visible';
while(tip.firstChild)tip.removeChild(tip.firstChild);var h=document.createElement('div');h.className='x';h.textContent='x = '+fmt(x)+(d.xu||'');tip.appendChild(h);
d.series.forEach(function(s,si){var best=null;s.pts.forEach(function(p){if(p.x===x)best=p});if(!best)return;var r=document.createElement('div');r.className='row';
var k=document.createElement('i');k.className='key s'+(si+1);var v=document.createElement('b');v.textContent=fmt(best.y)+(d.yu||'');var n=document.createElement('span');n.textContent=s.name;
r.appendChild(k);r.appendChild(v);r.appendChild(n);tip.appendChild(r)});tip.hidden=false;var box=svg.getBoundingClientRect(),scale=box.width/` + "560" + `;
var left=px*scale+12;if(left+tip.offsetWidth>box.width)left=px*scale-tip.offsetWidth-12;tip.style.left=Math.max(0,left)+'px';tip.style.top='8px'}
function hide(){cross.style.visibility='hidden';tip.hidden=true;cur=-1}
hit.addEventListener('pointermove',function(e){var box=svg.getBoundingClientRect(),px=(e.clientX-box.left)*` + "560" + `/box.width,x=inv(sx,px),best=0,bd=Infinity;
xs.forEach(function(v,i){var dd=Math.abs(at(sx,v)-px);if(dd<bd){bd=dd;best=i}});show(best)});
hit.addEventListener('pointerleave',hide);hit.addEventListener('blur',hide);hit.addEventListener('focus',function(){show(0)});
hit.addEventListener('keydown',function(e){if(e.key==='ArrowRight'){show(Math.min(xs.length-1,cur+1));e.preventDefault()}if(e.key==='ArrowLeft'){show(Math.max(0,cur-1));e.preventDefault()}if(e.key==='Escape')hide()})});
if(location.hash){var el=document.querySelector(location.hash);if(el&&el.tagName==='DETAILS')el.open=true}
document.querySelectorAll('#ops a').forEach(function(a){a.addEventListener('click',function(){var el=document.querySelector(a.getAttribute('href'));if(el)el.open=true})});
})();
`
