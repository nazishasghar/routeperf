<p align="center">
  <img src="docs/assets/banner.svg" alt="routeperf — per-endpoint performance and Big O for your API" width="100%">
</p>

<p align="center">
  <a href="https://github.com/nazishasghar/routeperf/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/nazishasghar/routeperf?color=7c3aed&label=release"></a>
  <a href="https://github.com/nazishasghar/routeperf/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/nazishasghar/routeperf/actions/workflows/ci.yml/badge.svg"></a>
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/nazishasghar/routeperf?color=00ADD8">
  <img alt="PostgreSQL 13+" src="https://img.shields.io/badge/PostgreSQL-13%2B-336791?logo=postgresql&logoColor=white">
  <img alt="MySQL 8.0.18+" src="https://img.shields.io/badge/MySQL-8.0.18%2B-4479A1?logo=mysql&logoColor=white">
  <img alt="Platforms" src="https://img.shields.io/badge/macOS%20%C2%B7%20Linux%20%C2%B7%20Windows-555">
</p>

<p align="center">
  <b>Point it at your Swagger/OpenAPI spec, your running API and its local database.<br>
  Get latency, DB time, queries per request, an estimated Big O and the exact fix for every endpoint.</b>
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#reading-the-report">Reading the report</a> ·
  <a href="#troubleshooting">Troubleshooting</a>
</p>

```bash
curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | sh
routeperf init && routeperf check && routeperf run
```

<p align="center">
  <img src="docs/assets/demo.svg" alt="routeperf run: endpoint table with p50/p95, DB time, queries per request, rows examined, Big O, confidence and status, followed by findings with CREATE INDEX fixes" width="100%">
</p>

## How it works

<p align="center">
  <img src="docs/assets/how-it-works.svg" alt="1 read the spec, 2 call every route, 3 capture the SQL, 4 replay with EXPLAIN ANALYZE on 1%→100% data subsets, 5 Big O and fixes" width="100%">
</p>

For each endpoint, routeperf:
- sends real requests, using your token, cookies or headers
- captures the SQL it runs from the database's statement log
- replays that SQL with `EXPLAIN ANALYZE` on 1%→100% copies of your data, so the growth curve shows the Big O
- reports what to fix: missing indexes, N+1 queries, unindexed foreign keys, O(k²) loops in app code

Works with **PostgreSQL 13+** and **MySQL 8.0.18+**, on macOS, Linux and Windows.

---

