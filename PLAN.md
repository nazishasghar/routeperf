# routeperf: Plan

> A CLI that reads an OpenAPI/Swagger spec and calls every route on a running API that uses a **local Postgres or MySQL** database loaded with bulk test data. It captures the SQL each route runs, replays that SQL with `EXPLAIN ANALYZE`, and reports latency, query plans, and an **estimated Big O per route** with a confidence level.

Status: plan · Date: 2026-10-05

---

## 0. Summary

| Topic | Decision |
|---|---|
| User inputs | swagger.json URL, API base URL, DB URL (local), auth (bearer token, cookies, custom headers, OAuth2 client-credentials, login flow), optional fixtures |
| Databases | Postgres 13+ and MySQL 8.0.18+ |
| HTTP methods | All. Writes are made safe by snapshot → run → restore |
| SQL capture | Server statement log on the local DB, so no change to the app. A wire proxy is a later add-on |
| Big O method | Three independent signals combined (see §7): **A** plan-tree algebra, **B** data-scale experiments on nested 1%→100% subsets, **C** output-scale experiments over HTTP (`limit`, page size, bulk array length) |
| How the class is chosen | The *degree* comes from deterministic work counters (rows examined). *Log factors* come from the plan. *Time* is used for cross-checks and projections |
| Language | **Go**: single binary, fastest runtime, and mature libraries for every piece |

---

## 1. Goals and non-goals

**Goals**
- One command produces, for each route: p50/p95/p99 latency, queries per request, the plan of each query, a Big O estimate with confidence, a projection at 10× and 100× data, and concrete fixes.
- Make no changes to the application under test.
- Leave the DB in the same state before and after a run.
- Support CI: JSON output, thresholds, and a diff against a baseline.

**Non-goals (v1)**
- Load or concurrency testing. That is a separate mode, planned for later.
- Remote or production databases. Non-local DB hosts are refused by default.
- *Proving* complexity. The output is an empirical estimate with evidence and a confidence level.
- Profiling app code that never touches the DB. This is visible only through the black-box "app time" fit (§7.5).

---

## 2. Inputs

### 2.1 CLI

```bash
routeperf run \
  --spec     http://localhost:3000/swagger.json \
  --api-url  http://localhost:3000 \
  --db-url   postgres://perf:perf@localhost:5432/app \
  --token    "$API_TOKEN" \
  -H "X-Tenant-Id: acme" -H "X-Api-Key: $API_KEY" \
  -b "session=$SESSION" --cookie-jar ./cookies.txt
```

- The dialect comes from the DB URL scheme: `postgres://` or `postgresql://` means Postgres, `mysql://` means MySQL.
- Settings are applied in this order of precedence: **flags > `routeperf.yaml` > environment**. Config values support `${VAR}` interpolation.
- `-H` and `-b` can be repeated and use curl syntax.

### 2.2 Config file (`routeperf.yaml`)

```yaml
spec: http://localhost:3000/swagger.json
api:
  base_url: http://localhost:3000
  timeout: 30s
db:
  url: ${DATABASE_URL}            # postgres://… or mysql://…
  allow_remote: false             # refuse non-localhost unless true

auth:                             # any combination; see §9
  bearer: ${API_TOKEN}
  headers:   { X-Tenant-Id: acme, X-Api-Key: ${API_KEY} }
  cookies:   { session: ${SESSION_COOKIE} }
  cookie_jar: ./cookies.txt
  oauth2_client_credentials:
    token_url: http://localhost:3000/oauth/token
    client_id: ${CLIENT_ID}
    client_secret: ${CLIENT_SECRET}
    scope: "read write"
  login:
    method: POST
    path: /auth/login
    body: { email: perf@test.dev, password: ${PERF_PASSWORD} }
    extract: { cookies: true, bearer_from: "$.data.accessToken" }
    refresh_on: [401]

capture:
  mode: log                       # log (default) | proxy (later)
  pg_log_source: auto             # auto | pg_read_file | docker:<container>
  settle: 50ms                    # wait after response for trailing queries

run:
  methods: [GET, HEAD, POST, PUT, PATCH, DELETE]
  warmup: 3
  iterations: 20
  include: []                     # operationIds or tags; empty = all
  exclude: []
  dangerous_ops: []               # must list bulk deletes/resets explicitly

scale:
  data_steps: [0.01, 0.03, 0.10, 0.30, 1.0]
  k_params: [limit, page_size, pageSize, per_page, size, top, first, count]
  k_steps: [1, 10, 100, 1000]
  growth_model: wider             # wider (more parents) | denser (more children per parent)

writes:
  snapshot: tables                # tables | template (PG) | dump | none

thresholds:
  p95_ms: 300
  max_queries_per_request: 10
  max_degree: 1                   # fail CI if any route is worse than O(n)

fixtures: ./fixtures.yaml
```

### 2.3 Fixtures (optional, `fixtures.yaml`)

Fixtures are keyed by `operationId`. A value prefixed with `sql:` is resolved against the DB at run time.

```yaml
getUserOrders:
  path:  { id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1" }
  query: { status: "paid" }
createOrder:
  body:
    user_id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1"
    items: [{ sku: "ABC-1", qty: 2 }]
```

`{anchor}` expands to the dialect's "row is in the 1% subset" predicate (§7.4). Using it guarantees that the same IDs exist at every data scale.

**Input resolution order** (first match wins):
1. `fixtures.yaml`
2. Spec `example`, `examples`, or `default`
3. **DB sampling**: map the parameter to a column (`userId` → `users.id`, `{id}` on `/orders/{id}` → `orders.id`) and pick an anchor row
4. Schema-driven fake data (`format`, `enum`, `pattern`, `minimum`/`maximum`)

`routeperf discover` prints every parameter it could not resolve and writes a fixtures skeleton for them.

---

## 3. Architecture

```mermaid
flowchart LR
  subgraph IN["User inputs"]
    S["swagger.json URL"]
    A["API URL + auth"]
    D["Local DB URL"]
    F["fixtures.yaml (optional)"]
  end

  S --> SL["Spec loader<br/>OAS2/3, $ref"] --> CAT["Endpoint catalog<br/>ops, params, bodies, security"]
  CAT --> IR["Input resolver"]
  F --> IR
  D --> IR
  A --> AU["Auth manager"]
  IR --> EX["HTTP executor<br/>warmup, N iters, k-axis"]
  AU --> EX
  EX -->|requests| APP["Your API"]
  APP -->|SQL| DB[("Local Postgres / MySQL")]
  DB -->|statement log| CAP["Capture + correlation"]
  EX -->|request windows| CAP
  CAP --> RP["EXPLAIN replay<br/>rolled back"]
  RP <--> DB
  SC["Scale engine<br/>nested FK-consistent subsets"] <--> DB
  RP <--> SC
  RP --> CX["Complexity engine<br/>plan algebra + curve fit"]
  EX -->|latency, q per request| CX
  CX --> ADV["Advisor"] --> REP["Reports<br/>tty, JSON, MD, HTML, diff"]
  SNAP["Snapshot manager"] <--> DB
```

