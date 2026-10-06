# Regenerates the README images (docs/assets/*.svg): python3 scripts/gen-images.py
import html, math
import os
OUT = os.path.join(os.path.dirname(os.path.abspath(__file__)), "..", "docs", "assets") + os.sep
SANS = "-apple-system, BlinkMacSystemFont, 'Segoe UI', Inter, Helvetica, Arial, sans-serif"
MONO = "ui-monospace, SFMono-Regular, 'SF Mono', Menlo, Consolas, 'Liberation Mono', monospace"
C = dict(text="#e6edf3", muted="#8b949e", dim="#6e7681", cyan="#39d0f5", blue="#79c0ff", violet="#b39dfa",
         green="#3fb950", amber="#e3b341", red="#f85149", pink="#f778ba", white="#ffffff")
esc = lambda s: html.escape(s, quote=False)

def defs_common():
    return f'''<defs>
  <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1"><stop offset="0" stop-color="#0b1224"/><stop offset="1" stop-color="#170f36"/></linearGradient>
  <linearGradient id="accent" x1="0" y1="0" x2="1" y2="0"><stop offset="0" stop-color="{C['cyan']}"/><stop offset="1" stop-color="{C['violet']}"/></linearGradient>
  <radialGradient id="glow1" cx="0.85" cy="0.1" r="0.6"><stop offset="0" stop-color="#7c3aed" stop-opacity="0.35"/><stop offset="1" stop-color="#7c3aed" stop-opacity="0"/></radialGradient>
  <radialGradient id="glow2" cx="0.1" cy="1" r="0.6"><stop offset="0" stop-color="#06b6d4" stop-opacity="0.25"/><stop offset="1" stop-color="#06b6d4" stop-opacity="0"/></radialGradient>
  <pattern id="dots" width="22" height="22" patternUnits="userSpaceOnUse"><circle cx="1.5" cy="1.5" r="1.2" fill="#ffffff" fill-opacity="0.06"/></pattern>
</defs>'''

def card(w, h):
    return f'''<rect width="{w}" height="{h}" rx="24" fill="url(#bg)"/>
<rect width="{w}" height="{h}" rx="24" fill="url(#glow1)"/><rect width="{w}" height="{h}" rx="24" fill="url(#glow2)"/>
<rect width="{w}" height="{h}" rx="24" fill="url(#dots)"/>
<rect x="0.5" y="0.5" width="{w-1}" height="{h-1}" rx="23.5" fill="none" stroke="#ffffff" stroke-opacity="0.10"/>'''