- [Install](#install)
- [Quick start](#quick-start)
- [What your setup needs](#what-your-setup-needs)
- [Configuration](#configuration) · [Authentication](#authentication) · [Fixtures](#fixtures)
- [Commands](#commands)
- [Reading the report](#reading-the-report)
- [What routeperf changes on your database](#what-routeperf-changes-on-your-database)
- [CI](#ci)
- [Troubleshooting](#troubleshooting)
- [Development](#development) · [Publishing releases](#publishing-releases-maintainers)

---

## Install

### macOS / Linux: one-line installer

```bash
curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | sh
```

The installer:
- picks your OS and CPU
- downloads the latest release and verifies its checksum
- installs it to `/usr/local/bin`, or to `~/.local/bin` if `/usr/local/bin` isn't writable

You can change its behaviour with these variables:

| Variable | Effect |
|---|---|
| `ROUTEPERF_VERSION=v0.2.0` | install a specific version (default: latest) |
| `ROUTEPERF_BIN=perfcheck` | install under **your own command name** |
| `ROUTEPERF_INSTALL_DIR=~/bin` | install location |
| `ROUTEPERF_BASE_URL=https://artifacts.mycorp/routeperf` | download from an internal mirror instead of GitHub |

For example, to install as `perfcheck` in `~/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | ROUTEPERF_BIN=perfcheck ROUTEPERF_INSTALL_DIR=~/bin sh
```

### Windows

Download `routeperf_windows_amd64.zip` (or `_arm64`) from the [releases page](https://github.com/nazishasghar/routeperf/releases). Unzip it, then put `routeperf.exe` in a folder on your `PATH`.

### With Go (1.26+)

```bash
go install github.com/nazishasghar/routeperf/cmd/routeperf@latest     # → $(go env GOPATH)/bin/routeperf
```

### From source

```bash
git clone https://github.com/nazishasghar/routeperf && cd routeperf
make install                      # → ~/.local/bin/routeperf
make install BIN=perfcheck        # same, as the command `perfcheck`
make install PREFIX=/usr/local    # → /usr/local/bin (may need sudo)
```

### Check it, add tab-completion

```bash
routeperf version

# zsh
routeperf completion zsh > "${fpath[1]}/_routeperf"
# bash
routeperf completion bash | sudo tee /etc/bash_completion.d/routeperf >/dev/null
# fish
routeperf completion fish > ~/.config/fish/completions/routeperf.fish
```

**Update:** re-run the installer (or `go install …@latest`). **Uninstall:** delete the binary (`rm "$(command -v routeperf)"`), or run `make uninstall`.

---

## Quick start

Run these from your API project's folder:

```bash
routeperf init     # asks for spec URL, API URL, DB URL and auth; writes routeperf.yaml
routeperf check    # verifies every connection and tells you exactly what is wrong
routeperf run      # measures every endpoint; prints the table and writes the reports
```

`init` asks for:

```
Swagger / OpenAPI URL or file: http://localhost:3000/swagger.json
API base URL [http://localhost:3000]:
Database URL (postgres://user:pass@localhost:5432/db or mysql://user:pass@127.0.0.1:3306/db): postgres://me@localhost:5432/app_dev
How does the API authenticate? (comma-separated numbers)
  1) Bearer token  2) Cookies  3) Custom headers  4) Login endpoint  5) OAuth2 client credentials  6) None
Choice [6]: 1
Bearer token (hidden):
Store secrets (tokens, passwords, cookies) in the file? (No = use env vars) [y/N]: n
saved routeperf.yaml
Set these environment variables before running:
  export RP_BEARER='…'
```

`check` reports on each prerequisite, and prints a fix line for each problem:

```
  ✓ API reachable          TCP connect to localhost:3000 succeeded
  ✓ Spec                   "Shop API" OpenAPI 3.0.3 — 42 operations (25 read, 17 write)
  ✓ DB connect             postgres 16.2, database "app_dev"
  ✓ SQL capture            statement log readable: /opt/homebrew/var/log/postgresql@16.log
  ✗ App → DB link          all probe requests were rejected: /orders 401, /users 401
    fix: credentials are wrong or missing (--token / -H / -b / auth.login)
```

`run` repeats `check` first and won't start until everything passes. Results go to:
- the terminal
- `routeperf-out/report.md`: endpoint table, then per-endpoint details with plans, scaling tables and advice
- `routeperf-out/results.json`: everything, for tooling and CI

You don't need a config file. Every setting can be passed as a flag:

```bash
routeperf run --spec http://localhost:3000/swagger.json --api-url http://localhost:3000 \
  --db-url postgres://me@localhost:5432/app_dev --token "$API_TOKEN"
```

---

## What your setup needs

1. **The API running locally** (or anywhere routeperf can reach), connected to the database you pass as `--db-url`. `check` confirms that the API's SQL really lands in that database.
2. **A Swagger 2.0 or OpenAPI 3.x document**, as a URL or a file (JSON or YAML).
3. **A local, disposable database with realistic bulk data.**
   - Big O is measured from how work grows, so main tables need at least ~10k rows; 100k+ is better.
   - Remote hosts are refused unless you pass `--allow-remote-db`.
4. **Database privileges** to switch on statement logging for the duration of the run:

| | Postgres | MySQL |
|---|---|---|
| Version | 13+ | 8.0.18+ (also 9.x) |
| Privilege | superuser (`ALTER SYSTEM`, `pg_reload_conf`) | `SYSTEM_VARIABLES_ADMIN` or `SUPER` (root works) |
| How SQL is captured | server stderr log; auto-detected for Homebrew, Linux packages, `logging_collector`, and Docker containers publishing the port | `general_log` → `mysql.general_log` table |
| If auto-detect fails | `--pg-log-file /path/to/postgres.log` or `--pg-log-file docker:<container>` | – |

**Postgres in Docker:** if a container publishes the DB port (e.g. `-p 5432:5432`), routeperf follows its logs with `docker logs -f` automatically. The `postgres` user in the official image is a superuser.

---

## Configuration

Precedence is **flags > `routeperf.yaml` > environment**. The config file is read from the current directory; override it with `--config` or `ROUTEPERF_CONFIG`. Every value supports `${ENV_VAR}` and `${ENV_VAR:-default}`.

A fully commented template is in [`routeperf.example.yaml`](routeperf.example.yaml); `routeperf init` writes one for you. The most-used keys:

```yaml
spec: http://localhost:3000/swagger.json
api: { base_url: http://localhost:3000, timeout: 30s }
db:  { url: "${DATABASE_URL}" }

run:
  methods: [GET, HEAD, POST, PUT, PATCH, DELETE]
  warmup: 2
  iterations: 10            # timed requests per endpoint
  include: []               # operationIds or tag:<name>
  exclude: []
  dangerous_ops: []         # collection DELETEs / reset-style endpoints run only if listed here

scale:
  data_steps: [0.01, 0.03, 0.10, 0.30, 1.0]   # data subsets for growth in n
  k_steps: [1, 10, 100, 250, 500, 1000]       # values for limit/page_size params & bulk arrays
  repeats: 3

writes:
  snapshot: tables          # tables (copy + restore) | none

thresholds:                 # turn routes into FAIL/WARN
  p95_ms: 300
  max_queries_per_request: 10
```

Commit a `routeperf.example.yaml` for your team. Keep `routeperf.yaml` out of git if it contains anything personal (the shipped `.gitignore` already excludes it).

### Authentication

Use any combination of the methods below. routeperf reads the spec's `securitySchemes` and sends whatever satisfies each operation. `check` shows which schemes are covered.

```yaml
auth:
  bearer: ${API_TOKEN}                           # Authorization: Bearer …   (flag: --token)
  headers: { X-Api-Key: "${API_KEY}", X-Tenant-Id: acme }   # (flag: -H "Name: value", repeatable)
  cookies: { session: "${SESSION}" }             # (flag: -b "name=value", repeatable)
  cookie_jar: ./cookies.txt                      # Netscape/curl format (flag: --cookie-jar)
  query: { api_key: "${API_KEY}" }               # API keys sent in the query string

  login:                                         # log in once before the run
    method: POST
    path: /auth/login
    body: { email: perf@test.dev, password: "${PERF_PASSWORD}" }
    extract:
      cookies: true                              # keep the session cookie it sets
      bearer_from: $.data.accessToken            # and/or use a token from the JSON response
    refresh_on: [401]                            # log in again when a request gets 401

  oauth2_client_credentials:                     # machine-to-machine tokens, refreshed automatically
    token_url: http://localhost:3000/oauth/token
    client_id: ${CLIENT_ID}
    client_secret: ${CLIENT_SECRET}
    scope: "read write"
```

Credential values are masked (`***`) in every report and log.

### Fixtures

routeperf fills in request inputs automatically, in this order:
1. Spec `example` / `default` / `enum` values.
2. Real IDs sampled from your database. It maps `{id}` on `/users/{id}`, or `user_id`, to `users.id`, and picks rows that exist at every data scale.
3. Values generated from the schema.

To control inputs yourself, add `fixtures.yaml`, keyed by `operationId`:

```yaml
getUserOrders:
  path:  { id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1" }   # {anchor} = a row present at every data scale
  query: { status: paid }
createOrder:
  body:
    user_id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1"
    items: [{ sku: ABC-1, qty: 2 }]
```

Inputs that routeperf generated without a source are listed at the end of `report.md`.

---

## Commands

| Command | What it does |
|---|---|
| `routeperf init` | Interactive setup; writes `routeperf.yaml` (secrets as env-var references unless you choose otherwise) |
| `routeperf check` | Verifies spec, API, auth, DB, privileges, SQL capture and the API→DB link, and says what to fix. Exit code 1 if anything fails |
| `routeperf discover` | Lists the operations that will run and the ones skipped (with the reason) |
| `routeperf run` | Full run: check → requests → capture → EXPLAIN replay at every scale → restore → report |
| `routeperf report results.json` | Re-prints the table and re-writes the Markdown report from a saved JSON |
| `routeperf repair` | Cleans up after a killed run: log settings, data snapshot, temporary schemas |
| `routeperf version` | Version info |
| `routeperf completion zsh\|bash\|fish\|powershell` | Shell completion |

Useful `run` flags:

| Flag | Effect |
|---|---|
| `--no-writes` | GET/HEAD only |
| `-y`, `--yes` | don't ask before running write operations (required in non-interactive shells) |
| `--only <ids>`, `--exclude <ids>` | operationIds or `tag:<name>` |
| `--no-scale` | skip the scaling experiments; plan-only Big O, faster but lower confidence |
| `-n 20` | timed requests per endpoint |
| `-o dir` | output directory |
| `--ci` | exit code 2 if any endpoint FAILs |
| `--non-interactive` | never prompt |
| `--pg-plan-mode auto\|custom\|generic` | Postgres replay plans. `auto` (default) detects when the app's prepared statements run a generic plan and replays the same way |

Global flags: `--spec --api-url --db-url --token -H -b --cookie-jar --pg-log-file --pg-plan-mode --config --allow-remote-db -v`.

---

## Reading the report

| Column | Meaning |
|---|---|
| P50 / P95 | HTTP latency over the timed requests |
| DB | DB execution time per request. Postgres reports what the app actually spent; MySQL's is estimated from replays |
| Q/REQ | SQL statements per request. `k + 1` means one query plus one per returned item: an **N+1** |
| ROWS/REQ | rows the database examined per request |
| BIG O | how the endpoint's cost grows (see below) |
| CONF | confidence of the Big O: `high` / `medium` / `low` |
| STATUS | `FAIL` (grows with table size, or a threshold was exceeded), `WARN` (N+1, app-side superlinear work, errors), `OK` |

**Big O symbols:**
- `n_orders`: the number of rows in table `orders`
- `k`: the size of the output you ask for (`limit` / page size), or the number of items in a bulk body

So `O(k·log n_order_items + n_orders)` means: one full scan of `orders`, plus `k` index lookups into `order_items`.

**How the estimate is made.** routeperf combines three signals:
1. **Data scale.** Each query is replayed on nested 1%, 3%, 10%, 30% and 100% copies of your data, with foreign keys kept consistent. A curve fit of *rows examined* against table size gives the polynomial degree. Rows examined is deterministic, unlike time.
2. **Plan.** The `EXPLAIN` tree provides the log factors and names the table responsible.
3. **Output scale.** Requests are repeated with growing `limit` or body sizes. This catches N+1 queries and app-side work such as an O(k²) loop in code.

Confidence is high when the signals agree, the fit is tight (R² ≥ 0.95), the data spans 1.5+ orders of magnitude, and the plan stays the same across scales. When the planner switches plans at some data size, the report says where.

`report.md` also projects p50 at 10× and 100× your current data, from the fitted curves.

**Background traffic.** Before the run and before each endpoint, routeperf watches the database while it sends nothing. Any SQL seen in that window comes from background workers, cron jobs or other clients. Those query shapes are left out of every endpoint's numbers, and the report says what was excluded.

**Prepared statements (Postgres).** Apps that use prepared statements switch to a *generic* plan after a few executions. routeperf compares the app's real timing with its replay. When they differ by more than 3×, it replays with the generic plan too and notes this in the report. Force a mode with `--pg-plan-mode`.

**Index advice:**
- It is ready to run on your database: named indexes valid on both Postgres and MySQL (`CREATE INDEX idx_orders_user_id_created_at ON orders (user_id, created_at DESC);`).
- It is checked against your existing indexes. When an index already covers the filter but wasn't used, routeperf suggests `ANALYZE` instead of a duplicate index. When an existing index covers only part of the filter, it says which one to replace.

---

## What routeperf changes on your database

Everything below is temporary and undone at the end of the run, including when you press Ctrl-C.

- **Statement logging.**
  - Postgres: `log_min_duration_statement=0`, `log_parameter_max_length=-1` and `log_line_prefix`, set via `ALTER SYSTEM`. The previous values are restored afterwards.
  - MySQL: `general_log=ON` and `log_output=TABLE`, restored afterwards.
- **Data subsets:** temporary schemas (Postgres) or databases (MySQL) named `_rp_s01`, `_rp_s03`, `_rp_s10`, `_rp_s30`, dropped after the run. They need about 45% extra disk for the tables involved.
- **Write endpoints (POST/PUT/PATCH/DELETE):**
  - routeperf asks before running them (skip the prompt with `--yes`; skip writes entirely with `--no-writes`).
  - It copies every table into `_rp_snap` first.
  - Afterwards it restores the tables that changed (sequences and auto-increment counters included), checks the row counts, and drops the copy.
- **`EXPLAIN ANALYZE` replays** run inside transactions that are always rolled back.

If a run is killed with `kill -9` or a crash, run `routeperf repair`.

> **Never point routeperf at production.** `EXPLAIN ANALYZE` executes queries and write endpoints commit data. Use a local or disposable database.

---

## CI

```yaml
# GitHub Actions example: API + seeded Postgres in services, then:
- run: curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | sh
- run: routeperf run --non-interactive --yes --ci -o perf
  env:
    ROUTEPERF_CONFIG: routeperf.ci.yaml
    API_TOKEN: ${{ secrets.PERF_API_TOKEN }}
- uses: actions/upload-artifact@v4
  with: { name: routeperf-report, path: perf/ }
```

`--ci` exits with code 2 when any endpoint is `FAIL`. Set `thresholds:` in your config to decide what counts as a failure.

---

## Troubleshooting

| `check` says | Fix |
|---|---|
| `API reachable ✗ connection refused` | Start the API, or fix `--api-url` |
| `Spec ✗ 401` | The spec endpoint needs auth: configure auth (it is applied to the spec request too), or pass a local file |
| `DB connect ✗ database "x" does not exist` | Fix the database name in `--db-url` |
| `DB privileges ✗ role is not superuser` (Postgres) | Use a superuser on the local DB, e.g. `postgres://postgres@localhost:5432/app` |
| `SQL capture ✗ cannot locate the Postgres server log` | `--pg-log-file /path/to/server.log`, or `--pg-log-file docker:<container>`, or enable `logging_collector` |
| `SQL capture ✗ probe query not found` | The log file isn't this server's. Check which file the server writes to (Homebrew: `/opt/homebrew/var/log/postgresql@<ver>.log`) |
| `App → DB link ✗ all probe requests were rejected` | Wrong or missing credentials |
| `App → DB link ✗ … issued no SQL to this database` | The API uses a different DB than `--db-url`; compare it with the API's `DATABASE_URL` |
| `Test data ! largest table has 800 rows` | Load more bulk data; growth can't be measured on tiny tables |
| `Previous run ! pending state` | Run `routeperf repair` |
| `Quiet DB ! N statement(s) … while idle` | Background jobs are hitting the DB. routeperf excludes those query shapes, but stopping workers or cron jobs gives the cleanest numbers |
| An endpoint shows many non-2xx responses | Inputs were invalid: add a fixture for that `operationId` |

---

## Limitations

- App code that does CPU work without touching the database is visible only through the output-scale test, where it shows up as app time growing with `k`.
- Caches in the app (Redis, ORM identity maps) can hide queries on repeated requests.
- Exponential growth isn't modelled. Big O is an empirical estimate with a confidence level, not a proof.
- One request is in flight at a time, because that's how SQL is attributed to endpoints. Background SQL is detected and excluded by query shape. An endpoint that runs exactly the same query shape as a background job can lose that statement from its count.

---

## Development

```bash
make build          # bin/routeperf
make test           # go vet + unit tests
# end-to-end: seeds the testbed in both DBs, runs routeperf, asserts every planted problem + fix
RP_IT_PG=postgres://localhost:5432/routeperf_it RP_IT_MYSQL=mysql://root@127.0.0.1:3306/routeperf_it \
  go test -tags integration -v -timeout 20m ./integration/
make testbed        # sample API with planted problems, for end-to-end checks
make testbed-seed DB=postgres://localhost:5432/routeperf_testbed
bin/testbed --db-url postgres://localhost:5432/routeperf_testbed --addr :8088 &
bin/routeperf run --spec http://localhost:8088/swagger.json --api-url http://localhost:8088 \
  --db-url postgres://localhost:5432/routeperf_testbed --token testtoken
```

The testbed supports MySQL as well (`--db-url mysql://root@127.0.0.1:3306/routeperf_testbed`). It accepts `Bearer testtoken`, `X-Api-Key: testkey`, cookie `session=testsession`, or `POST /auth/login` with `{"email":"perf@test.dev","password":"secret"}`. Its planted problems are:
- background job noise (`--background-noise 120ms`)
- UUID primary keys (`/notes`)
- an unindexed filter
- an N+1 query
- an unindexed FK cascade
- a full-table aggregate
- an INSERT per item
- an O(k²) loop in the app

Layout:

```
cmd/routeperf/        CLI (cobra): commands, prompts
internal/spec         Swagger 2.0 / OpenAPI 3 → operation catalog
internal/auth         bearer, headers, cookies, cookie jar, login, OAuth2; scheme mapping; redaction
internal/inputs       request building: fixtures, examples, anchor IDs, generated values
internal/db           Postgres + MySQL: catalog, log capture, EXPLAIN, subsets, snapshot/restore
internal/plan         plan model + Postgres JSON / MySQL TREE parsers
internal/analyze      curve fitting, plan algebra, verdicts, advisor
internal/runner       doctor (check) and the run lifecycle
internal/report       terminal, Markdown, JSON
testbed/              sample API with planted problems
integration/          end-to-end test against real Postgres + MySQL (go test -tags integration)
```

The design and the Big O method are described in [PLAN.md](PLAN.md).

## Publishing releases (maintainers)

1. **One time:** point the module path and docs at your repository, then push.
   ```bash
   scripts/set-repo.sh github.com/<owner>/routeperf
   git init && git add -A && git commit -m "routeperf" && git remote add origin git@github.com:<owner>/routeperf.git && git push -u origin main
   ```
2. **Release:** tag a version. The `release` workflow runs GoReleaser and publishes macOS, Linux and Windows archives (amd64 and arm64) plus `checksums.txt`. The installer and `go install` pick it up.
   ```bash
   git tag v0.1.0 && git push --tags
   ```
3. **Repository look:** README images live in `docs/assets/` (regenerate the SVGs with `python3 scripts/gen-images.py`). For link previews, upload `docs/assets/social-preview.png` under *Settings → General → Social preview*.
4. **Optional:**
   - **Homebrew:** create `<owner>/homebrew-tap` and uncomment `brews:` in `.goreleaser.yaml`. Users then run `brew install <owner>/tap/routeperf`.
   - **No GitHub:** run `make release` and upload `dist/*` to any HTTP server. Users install with `ROUTEPERF_BASE_URL=https://your-server/path sh install.sh`.