| Component | Responsibility |
|---|---|
| Spec loader | Fetch the spec URL and resolve `$ref`s. Convert Swagger 2.0 to OAS3 so later stages handle a single shape |
| Endpoint catalog | Per operation: method, path, params, body schema, response schema, `security`, tags. Groups operations into *resources* and orders them (§8.1) |
| Input resolver | Builds concrete requests using the order in §2.3. Picks **anchor IDs** that exist at every scale |
| Auth manager | Applies static credentials, fetches and refreshes OAuth2/login tokens, maps `securitySchemes` to credentials (§9) |
| HTTP executor | Sends requests **serially**. Does warmup, timed iterations, and k-axis sweeps. Records an HDR histogram, status codes, and response sizes |
| Capture | Turns server-side statement logs into per-request lists of `(sql, params, duration, session)` (§5) |
| EXPLAIN replay | Re-runs each unique query with `EXPLAIN ANALYZE` on real tables and on scaled copies. DML always runs inside a transaction that is rolled back (§6) |
| Scale engine | Builds nested, FK-consistent subsets at 1/3/10/30% (§7.4) |
| Complexity engine | Static plan algebra, curve fitting, and combination into a Big O with confidence (§7) |
| Advisor | Rule-based fixes (§10) |
| Snapshot manager | Snapshots before writes, detects touched tables, restores after the run (§8.3) |
| Reporter | Terminal summary, `results.json`, Markdown, HTML, and baseline `diff` |

---

## 4. Run lifecycle

```mermaid
sequenceDiagram
  participant U as User
  participant T as routeperf
  participant API as Your API
  participant DB as Local DB

  U->>T: routeperf run
  T->>DB: preflight: version, privileges, host is local
  T->>T: load spec, catalog, resolve inputs (anchor IDs)
  T->>DB: snapshot (if writes enabled)
  T->>DB: enable statement logging
  rect rgba(120,160,255,0.12)
  note over T,DB: Phase R: read-only operations
  loop each GET/HEAD op, serially
    T->>API: warmup + N requests, then k-axis sweep
    API->>DB: app SQL
    T->>DB: read log slice for each request window
  end
  end
  T->>DB: build scaled subsets of in-scope tables (data still pristine)
  rect rgba(255,160,120,0.12)
  note over T,DB: Phase W: write operations in lifecycle order
  loop POST → GET → PUT/PATCH → DELETE per resource
    T->>API: requests with chained IDs
    API->>DB: app SQL
    T->>DB: read log slice
  end
  end
  T->>DB: disable logging, restore server settings
  loop each unique query fingerprint
    T->>DB: EXPLAIN ANALYZE at 100% and at each subset (rolled back)
  end
  T->>DB: restore touched tables, drop subsets
  T->>T: fit curves, compose Big O, run advisor
  T->>U: tty summary + results.json + report.html
```

1. **Preflight.** Check the DB version, privileges (§5.1), whether the host is local, whether the API is reachable, and whether auth works (one probe request per security scheme). Measure the fixed HTTP overhead by calling a trivial route (`/health` if one exists).
2. **Discover.** Build the catalog, resolve inputs, and list skipped operations with the reason for each (missing auth, unresolved param, `dangerous_ops`).
3. **Snapshot.** Only when writes are enabled (§8.3).
4. **Phase R.** Read-only operations first, so the scaled subsets are built from pristine data.
5. **Build subsets** (§7.4). Tables that first appear in Phase W are copied from the snapshot copy when one exists.
6. **Phase W.** Write operations in lifecycle order with ID chaining (§8.1).
7. **Replay.** Run `EXPLAIN ANALYZE` on every unique query fingerprint. This happens *before* the restore so that tool-created rows still exist.
8. **Restore and clean up.**
9. **Analyze and report.**

---

## 5. Capturing SQL per route

The DB is local, so the tool can turn on full statement logging, read it back, and turn it off again. The app stays untouched.

### 5.1 Log mode (default)

| | Postgres | MySQL |
|---|---|---|
| Enable | `ALTER SYSTEM SET log_min_duration_statement = 0;` plus `log_parameter_max_length = -1` (log bind params in full) and `log_line_prefix = '%m [%p] %a '`, then `SELECT pg_reload_conf();`. A reload applies to the app's existing sessions, so no app restart is needed | `SET GLOBAL log_output = 'TABLE'; SET GLOBAL general_log = 'ON';` (dynamic, no restart) |
| Read | Collector on: `pg_read_file(pg_current_logfile('jsonlog'), offset, len)` (jsonlog needs PG 15+; csvlog on 13–14). Collector off (common in Docker): `docker logs --since` on the container, parsing the text format with the prefix above | `SELECT event_time, thread_id, command_type, argument FROM mysql.general_log WHERE event_time BETWEEN ? AND ?` |
| Bind params | The extended protocol logs `execute <name>: SELECT … $1` with `DETAIL: Parameters: $1 = '42'` | Each `Execute` row of a prepared statement holds the SQL *with values already substituted* |
| Exclude own sessions | Tool sessions set `application_name = 'routeperf'` and `SET log_min_duration_statement = -1`. Lines are also filtered by pid | Tool session runs `SET SESSION sql_log_off = ON`. Rows are also filtered by `CONNECTION_ID()` |
| Extra counters | none | `performance_schema.events_statements_history_long` (enable the consumer) gives `ROWS_EXAMINED` and `TIMER_WAIT` for each app statement |
| Restore | `ALTER SYSTEM RESET …; SELECT pg_reload_conf();` | Previous `general_log` / `log_output` values put back, then `TRUNCATE mysql.general_log` |
| Privileges | Superuser, or `pg_read_server_files` plus a grant for `ALTER SYSTEM` (PG 15+) | `SYSTEM_VARIABLES_ADMIN` (or `SUPER`), `SELECT` on `mysql.general_log` and `performance_schema` |

Settings are restored on any exit, including Ctrl-C, through a deferred cleanup plus a crash marker file. The next run checks the marker and repairs the settings if needed.

### 5.2 Correlating queries to requests