# ---------------------------------------------------------------- banner
def banner():
    W, H = 1280, 400
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="routeperf — per-endpoint performance and Big O for your API">',
         defs_common(), card(W, H)]
    # logo mark
    lx, ly, ls = 72, 92, 116
    o.append(f'<rect x="{lx}" y="{ly}" width="{ls}" height="{ls}" rx="28" fill="#ffffff" fill-opacity="0.05" stroke="url(#accent)" stroke-width="2.5"/>')
    o.append(f'<polyline points="{lx+24},{ly+92} {lx+24},{ly+24}" stroke="{C["dim"]}" stroke-width="2" fill="none"/><polyline points="{lx+24},{ly+92} {lx+94},{ly+92}" stroke="{C["dim"]}" stroke-width="2" fill="none"/>')
    o.append(f'<path d="M{lx+28},{ly+86} L{lx+90},{ly+30}" stroke="{C["red"]}" stroke-width="5" stroke-linecap="round" fill="none"/>')
    o.append(f'<path d="M{lx+28},{ly+84} C{lx+40},{ly+70} {lx+56},{ly+67} {lx+90},{ly+64}" stroke="{C["green"]}" stroke-width="5" stroke-linecap="round" fill="none"/>')
    o.append(f'<circle cx="{lx+90}" cy="{ly+30}" r="6" fill="{C["red"]}"/><circle cx="{lx+90}" cy="{ly+64}" r="6" fill="{C["green"]}"/>')
    # text
    tx = 220
    o.append(f'<text x="{tx}" y="150" font-family="{SANS}" font-size="74" font-weight="800" fill="{C["white"]}" letter-spacing="-1.5">route<tspan fill="url(#accent)">perf</tspan></text>')
    o.append(f'<text x="{tx+2}" y="198" font-family="{SANS}" font-size="25" font-weight="600" fill="#d2dbe5">Per-endpoint performance &amp; Big O for your API</text>')
    o.append(f'<text x="{tx+2}" y="236" font-family="{SANS}" font-size="17" fill="{C["muted"]}">OpenAPI · GraphQL · gRPC → real requests → captured SQL → EXPLAIN ANALYZE</text>')
    o.append(f'<text x="{tx+2}" y="261" font-family="{SANS}" font-size="17" fill="{C["muted"]}">on 1% → 100% of your data → Big O ± CI, N+1 and proven index fixes</text>')
    chips = [("PostgreSQL", C["blue"]), ("MySQL", C["cyan"]), ("N+1 detection", C["amber"]), ("Index advice", C["green"]), ("Big O", C["violet"])]
    x = tx + 2
    for label, col in chips:
        w = int(len(label) * 8.6 + 30)
        o.append(f'<rect x="{x}" y="290" width="{w}" height="32" rx="16" fill="{col}" fill-opacity="0.12" stroke="{col}" stroke-opacity="0.55"/>')
        o.append(f'<text x="{x + w/2}" y="311" text-anchor="middle" font-family="{SANS}" font-size="14.5" font-weight="600" fill="{col}">{esc(label)}</text>')
        x += w + 10
    o.append(f'<text x="{tx+2}" y="356" font-family="{SANS}" font-size="14" fill="{C["dim"]}">macOS · Linux · Windows   ·   single binary   ·   curl | sh install</text>')
    # growth chart card
    cx, cy, cw, ch = 912, 70, 300, 262
    o.append(f'<rect x="{cx}" y="{cy}" width="{cw}" height="{ch}" rx="18" fill="#0d1117" fill-opacity="0.65" stroke="#ffffff" stroke-opacity="0.12"/>')
    o.append(f'<text x="{cx+20}" y="{cy+32}" font-family="{SANS}" font-size="13.5" font-weight="600" fill="{C["text"]}">rows examined as data grows</text>')
    px0, px1, py0, py1 = cx + 46, cx + cw - 22, cy + ch - 46, cy + 56
    for i in range(5):
        y = py0 - (py0 - py1) * i / 4
        o.append(f'<line x1="{px0}" y1="{y:.1f}" x2="{px1}" y2="{y:.1f}" stroke="#ffffff" stroke-opacity="0.06"/>')
    for i, lab in enumerate(["1", "10", "1k", "100k"]):
        y = py0 - (py0 - py1) * (i * 1.6) / 5.5 - 0
    for v, lab in [(0, "1"), (2, "100"), (4, "10k"), (5.3, "200k")]:
        y = py0 - (py0 - py1) * v / 5.5
        o.append(f'<text x="{px0-8}" y="{y+4:.1f}" text-anchor="end" font-family="{MONO}" font-size="10.5" fill="{C["dim"]}">{lab}</text>')
    steps = ["1%", "3%", "10%", "30%", "100%"]
    xs = [px0 + (px1 - px0) * i / 4 for i in range(5)]
    for x_, s in zip(xs, steps):
        o.append(f'<text x="{x_:.1f}" y="{py0+20}" text-anchor="middle" font-family="{MONO}" font-size="10.5" fill="{C["dim"]}">{s}</text>')
    def series(vals, col, label, dy):
        pts = [(x_, py0 - (py0 - py1) * math.log10(max(v, 1)) / 5.5) for x_, v in zip(xs, vals)]
        o.append(f'<polyline points="{" ".join(f"{a:.1f},{b:.1f}" for a, b in pts)}" fill="none" stroke="{col}" stroke-width="3" stroke-linecap="round" stroke-linejoin="round"/>')
        for a, b in pts:
            o.append(f'<circle cx="{a:.1f}" cy="{b:.1f}" r="4" fill="#0d1117" stroke="{col}" stroke-width="2.5"/>')
        a, b = pts[-1]
        o.append(f'<text x="{a-6:.1f}" y="{b+dy:.1f}" text-anchor="end" font-family="{MONO}" font-size="12" font-weight="700" fill="{col}">{esc(label)}</text>')
    series([1680, 5330, 19330, 59350, 200000], C["red"], "O(n) seq scan", -12)
    series([3, 3, 3, 3, 3], C["green"], "O(log n) index", -12)
    o.append('</svg>')
    open(OUT + "banner.svg", "w").write("\n".join(o))

# ---------------------------------------------------------------- terminal
def terminal():
    W = 1200
    lines = [
        [("$ ", C["green"]), ("routeperf run", C["text"])],
        [],
        [("Checking connections…", C["muted"])],
        [("  ✓ ", C["green"]), ("Spec                 ", C["text"]), ('"Shop API" OpenAPI 3.0.3 — 13 operations (8 read, 5 write)', C["muted"])],
        [("  ✓ ", C["green"]), ("DB connect           ", C["text"]), ('postgres 18.0, database "shop_dev" — 4 tables, 860k rows', C["muted"])],
        [("  ✓ ", C["green"]), ("SQL capture          ", C["text"]), ("statement log readable", C["muted"])],
        [("  ✓ ", C["green"]), ("App → DB link        ", C["text"]), ("15 SQL statements captured from 4 probe requests", C["muted"])],
        [],
        [("METHOD  ROUTE                        P50      P95      DB   Q/REQ  ROWS/REQ  BIG O                               CONF  STATUS", C["dim"])],
    ]
    rows = [
        ("GET   ", "/users/{id}/orders       ", "21.2ms", "22.6ms", "18.6ms", "k + 1", " 200k", "O(k·log n_order_items + n_orders)", "FAIL"),
        ("GET   ", "/orders/search           ", "22.0ms", "25.9ms", "20.7ms", "1    ", " 200k", "O(n_orders + k)", "FAIL"),
        ("GET   ", "/reports/sales           ", "32.5ms", "41.4ms", "29.9ms", "1    ", " 200k", "O(n_orders·log n_orders)", "FAIL"),
        ("DELETE", "/users/{id}              ", "26.2ms", "27.8ms", "24.8ms", "1    ", "    1", "O(n_orders)", "FAIL"),
        ("GET   ", "/users/{id}/recommend…   ", " 0.9ms", " 1.9ms", " 0.0ms", "1    ", "   20", "O(k^2)", "WARN"),
        ("POST  ", "/orders                  ", " 1.6ms", " 2.3ms", " 0.4ms", "k + 1", "    0", "O(k·log n_order_items)", "WARN"),
        ("GET   ", "/users/{id}              ", " 0.6ms", " 1.1ms", " 0.0ms", "1    ", "    1", "O(log n_users)", "OK"),
        ("GET   ", "/orders                  ", " 1.2ms", " 1.6ms", " 0.0ms", "1    ", "   20", "O(k)", "OK"),
    ]
    sc = {"FAIL": C["red"], "WARN": C["amber"], "OK": C["green"]}
    for m, r, p50, p95, db, q, rows_, bo, st in rows:
        qcol = C["amber"] if "k" in q else C["text"]
        lines.append([(m + "  ", C["blue"]), (r + "  ", C["text"]), (f"{p50}   {p95}  {db}   ", C["muted"]), (q + "  ", qcol),
                      (rows_ + "     ", C["muted"]), (bo.ljust(36), C["cyan"]), ("high  ", C["muted"]), (st, sc[st])])
    lines += [
        [],
        [("Findings", C["text"])],
        [("  FAIL ", C["red"]), ("GET /users/{id}/orders — seq scan on orders (200k rows examined) filtered by user_id", C["text"])],
        [("       CREATE INDEX ON orders (user_id, created_at DESC);", C["green"])],
        [("  FAIL ", C["red"]), ("GET /users/{id}/orders — N+1: query runs once per returned item; batch with JOIN / IN (...)", C["text"])],
        [("  FAIL ", C["red"]), ("DELETE /users/{id} — FK orders.user_id has no index; every DELETE on users scans orders", C["text"])],
        [("       CREATE INDEX ON orders (user_id);", C["green"])],
        [("  WARN ", C["amber"]), ("GET /users/{id}/recommendations — app-side time grows ~k²; inspect in-memory loops", C["text"])],
        [],
        [("DB restored: order_items, orders, users — ", C["muted"]), ("verified", C["green"]), ("    Reports: routeperf-out/report.md, results.json", C["muted"])],
    ]
    lh, top = 21, 74
    H = top + lh * len(lines) + 26
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="routeperf run output: per-endpoint latency, queries per request, Big O and fixes">',
         f'<defs><linearGradient id="tb" x1="0" y1="0" x2="0" y2="1"><stop offset="0" stop-color="#1b2130"/><stop offset="1" stop-color="#161b26"/></linearGradient></defs>',
         f'<rect width="{W}" height="{H}" rx="16" fill="#0d1117"/>',
         f'<rect x="0.5" y="0.5" width="{W-1}" height="{H-1}" rx="15.5" fill="none" stroke="#30363d"/>',
         f'<path d="M16 0.5 H{W-16} A15.5 15.5 0 0 1 {W-0.5} 16 V44 H0.5 V16 A15.5 15.5 0 0 1 16 0.5 Z" fill="url(#tb)"/>',
         f'<line x1="0" y1="44" x2="{W}" y2="44" stroke="#30363d"/>']
    for i, col in enumerate(["#ff5f57", "#febc2e", "#28c840"]):
        o.append(f'<circle cx="{24 + i*22}" cy="22" r="6.5" fill="{col}"/>')
    o.append(f'<text x="{W/2}" y="27" text-anchor="middle" font-family="{SANS}" font-size="13" fill="{C["muted"]}">routeperf — zsh</text>')
    for i, segs in enumerate(lines):
        y = top + i * lh
        if not segs:
            continue
        spans = "".join(f'<tspan fill="{col}">{esc(t)}</tspan>' for t, col in segs)
        o.append(f'<text x="28" y="{y}" font-family="{MONO}" font-size="13.2" xml:space="preserve">{spans}</text>')
    o.append('</svg>')
    open(OUT + "demo.svg", "w").write("\n".join(o))