- Requests are sent **one at a time**. Each request owns the window `[t_send − 5ms, t_last_byte + settle]`. The DB is on the same machine, so there is no clock skew.
- **Quiet check.** Before each request, the tool confirms that no non-tool statements arrived in the previous 200 ms. If any did, a background job or another client is active. The tool warns, retries, and marks affected samples as noisy.
- **Statement classes.** `BEGIN/COMMIT/ROLLBACK/SET/SHOW/SELECT 1/DEALLOCATE` are counted as overhead and transactions, but excluded from Big O.
- **Trailing statements** that arrive after the response but within `settle` are tagged "post-response". This catches async work started by the request.
- **Fingerprints.** pg_query fingerprints for Postgres and a normalized digest from the parser for MySQL. Repeated fingerprints within one request are the basis for N+1 detection.

### 5.3 Proxy mode (later, M5)

This mode is for DBs where the tool can't change server logging (no superuser, managed DB). It runs a wire-protocol proxy (`pgproto3` for PG, `go-mysql` server package for MySQL), and the app's DB URL points at the proxy port. It gives exact SQL plus typed bind params. Known caveat: go-mysql drops `COM_STMT_EXECUTE` args when the client does not re-send parameter types, so the proxy must cache the types from the first execute.

---

## 6. EXPLAIN replay

Every unique `(fingerprint, params)` is replayed on a separate tool connection.

### 6.1 Postgres

```sql
BEGIN;
SET LOCAL statement_timeout = '60s';
SET LOCAL search_path = <scale schema>, public;          -- for subsets only
EXPLAIN (ANALYZE, BUFFERS, VERBOSE, SETTINGS, FORMAT JSON) <sql>;   -- params bound, untyped
ROLLBACK;
```

- Params are bound as untyped text (OID 0), so the server infers types exactly as it did for the app's original `Parse`.
- JSON fields used: `Node Type`, `Relation Name`, `Index Name`, `Actual Rows`, `Actual Loops`, `Rows Removed by Filter`, `Rows Removed by Index Recheck`, `Shared Hit/Read Blocks`, `Actual Total Time`, `Plan Rows`, top-level `Execution Time` and `Triggers` (FK checks with `Calls` and `Time`).
- `Actual Rows` and `Rows Removed by Filter` are **per-loop averages**. The tool multiplies them by `Actual Loops`.
- **Generic plans.** Apps that use prepared statements may switch to a generic plan after five executions. With `--pg-plan-mode generic`, the tool sets `plan_cache_mode = force_generic_plan` and replays through `PREPARE` + `EXPLAIN ANALYZE EXECUTE`, so the plan matches what the app actually runs.
- AFTER triggers are reported separately. Deferred constraint triggers are not measured, which is noted in the report.

### 6.2 MySQL

```sql
START TRANSACTION;
EXPLAIN ANALYZE <sql>;            -- TREE output; FORMAT=JSON when the server supports it (feature-detected)
ROLLBACK;
```

- TREE lines are parsed by indentation:
  `-> Index lookup on o using idx_user (user_id=42)  (cost=1.10 rows=3) (actual time=0.031..0.035 rows=3 loops=1)`
- JSON output for `EXPLAIN ANALYZE` exists on newer servers (`explain_json_format_version=2`). The tool tries it first and falls back to TREE.
- **Gap:** `EXPLAIN ANALYZE` supports only `SELECT`, multi-table `UPDATE`/`DELETE`, and `TABLE`. For `INSERT` and single-table `UPDATE`/`DELETE`, the tool does three things:
  1. Runs `EXPLAIN FORMAT=JSON` to get the estimated plan and access types.
  2. Executes the DML inside `START TRANSACTION … ROLLBACK` and reads its `ROWS_EXAMINED` and `TIMER_WAIT` plus `Handler_read_*` deltas.
  3. Rewrites the statement as a WHERE probe, `SELECT 1 FROM t WHERE <same predicate>`, and runs `EXPLAIN ANALYZE` on that to cost the lookup part.

### 6.3 Unified plan model

Both dialects map into one tree, so the complexity engine does not depend on the dialect:

```go
type Node struct {
    Op          Op       // SeqScan, FullIndexScan, IndexRange, IndexLookup, UniqueLookup,
                         // Sort, TopNSort, HashJoin, NestedLoop, MergeJoin, Aggregate,
                         // Limit, CorrelatedSubPlan, Materialize, Memoize, Modify, Other
    Relation    string
    Index       string
    Rows        float64  // total = per-loop rows × loops
    Loops       float64
    RowsRemoved float64  // total, filters + rechecks
    EstRows     float64
    Pages       float64  // PG buffers; MySQL n/a per node
    TimeMs      float64  // inclusive
    Blocking    bool     // consumes all input before emitting (Sort, Hash, HashAggregate…)
    Children    []*Node
}
```

---

## 7. How the Big O is computed

### 7.1 What can be observed

A spec contains no algorithm. What the tool *can* observe is:
1. **The plan.** EXPLAIN shows which algorithm the DB chose: scan type, join type, sort.
2. **Growth with data.** Replay the same query on smaller, nested copies of the data and watch how its work grows.
3. **Growth with output.** Ask the API for more rows (`limit`), or send bigger bulk bodies, and watch latency and query count grow.

Each signal misses something on its own. The plan can't tell whether a filter's output grows with the table. Timing is noisy. HTTP growth can't separate DB cost from app cost. Combined, they give a defensible estimate. This is the empirical-complexity approach of *trend-prof* (fit workload size against cost over several orders of magnitude) and Google Benchmark's `oAuto` fitting, applied to queries.

### 7.2 Variables

| Symbol | Meaning |
|---|---|
| `n_t` | Row count of table `t` (exact count on subsets, `reltuples` / `TABLE_ROWS` as a hint at 100%) |
| `k` | Route output size: the value of the page-size param, or the length of a body array for bulk writes |
| `q` | SQL statements per request |
| `r_v` | Rows produced by plan node `v` (total, all loops) |

Big O is reported **with table names**, e.g. `O(n_orders + k·log n_order_items)`. A bare `O(n)` hides which table is the problem.

### 7.3 Signal A: static plan algebra

Each plan is turned into a symbolic cost expression, bottom-up.

**Leaf and node costs**

| Unified op | Postgres node | MySQL TREE iterator | Cost |
|---|---|---|---|
| SeqScan | `Seq Scan`, `Parallel Seq Scan` | `Table scan on t` | `n_t` |
| FullIndexScan | `Index Scan` without index condition | `Index scan on t using i` | `n_t` |
| IndexRange | `Index Scan` / `Index Only Scan` / `Bitmap Index Scan` with range condition | `Index range scan on t` | `log n_t + r` |
| IndexLookup | `Index Scan` with equality on a non-unique index | `Index lookup on t using i` | `log n_t + r` |
| UniqueLookup | equality on PK / unique index | `Single-row index lookup` | `log n_t` |
| Sort | `Sort` | `Sort` | `r_in · log r_in` |
| TopNSort | `Sort` with `top-N heapsort` | `Sort … limit input to N row(s)` | `r_in · log N` |
| HashJoin | `Hash Join` + `Hash` | `Inner hash join` | `cost(build) + cost(probe)` |
| MergeJoin | `Merge Join` | n/a | `cost(a) + cost(b)` |
| NestedLoop | `Nested Loop` | `Nested loop inner/left join` | `cost(outer) + r_outer · cost(inner)` |
| CorrelatedSubPlan | `SubPlan` | `Select #N (subquery…; dependent)` | `r_outer · cost(sub)` |
| Aggregate | `Aggregate`, `HashAggregate`, `GroupAggregate` | `Aggregate`, `Group aggregate` | `cost(child) + r_child` |
| Limit L | `Limit` | `Limit` | `cost(child)` if the pipeline below has a blocking node, else `prefix(child, L)`, which scales with `L` |
| Memoize | `Memoize` (PG 14+) | n/a | `distinct_keys · cost(inner) + hits` |
| Modify | `ModifyTable` | DML (via §6.2 fallbacks) | `cost(child) + r · (#indexes(t) · log n_t + Σ FK-check cost)` |

**Composition rules**
1. Costs are polynomials over the symbols `n_t`, `k`, `r_v`, and their logs.
2. Sibling subtrees **add**. A nested loop or correlated subplan **multiplies** the inner cost by the outer row count.
3. Each `r_v` is replaced by its *measured growth* from Signal B. `r_v ~ n_t^e` with `e≈0` means a constant (e.g. equality on a selective column). `e≈1` means it grows with the table (e.g. a range or low-selectivity filter).
4. **Dominance pruning.** Term A dominates term B if every exponent in A is ≥ the matching exponent in B and at least one is strictly greater. Terms that are incomparable are kept, e.g. `n_orders + n_users`.
5. Fallback when Signal B is unavailable (tables too small): equality on a unique index gives `r=1`; equality on a non-unique index gives `r = fan-out` (constant); a range or no predicate gives `r ∝ n_t`; any other filter gives `r = σ·n_t` with σ taken from the planner's estimate.

**FK costs on writes (PG).** The `Triggers` array lists `RI_ConstraintTrigger_*` entries with `Calls` and `Time`. If a parent `DELETE`/`UPDATE` drives an FK on a child table whose FK column has **no index**, each call is a scan of the child, giving `r · n_child`. The catalog is checked for this directly. InnoDB auto-creates indexes on FK columns, so this case mostly affects Postgres.

### 7.4 Signal B: data-scale experiments

The goal is to watch each query's work grow with `n` while everything else stays fixed.

1. **Scope.** Tables referenced by captured queries, plus their FK closure (the parents needed for joins).
2. **FK graph** from the catalog: PG `pg_constraint`, MySQL `information_schema.REFERENTIAL_CONSTRAINTS` + `KEY_COLUMN_USAGE`.
3. **Nested, deterministic sampling.** A root table (no in-scope parent) keeps a row when `hash(pk) mod 10000 < p·10000`. The hash is deterministic, so the subsets nest: **1% ⊂ 3% ⊂ 10% ⊂ 30% ⊂ 100%**.
4. **FK-consistent children** (default `growth_model: wider`). Child tables are walked in topological order. A child row is kept when all of its non-null in-scope FK parents are kept. Per-parent fan-out (orders per user) stays realistic, and `n` grows by adding *more entities of the same shape*. With `growth_model: denser`, children are hashed independently instead, so fan-out grows. Use this when the realistic growth is "more orders per user". Tables in cycles, or not reachable from a root, are hashed on their own PK.
5. **Materialize** each step into its own namespace (`_rp_s01`, `_rp_s03`, `_rp_s10`, `_rp_s30`) with identical indexes:
   PG `CREATE TABLE _rp_s10.t (LIKE public.t INCLUDING ALL); INSERT … SELECT …;`
   MySQL `CREATE TABLE _rp_s10.t LIKE app.t; INSERT … SELECT …;`
   Then run `ANALYZE` (PG) or `ANALYZE TABLE` (MySQL) so planner statistics match each subset.
6. **Route replay to a step.** PG uses `SET LOCAL search_path`, and schema-qualified names are rewritten through a pg_query parse/deparse. MySQL uses `USE _rp_s10`, and qualified names are rewritten through the SQL parser.
7. **Anchor IDs.** Captured params must exist at every step, so the input resolver picks IDs from the 1% root set (the `{anchor}` predicate). Nesting guarantees they exist everywhere.
8. **Minimum size.** A step is skipped when its largest in-scope table has fewer than 1,000 rows. If a table has fewer than 10k rows in total, the tool reports *static only, confidence low*, and suggests loading more bulk data.
9. **Repeats.** One warmup plus five measured runs per step, keeping the median. All steps are measured with a warm cache, so cache state doesn't bias the curve.
10. **Plan flips.** Each step records a plan-shape hash (node types + relations + indexes). If the shape changes, each contiguous segment is fitted separately and the report names the switch, e.g. "planner switches Index Scan → Seq Scan between 10% and 30%". This is often the most useful finding.
11. **Per-node growth.** Nodes are matched across steps by shape path. Each node's `r_v` is fitted against `n` to get its exponent `e`, which feeds rule 3 of §7.3.
12. **Cost.** Subsets take about 44% of the in-scope tables' size. They are dropped after the run unless `--keep-scale` is passed.

DML is scale-replayed for `INSERT`s (FK parents are anchors, so they exist) and for `UPDATE`/`DELETE`s whose targets exist in the subset. Statements that target rows created by the tool fall back to static analysis plus the 100% replay.

### 7.5 Signal C: output-scale experiments (over HTTP)