# ---------------------------------------------------------------- how it works
def how():
    W, H = 1280, 300
    steps = [
        ("1", "Read the API", ["OpenAPI · GraphQL · gRPC", "every route, param, body", "and security scheme"], C["cyan"]),
        ("2", "Call every route", ["real requests with your", "token · cookies · headers", "writes run on a snapshot"], C["blue"]),
        ("3", "Capture the SQL", ["statement log or wire proxy", "queries/request, N+1,", "rows examined"], C["violet"]),
        ("4", "Replay at scale", ["EXPLAIN ANALYZE per", "subset and per table", ""], C["pink"]),
        ("5", "Big O + fixes", ["fixes proven with HypoPG", "HTML · Markdown · JSON", "CI diff exit codes"], C["green"]),
    ]
    o = [f'<svg xmlns="http://www.w3.org/2000/svg" width="{W}" height="{H}" viewBox="0 0 {W} {H}" role="img" aria-label="How routeperf works: read spec, call routes, capture SQL, replay at scale, Big O and fixes">',
         defs_common(), card(W, H)]
    cw, gap, x0, y0, chh = 214, 34, 40, 52, 196
    for i, (num, title, desc, col) in enumerate(steps):
        x = x0 + i * (cw + gap)
        o.append(f'<rect x="{x}" y="{y0}" width="{cw}" height="{chh}" rx="18" fill="#0d1117" fill-opacity="0.6" stroke="{col}" stroke-opacity="0.45"/>')
        o.append(f'<rect x="{x}" y="{y0}" width="{cw}" height="5" rx="2.5" fill="{col}" fill-opacity="0.9"/>')
        o.append(f'<circle cx="{x+34}" cy="{y0+44}" r="17" fill="{col}" fill-opacity="0.16" stroke="{col}"/>')
        o.append(f'<text x="{x+34}" y="{y0+50}" text-anchor="middle" font-family="{SANS}" font-size="16" font-weight="800" fill="{col}">{num}</text>')
        o.append(f'<text x="{x+62}" y="{y0+50}" font-family="{SANS}" font-size="17" font-weight="700" fill="{C["text"]}">{esc(title)}</text>')
        for j, d in enumerate(desc):
            if d:
                o.append(f'<text x="{x+20}" y="{y0+92 + j*22}" font-family="{SANS}" font-size="13.5" fill="{C["muted"]}">{esc(d)}</text>')
        if num == "4":  # mini bars for the data subsets
            for k, (lab, h) in enumerate([("1%", 8), ("3%", 14), ("10%", 24), ("30%", 38), ("100%", 56)]):
                bx = x + 22 + k * 37
                o.append(f'<rect x="{bx}" y="{y0+chh-30-h}" width="24" height="{h}" rx="4" fill="{col}" fill-opacity="{0.35 + k*0.13:.2f}"/>')
                o.append(f'<text x="{bx+12}" y="{y0+chh-14}" text-anchor="middle" font-family="{MONO}" font-size="10.5" fill="{C["dim"]}">{lab}</text>')
        if i < len(steps) - 1:
            ax = x + cw + 7
            o.append(f'<path d="M{ax},{y0+chh/2} h18 m-7,-7 l7,7 l-7,7" fill="none" stroke="{C["dim"]}" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"/>')
    o.append(f'<text x="{W/2}" y="{H-22}" text-anchor="middle" font-family="{SANS}" font-size="13.5" fill="{C["dim"]}">degree from rows-examined growth (with 95% CI) · log factors from the plan · N+1 and app-side cost from page-size sweeps</text>')
    o.append('</svg>')
    open(OUT + "how-it-works.svg", "w").write("\n".join(o))

banner(); terminal(); how()
print("ok")