- **Scale params.** Integer query params whose names match `k_params` (honoring the schema's `minimum`/`maximum`), and body arrays on bulk endpoints (honoring `maxItems`).
- **Steps.** `k ∈ {1, 10, 100, 1000}`, capped by the schema and by the number of rows available.
- At each `k`, the tool measures:
  - `T_http(k)`: median request latency
  - `q(k)`: statements per request, by fingerprint
  - `T_db(k)`: sum of statement durations from the log
  - `T_app(k) = T_http(k) − T_db(k) − fixed overhead`
- **N+1.** Fit `q(k)` per fingerprint. Degree 1 means that fingerprint runs once per returned item, which is N+1. The same fingerprint with *identical* params repeated within one request is reported as a redundant query.
- **App-side complexity.** Fit `T_app(k)`. Degree ≥ 2 flags in-memory work in the app such as nested loops or repeated sorts, even though the tool can't see the source.

### 7.6 Work metrics vs time

Wall-clock time is noisy: caches, CPU frequency, GC, background I/O. trend-prof fits basic-block *counts* rather than time for this reason. routeperf fits a **deterministic work counter** to choose the growth class, and uses time only for coefficients and projections.

| Metric | Postgres | MySQL |
|---|---|---|
| **Rows examined** (primary) | Σ over scan nodes of `(Actual Rows + Rows Removed by Filter + Rows Removed by Index Recheck) × Actual Loops` | `Handler_read_*` delta in the replay session (`FLUSH STATUS` → run → `SHOW SESSION STATUS LIKE 'Handler_read%'`), cross-checked with `ROWS_EXAMINED` |
| Pages touched | Root node `Shared Hit Blocks + Shared Read Blocks` (cumulative) | `Innodb_buffer_pool_read_requests` delta (global, valid because the tool runs alone) |
| Time | `Execution Time` | Root iterator's last-row `actual time` / `TIMER_WAIT` |

Rows examined can't see comparison counts inside a sort or B-tree depth. Those **log factors come from Signal A**.

### 7.7 Curve-fitting algorithm

Input: points `(x_i, y_i)`, where `x` is `n_t` of the dominant table (Signal B) or `k` (Signal C), and `y` is the median work or time at that step.

```text
classes := [O(1), O(log x), O(x), O(x log x), O(x^2), O(x^3)]     // ordered simplest → most complex

for c in classes:
    // y ≈ a + b·f_c(x), with a ≥ 0, b ≥ 0   (non-negative least squares; O(1): y ≈ a)
    // the intercept a absorbs fixed overhead (planning, round trip), which otherwise
    // makes linear growth look like O(log x) at small x
    (a_c, b_c) := nnls(f_c(x), y)
    nrmse[c]   := sqrt(mean((y − (a_c + b_c·f_c(x)))^2)) / mean(y)   // normalized RMS, as in Google Benchmark

best   := argmin_c nrmse[c]
chosen := first c in classes with nrmse[c] ≤ 1.10·nrmse[best] + 0.005  // Occam: prefer the simpler class
                                                                        // unless the complex one is clearly better
slope  := OLS slope of log(y − a_chosen) on log(x)                      // power-law exponent, trend-prof style
degree := round(slope)  if |slope − round(slope)| ≤ 0.25  else "between"

r2       := 1 − SS_res/SS_tot for the chosen class
decades  := log10(max x / min x)
```

**Confidence**

| Level | Conditions |
|---|---|
| high | ≥ 4 points, `decades ≥ 1.5`, `R² ≥ 0.95`, slope bucket agrees with the chosen class, static and empirical degrees agree, no plan flip |
| medium | exactly one condition fails |
| low | two or more conditions fail, or static only |

`O(x)` and `O(x log x)` differ only by a log factor, which needs a wide range and clean data to tell apart. That is why the log factor comes from the plan (§7.8), not from the fit.

### 7.8 Combining into one verdict per query

```mermaid
flowchart TD
  P["EXPLAIN plans at 1%…100%"] --> SH{"Same plan shape at every step?"}
  SH -- no --> SEG["Split into segments, fit each,<br/>report the plan flip point"]
  SH -- yes --> W["Fit rows-examined W(n)"]
  SEG --> W
  W --> DEG["Degree d from W's slope<br/>(robust, deterministic)"]
  P --> ST["Static algebra (§7.3) with<br/>per-node growth substituted"]
  ST --> LOG["Log factors l + table attribution"]
  DEG --> CMB["Class = n_t^d · log^l n_t"]
  LOG --> CMB
  CMB --> TCHK{"Time fit T(n) within one class?"}
  TCHK -- yes --> OK["Verdict + confidence"]
  TCHK -- no --> FLAG["Verdict, confidence lowered,<br/>note: likely cache/IO effect"]
  ST --> AGREE{"Static degree == empirical d?"}
  AGREE -- no --> WHY["Empirical wins; explain why:<br/>selectivity misestimate, filter not indexed, flip"]
```

1. The **degree** comes from fitting rows examined over Signal B.
2. **Log factors and table names** come from the static algebra (sort steps, index descents).
3. **Time** must agree within one class. Otherwise confidence drops and the report says why.
4. **If static and empirical disagree, empirical wins**, and the report explains the gap.

### 7.9 Route-level Big O

```
C_route(n, k) = Σ over fingerprints fp of [ count_fp(k) · C_fp(n, k) ]  +  C_app(k)
```

- `count_fp(k)` comes from Signal C: either a constant or `∝ k` (N+1).
- `C_fp` is each query's verdict from §7.8.
- `C_app` comes from the `T_app(k)` fit.
- The report shows the dominant term(s) plus a table with one row per component. For routes without a scale param, rows returned are reported as an observed constant.

**Projection.** Each query's time fit `T_fp(n') = a + b·f(n')`, multiplied by `count_fp`, is summed at **10×** and **100×** the current data. Projections outside the measured range carry the warning "plan flips beyond observed range are possible".

### 7.10 Worked example (illustrative numbers)

`GET /users/{id}/orders?limit=k`. Data: 240k users, 2.4M orders, 9.6M order_items.

Captured at `k = 50`:

| # | Fingerprint | Count / req |
|---|---|---|
| Q1 | `SELECT * FROM users WHERE id = $1` | 1 |
| Q2 | `SELECT * FROM orders WHERE user_id = $1 ORDER BY created_at DESC LIMIT $2` | 1 |
| Q3 | `SELECT * FROM order_items WHERE order_id = $1` | 50 |

**Signal C.** `q(k) = k + 2`, so Q3 is an **N+1**.

**Q2 plan:** `Limit → Sort → Seq Scan orders (Filter: user_id = $1)`. Static cost: `n_orders + r·log r`.

**Signal B for Q2:**

| Step | n_orders | Rows examined | Time |
|---|---|---|---|
| 1% | 24k | 24,010 | 2.3 ms |
| 3% | 72k | 72,010 | 6.6 ms |
| 10% | 240k | 240,010 | 21 ms |
| 30% | 720k | 720,010 | 64 ms |
| 100% | 2.4M | 2,400,010 | 212 ms |

Fit on rows examined: `O(x)` nRMSE 0.001, `O(x log x)` 0.04, `O(log x)` 0.58. Chosen class `O(n)`, slope 1.00, R² 1.00. The Sort node's `r` stays constant (about 10 orders per user, `e≈0`), so `r·log r` is dominated. **Q2 = O(n_orders)**, confidence high.

**Q3:** `Index Scan using order_items_order_id_idx`. Rows examined stay flat across steps (`e≈0`), and the static rule `log n_t + r` gives **O(log n_order_items)**.

**Route:** `O(n_orders + k · log n_order_items)`, dominant term `n_orders`. At current data, p50 is 180 ms. Projected p50 is about 1.8 s at 10× and 18 s at 100×.

**Advice:**
- `CREATE INDEX ON orders (user_id, created_at DESC)` turns Q2 into `O(log n + k)`.
- Batch Q3 with `WHERE order_id = ANY($1)` / `IN (…)`, which brings `q` to 3.

### 7.11 Limitations (stated in every report)

- App computation that never touches the DB is visible only through `T_app(k)`. The tool has no access to the source.
- App caches (Redis, ORM identity maps) hide queries on repeat requests. The report shows first-hit vs repeat behavior separately.
- Exponential and factorial growth are not modeled.
- Subsets can shift the distribution of correlated columns. FK-consistent hashing reduces this, and the report discloses it.
- B-tree `log n` is constant in practice (depth 3–5 even for billions of rows). It is shown for correctness but ignored when ranking routes.
- Small-scale planner statistics can produce different plans. These are *detected and reported* as plan flips, never hidden.

---

## 8. Write operations (all methods)

### 8.1 Ordering and ID chaining

- **Resources** are path prefixes with params stripped: `/users/{id}/orders/{orderId}` maps to `users.orders`.
- **Order** within each resource: `POST` (create) → `GET` item → `GET` list → `PUT`/`PATCH` → `DELETE`. Parent resources run before child resources.
- **ID chaining** uses OAS3 `links` when present. Otherwise it takes a heuristic from the create response (`id`, `<resource>Id`, `Location` header) and maps it to path params on the same prefix.
- **Iterations.** Each `POST` iteration creates a new row. `PUT`/`PATCH` iterate on one tool-created resource. For `DELETE`, the tool pre-creates N resources outside the timed section, then deletes them one per iteration.
- **Dangerous operations** are never run unless listed in `dangerous_ops`. These are collection-level `DELETE` (no ID), and paths matching `reset|purge|truncate|drop|wipe|admin`.

### 8.2 Measuring writes

- HTTP latency and the k-axis work the same as for reads. For bulk endpoints, `k` is the body array length.
- DML replay: Postgres uses `EXPLAIN ANALYZE` inside `BEGIN … ROLLBACK`, which includes FK trigger costs. MySQL uses the §6.2 fallbacks. Either way, nothing is committed.
- Notes in the report:
  - Sequences and `AUTO_INCREMENT` counters advance even when the transaction rolls back. The restore resets them.
  - Triggers fire during replay. Their cost is part of the measurement.

### 8.3 Snapshot and restore

| Strategy | Postgres | MySQL | Speed | Notes |
|---|---|---|---|---|
| `tables` (default) | `CREATE TABLE _rp_snap.t AS TABLE public.t` | `CREATE TABLE _rp_snap.t LIKE app.t; INSERT … SELECT` | medium | Server-side copy, no network. About 2× disk for in-scope tables. Restores only *touched* tables |
| `template` | `CREATE DATABASE rp_snap TEMPLATE app STRATEGY FILE_COPY`; restore with `DROP DATABASE app WITH (FORCE)` + `CREATE DATABASE app TEMPLATE rp_snap` | n/a | fastest | Needs all sessions disconnected (the tool terminates them and the app pool reconnects), plus the `CREATEDB` privilege |
| `dump` | `pg_dump -Fc` / `pg_restore` | `mysqldump --single-transaction` / `mysql` | slowest | Needs client binaries |
| `none` | | | | Lifecycle cleanup only. The report warns about drift: updated rows stay changed, counters advance |

**Restore under `tables`** runs in one transaction per dialect:
- **Postgres:**
  1. `SET session_replication_role = replica`
  2. `TRUNCATE` each touched table
  3. `INSERT … SELECT` from the snapshot (`OVERRIDING SYSTEM VALUE` for identity columns)
  4. `setval` on sequences
  5. `ANALYZE`
- **MySQL:**
  1. `SET FOREIGN_KEY_CHECKS = 0`
  2. `TRUNCATE` each touched table
  3. `INSERT … SELECT` from the snapshot
  4. Reset `AUTO_INCREMENT`
  5. `ANALYZE TABLE`

### 8.4 Detecting touched tables

The tool doesn't rely only on parsed DML. It diffs per-table modification counters before and after Phase W. This also catches cascades and tables written by triggers:
- **Postgres:** `pg_stat_user_tables.n_tup_ins / n_tup_upd / n_tup_del`
- **MySQL:** `performance_schema.table_io_waits_summary_by_table.COUNT_INSERT / COUNT_UPDATE / COUNT_DELETE`

### 8.5 Safety guards

- **Local only.** The DB host must be `localhost`, `127.0.0.1`, `::1`, a Docker bridge address, or a socket. Anything else needs `--allow-remote-db`. **Never point routeperf at production.** `EXPLAIN ANALYZE` executes statements, and write routes commit through the app.
- **Writes need an explicit OK.** The first time writes run, an interactive confirmation shows the snapshot strategy and the operations involved. CI runs require `--yes`.
- **No snapshot, no writes.** If the snapshot fails, Phase W is skipped.
- **Crash recovery.** A crash marker records pending restores and server-setting changes. `routeperf repair` replays them.

---

## 9. Auth

| Mechanism | Config | Behavior |
|---|---|---|
| Bearer token | `--token` / `auth.bearer` | `Authorization: Bearer <token>` |
| Custom headers | `-H` / `auth.headers` | Sent as given. Used for API keys, tenant IDs, etc. |
| Cookies | `-b k=v` / `auth.cookies` / `--cookie-jar` (Netscape/curl format) | Cookie jar scoped to the API host. `Set-Cookie` responses are honored |
| OAuth2 client credentials | `auth.oauth2_client_credentials` | Fetches the token, caches it, refreshes 30 s before `expires_in`, and retries once after a `401` |
| Login flow | `auth.login` | Calls the login endpoint, keeps its cookies, and/or takes a bearer token via JSONPath. Re-logs in on `refresh_on` statuses |

- **Scheme mapping.** For each operation, the spec's `security` requirement is matched to a configured credential:
  - `http/bearer` → bearer
  - `apiKey in: header` → matching header name
  - `apiKey in: cookie` → cookie
  - `apiKey in: query` → query param
  - `oauth2` → client credentials

  Operations with `security: []` are sent with no credentials. An operation whose required scheme has no credential is **skipped** with the reason "auth missing".
- **Excluded from timing.** Token fetches and logins happen outside the timed section.
- **Redaction.** `Authorization`, `Cookie`, `Set-Cookie`, configured secret headers, and the `login.body` password are masked in `results.json`, HTML, logs, and `--verbose` output. Prefer `${ENV}` references over literal secrets in `routeperf.yaml`.

---

## 10. Advisor rules

| Rule | Trigger | Suggestion |
|---|---|---|
| Unindexed filter | SeqScan on a table with more than 10k rows that removes more than 90% of rows | Index on the filter columns, with column order from equality → range → sort |
| Sort not served by an index | Sort over rows that grow with `n`, or PG `Sort Method: external merge` | Composite index matching `WHERE` + `ORDER BY`, or raise `work_mem` / `sort_buffer_size` |
| Missing join index | NestedLoop whose inner side is a SeqScan with high `loops` | Index on the join key |
| N+1 | `q(k)` per fingerprint has degree 1 | Eager load, `JOIN`, or `= ANY($1)` / `IN (…)` batching |
| Redundant query | Same fingerprint and params repeated in one request | Request-scoped cache / memoization |
| Unbounded list | List route with no `LIMIT` on a table with more than 10k rows | Pagination (keyset over offset) |
| Deep offset | `OFFSET` that grows with the page number | Keyset pagination |
| Stale statistics | Estimated rows off from actual rows by more than 10× | `ANALYZE` / `ANALYZE TABLE`; extended statistics for correlated columns (PG) |
| Unindexed FK (PG) | Parent `DELETE`/`UPDATE` whose FK triggers scan the child | Index on the child's FK column |
| Correlated subquery | CorrelatedSubPlan / dependent subquery with outer rows growing | Rewrite as `JOIN` / `LATERAL` |
| Plan flip | Plan shape changes across scale steps | Report the threshold; pin the access path with an index, or fix the statistics |
| App-side superlinear | `T_app(k)` has degree ≥ 2 | Inspect in-memory loops, serialization, repeated sorting |
| Cold I/O | High ratio of `Shared Read` to `Hit`, or high buffer-pool misses | Check working set vs memory; bloat (PG `VACUUM`) |

---

## 11. Reports

**Terminal summary**

```
ROUTE                          P50     P95    Q/REQ   BIG O                               CONF   STATUS
GET    /orders?limit={k}       38ms    61ms   1       O(k · log n_orders)                 high   OK
GET    /users/{id}/orders      180ms   240ms  k+2     O(n_orders + k·log n_order_items)   high   FAIL  seq scan + N+1
POST   /orders                 22ms    33ms   4       O(log n_orders)                     med    OK
DELETE /users/{id}             410ms   470ms  3       O(n_sessions)                       high   WARN  unindexed FK sessions.user_id
GET    /reports/sales          2.1s    2.6s   1       O(n_orders)                         high   FAIL  plan flip at 30%
```

**Per-route card** (in Markdown/HTML)

```
GET /users/{id}/orders
  Big O (route)    O(n_orders + k · log n_order_items)          confidence: high
  Dominant term    n_orders: Seq Scan on orders (2.4M rows)
  Evidence         rows-examined exponent 1.00 (R² 1.00, 2.0 decades, 5 points); q(k) = k + 2
  Now              p50 180 ms · p95 240 ms · 2.40M rows examined/request
  Projected        10×: p50 ≈ 1.8 s   100×: p50 ≈ 18 s
  Fixes            CREATE INDEX ON orders (user_id, created_at DESC)   → O(log n + k)
                   batch order_items lookup (= ANY($1))                → q = 3
```

**`results.json`** (excerpt)

```json
{
  "route": "GET /users/{id}/orders",
  "latency_ms": { "p50": 180, "p95": 240, "p99": 270 },
  "queries_per_request": { "model": "k + 2", "n_plus_one": ["q3"] },
  "big_o": {
    "expr": "O(n_orders + k·log n_order_items)",
    "dominant": "n_orders",
    "confidence": "high",
    "evidence": { "rows_examined_slope": 1.0, "r2": 1.0, "decades": 2.0, "points": 5, "plan_flip": false }
  },
  "projection": { "10x": { "p50_ms": 1750 }, "100x": { "p50_ms": 17400 } },
  "queries": [ { "id": "q2", "fingerprint": "…", "plans": { "1.0": "…", "0.1": "…" }, "fit": { "…": "…" } } ],
  "advice": [ { "rule": "unindexed_filter", "sql": "CREATE INDEX …", "expected": "O(log n + k)" } ]
}
```

- **HTML** (single file, `embed`): sortable route table, per-route growth charts (log-log scatter with fitted curve, inline SVG), collapsible plan trees, and the advice list.
- **`routeperf diff base.json new.json`**: shows a class regression (e.g. `O(log n)` → `O(n)`), p95 changes beyond tolerance, and changes in `q/req`. Exits non-zero on regression.

---

## 12. CLI surface

```
routeperf init                       Scaffold routeperf.yaml + fixtures.yaml
routeperf discover                   Load spec, list operations, resolve inputs, write a fixtures skeleton,
                                     show auth mapping and skipped ops
routeperf run                        Full run (all phases)
    --only <opId|tag:x>   --exclude <…>
    --no-writes           --yes
    --no-scale            --data-steps 0.01,0.1,1   --k-steps 1,10,100
    --pg-plan-mode custom|generic
    --project 10x,100x
    --out ./routeperf-out
routeperf report results.json --html report.html --md report.md
routeperf diff base.json new.json [--p95-tolerance 15%]
routeperf repair                     Undo leftover server settings / restore an interrupted snapshot
```

Global flags: `--spec --api-url --db-url --token -H -b --cookie-jar --config --verbose`.

---

## 13. Tech stack and layout

**Why Go.** The user asked for "whichever is faster", and Go wins on both counts:
- **Runtime.** Low overhead in the measured path. When proxy mode arrives (M5), the proxy sits inline with the app's DB traffic, so its overhead matters.
- **Build speed.** Maintained libraries exist for every component.
- **Distribution.** One static binary.

TypeScript was considered for its OpenAPI sampling libraries. It lost on proxy performance and distribution.

| Concern | Package |
|---|---|
| CLI | `spf13/cobra` |
| OpenAPI 2/3, `$ref`, v2→v3 | `getkin/kin-openapi` (+ `openapi2conv`) |
| Postgres | `jackc/pgx/v5` |
| MySQL | `go-sql-driver/mysql` |
| PG parse / fingerprint / deparse | `pganalyze/pg_query_go` (cgo) |
| MySQL parse / rewrite | `pingcap/tidb/pkg/parser` |
| JSONPath (login extract) | `ohler55/ojg` |
| Fake data | `brianvoe/gofakeit` |
| Latency histograms | `HdrHistogram/hdrhistogram-go` |
| Regression | `gonum.org/v1/gonum/stat` + a small two-parameter NNLS |
| Reports | `html/template` + `embed`, inline SVG charts |
| Later (proxy mode) | `jackc/pgx/v5/pgproto3`, `go-mysql-org/go-mysql` |

`pg_query_go` needs cgo, so release builds use `goreleaser` with zig or the cross toolchain.

```
cmd/routeperf/          main, cobra commands
internal/spec/          load, deref, convert
internal/catalog/       operations, resources, ordering, links
internal/inputs/        fixtures, examples, DB sampling, anchors, faker
internal/auth/          static, oauth2cc, login, cookie jar, scheme mapping, redaction
internal/httpx/         executor, timing, histograms, k-axis sweeps
internal/capture/       Capturer interface; pglog, mysqlgenlog (later: pgproxy, mysqlproxy)
internal/dialect/       Dialect interface; postgres, mysql
internal/explain/       replay runner, PG JSON + MySQL TREE/JSON parsers → unified plan
internal/scale/         FK graph, nested hash subsets, namespace routing
internal/complexity/    static algebra, fitting, combination, route composition, projection
internal/advisor/       rules
internal/snapshot/      tables, template, dump; touched-table detection; restore
internal/report/        tty, json, md, html, diff
```

```go
type Dialect interface {
    Preflight(ctx context.Context) (Capabilities, error)
    Explain(ctx context.Context, q Captured, scale Scale) (*plan.Node, Work, error)
    FKGraph(ctx context.Context) (*FKGraph, error)
    BuildSubsets(ctx context.Context, tables []Table, steps []float64, model GrowthModel) (Subsets, error)
    TableCounters(ctx context.Context) (map[string]Counters, error)   // touched-table detection
    Snapshot(ctx context.Context, s Strategy, tables []Table) (Snapshot, error)
}

type Capturer interface {
    Start(ctx context.Context) error                                   // enable logging, remember old settings
    Window(ctx context.Context, from, to time.Time) ([]Captured, error) // statements in a request window
    Stop(ctx context.Context) error                                    // restore settings
}
```

---

## 14. Milestones

| # | Scope | Exit criteria |
|---|---|---|
| **M1** | Spec loader, catalog, all auth mechanisms, input resolver, serial executor, `discover`, latency-only report | Every operation of a sample API (PG and MySQL backed) gets p50/p95, or a skip reason |
| **M2** | Log capture (PG + MySQL), correlation, quiet check, EXPLAIN replay (PG JSON, MySQL TREE/JSON), unified plan, static algebra, `q/req` | Each route lists its queries and plans, with a static Big O |
| **M3** | Scale engine (nested FK-consistent subsets), work metrics, curve fitting, plan-flip detection, k-axis, combined verdict and confidence, advisor, Markdown/HTML report | The §7.10 example is reproduced on a seeded fixture DB. Known N+1 and seq-scan routes are flagged |
| **M4** | Writes: lifecycle ordering, ID chaining, snapshot strategies, touched-table restore, MySQL DML fallbacks, safety guards, `repair`, thresholds, `diff` | A full run with all methods leaves DB checksums identical before and after. CI exits non-zero on an injected regression |
| **M5** | Proxy capture (non-superuser / remote), sqlcommenter correlation, HypoPG what-if checks for suggested indexes (PG), load mode | Captured SQL in proxy mode matches log mode on the sample API |

Test bed: a small sample API (one PG, one MySQL) with deliberately planted problems: an unindexed filter, an N+1, an unindexed FK, a correlated subquery, deep offset pagination, and an O(k²) in-memory loop. Every rule in §10 has a route that must trigger it and a route that must not.

---

## 15. Risks and mitigations

| Risk | Mitigation |
|---|---|
| `logging_collector` off on the local PG (common in Docker) | `pg_log_source: docker:<container>` reads `docker logs`. Otherwise a one-time instruction to enable the collector and restart. Proxy mode in M5 |
| Background jobs or other clients write to the DB during a run | Quiet check before each request, filtering by session, noisy samples marked |
| Small tables give no growth signal | Static-only mode with confidence capped at low, plus a "load more data" hint |
| Subsets distort selectivity | FK-consistent nested hashing, `growth_model` option, disclosure in the report |
| Plan flips at small scales | Segmented fits; the flip is reported as a finding |
| App caches hide queries | First-hit vs repeat measured separately; optional cache-bust header in config |
| PG generic vs custom plans | `--pg-plan-mode generic` |
| MySQL `EXPLAIN ANALYZE` doesn't cover INSERT or single-table DML | Three-part fallback (§6.2) |
| Writes drift the data | Snapshot + touched-table restore + checksum verification in tests |
| Secrets leak into reports | Central redaction; secrets referenced through env vars |
| Timing noise | Class chosen from deterministic work counters; time used only for coefficients |

---

## 16. References

- Goldsmith, Aiken, Wilkerson. *Measuring Empirical Computational Complexity* (trend-prof), ESEC/FSE 2007: fits linear and power-law models of cost against workload size, using counts rather than time. https://theory.stanford.edu/~aiken/publications/papers/fse07.pdf
- Google Benchmark, `src/complexity.cc`: least-squares coefficient `Σ(t·g)/Σ(g²)`, RMS normalized by the mean, best class by minimum RMS among O(1), O(log N), O(N), O(N log N), O(N²), O(N³). https://github.com/google/benchmark/blob/main/src/complexity.cc
- PostgreSQL docs, *Using EXPLAIN*: cost formula, per-loop averages, `BEGIN … ROLLBACK` for DML, trigger timing, BUFFERS. https://www.postgresql.org/docs/current/using-explain.html
- PostgreSQL docs, *Error Reporting and Logging*: `jsonlog` (PG 15+), `log_parameter_max_length`. https://www.postgresql.org/docs/15/runtime-config-logging.html
- MySQL docs, *EXPLAIN Statement*: `EXPLAIN ANALYZE` (TREE, statement-type limits). https://dev.mysql.com/doc/refman/8.4/en/explain.html
- MySQL blog, *New JSON format for EXPLAIN*: `explain_json_format_version=2`, JSON for `EXPLAIN ANALYZE`. https://dev.mysql.com/blog-archive/new-json-format-for-explain/
- MySQL bug #69453 / general query log: prepared-statement `Execute` lines logged with values substituted. https://bugs.mysql.com/bug.php?id=69453
- Percona, *Capture database traffic using the Performance Schema*: `events_statements_history_long`, `ROWS_EXAMINED`. https://www.percona.com/blog/capture-database-traffic-using-performance-schema/
- go-mysql issue #1195: `COM_STMT_EXECUTE` args dropped without new-params-bound (proxy caveat). https://github.com/go-mysql-org/go-mysql/issues/1195
- Related reading: Coppa, Demetrescu, Finocchi, *Input-Sensitive Profiling* (PLDI 2012); Zaparanuks, Hauswirth, *Algorithmic Profiling* (PLDI 2012).
