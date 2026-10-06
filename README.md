<p align="center">
  <img src="docs/assets/banner.svg" alt="routeperf — per-endpoint performance and Big O for your API" width="100%">
</p>

<p align="center">
  <a href="https://github.com/nazishasghar/routeperf/releases/latest"><img alt="Release" src="https://img.shields.io/github/v/release/nazishasghar/routeperf?color=7c3aed&label=release"></a>
  <a href="https://github.com/nazishasghar/routeperf/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/nazishasghar/routeperf/actions/workflows/ci.yml/badge.svg"></a>
  <img alt="Go" src="https://img.shields.io/github/go-mod/go-version/nazishasghar/routeperf?color=00ADD8">
  <img alt="PostgreSQL 13+" src="https://img.shields.io/badge/PostgreSQL-13%2B-336791?logo=postgresql&logoColor=white">
  <img alt="MySQL 8.0.18+" src="https://img.shields.io/badge/MySQL-8.0.18%2B-4479A1?logo=mysql&logoColor=white">
  <img alt="OpenAPI · GraphQL · gRPC" src="https://img.shields.io/badge/OpenAPI%20%C2%B7%20GraphQL%20%C2%B7%20gRPC-555">
  <a href="LICENSE"><img alt="MIT license" src="https://img.shields.io/badge/license-MIT-555"></a>
</p>

<p align="center">
  <b>Point it at your API (OpenAPI, GraphQL or gRPC), the running service and its database.<br>
  Get latency, DB time, queries per request, an estimated Big O and a proven fix for every endpoint.</b>
</p>

<p align="center">
  <a href="#install">Install</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#reading-the-report">Reading the report</a> ·
  <a href="#ci">CI</a> ·
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
  <img src="docs/assets/how-it-works.svg" alt="1 read the API description, 2 call every route, 3 capture the SQL, 4 replay with EXPLAIN ANALYZE on 1%→100% data subsets and one table at a time, 5 Big O and proven fixes" width="100%">
</p>

For each endpoint, routeperf:
- sends real requests, using your token, cookies or headers
- captures the SQL it runs, from the database's statement log or through a wire proxy
- replays that SQL with `EXPLAIN ANALYZE` on 1%→100% copies of your data, and one table at a time, so the growth curve shows the Big O and which table drives it
- reports what to fix (missing indexes, N+1 queries, unindexed foreign keys, O(k²) loops in app code) and proves index fixes with hypothetical indexes before suggesting them

Works with **PostgreSQL 13+** and **MySQL 8.0.18+**, for **OpenAPI/Swagger**, **GraphQL** and **gRPC** APIs, on macOS, Linux and Windows.

---

- [Install](#install)
- [Quick start](#quick-start)
- [What your setup needs](#what-your-setup-needs) · [No admin rights? Proxy capture](#no-admin-rights-proxy-capture)
- [Configuration](#configuration) · [Authentication](#authentication) · [Inputs and fixtures](#inputs-and-fixtures)
- [GraphQL and gRPC](#graphql-and-grpc)
- [Commands](#commands)
- [Reading the report](#reading-the-report)
- [Cold cache](#cold-cache) · [Load mode](#load-mode) · [Sharing reports safely](#sharing-reports-safely)
- [What routeperf changes on your database](#what-routeperf-changes-on-your-database)
- [CI](#ci)
- [Troubleshooting](#troubleshooting) · [Limitations](#limitations)
- [Development](#development) · [Publishing releases](#publishing-releases-maintainers) · [License](#license)

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
| `ROUTEPERF_VERSION=v0.3.0` | install a specific version (default: latest) |
| `ROUTEPERF_BIN=perfcheck` | install under **your own command name** |
| `ROUTEPERF_INSTALL_DIR=~/bin` | install location |
| `ROUTEPERF_BASE_URL=https://artifacts.mycorp/routeperf` | download from an internal mirror instead of GitHub |

For example, to install as `perfcheck` in `~/bin`:

```bash
curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | ROUTEPERF_BIN=perfcheck ROUTEPERF_INSTALL_DIR=~/bin sh
```

### Homebrew (macOS)

```bash
brew install nazishasghar/tap/routeperf
```

The cask is published by the release workflow once the tap repository exists (see [Publishing releases](#publishing-releases-maintainers)).

### Windows

Download `routeperf_windows_amd64.zip` (or `_arm64`) from the [releases page](https://github.com/nazishasghar/routeperf/releases). Unzip it, then put `routeperf.exe` in a folder on your `PATH`.

### With Go (1.26+)

```bash
go install github.com/nazishasghar/routeperf/cmd/routeperf@latest     # → $(go env GOPATH)/bin/routeperf
```

The binary is pure Go (no cgo). The SQL parsers (libpg_query compiled to WebAssembly, and Vitess for MySQL) are built in.

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

**Update:** re-run the installer, `brew upgrade routeperf`, or `go install …@latest`. **Uninstall:** delete the binary (`rm "$(command -v routeperf)"`), or run `make uninstall`.

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
  ✓ Spec                   "Shop API" OpenAPI 3.0.3 — 42 operations (25 read, 17 write), 6 response links
  ✓ DB connect             postgres 16.2, database "app_dev"
  ✓ SQL capture            statements readable: /opt/homebrew/var/log/postgresql@16.log
  ✗ App → DB link          all probe requests were rejected: /orders 401, /users 401
    fix: credentials are wrong or missing (--token / -H / -b / auth.login)
```

`run` repeats `check` first and won't start until everything passes. Results go to:
- the terminal
- `routeperf-out/report.html`: one self-contained page to share, with a sortable endpoint table, growth charts, plans and fixes
- `routeperf-out/report.md`: the same in Markdown, for PRs and wikis
- `routeperf-out/results.json`: everything, for tooling, CI and `routeperf diff`

You don't need a config file. Every setting can be passed as a flag:

```bash
routeperf run --spec http://localhost:3000/swagger.json --api-url http://localhost:3000 \
  --db-url postgres://me@localhost:5432/app_dev --token "$API_TOKEN"
```

---

## What your setup needs

1. **The API running** where routeperf can reach it, connected to the database you pass as `--db-url`. `check` confirms that the API's SQL really lands in that database.
2. **An API description**: a Swagger 2.0 / OpenAPI 3.x document (URL or file, JSON or YAML), a GraphQL endpoint or SDL file, or a gRPC server with reflection (or its `.proto` files). See [GraphQL and gRPC](#graphql-and-grpc).
3. **A disposable database with realistic bulk data.**
   - Big O is measured from how work grows, so main tables need at least ~10k rows; 100k+ is better.
   - Remote hosts are refused unless you pass `--allow-remote-db`.
4. **A way to see the SQL.** Pick one:

| | Statement log (default) | Wire proxy (`--capture proxy`) |
|---|---|---|
| Postgres | superuser (`ALTER SYSTEM`, `pg_reload_conf`), server log readable | any user that can run the app's queries |
| MySQL | `SYSTEM_VARIABLES_ADMIN` or `SUPER` (root works) | any user that can run the app's queries |
| Works with RDS / Cloud SQL / shared servers | no | yes |
| App changes | none | point its database URL at the proxy |
| DB time per statement | Postgres log durations; MySQL `performance_schema` | measured on the wire |

| Statement log details | Postgres | MySQL |
|---|---|---|
| Version | 13+ | 8.0.18+ (also 9.x) |
| How SQL is captured | server stderr log; auto-detected for Homebrew, Linux packages, `logging_collector`, and Docker containers publishing the port | `general_log` → `mysql.general_log` table, plus `performance_schema` for real per-statement time and rows examined |
| If auto-detect fails | `--pg-log-file /path/to/postgres.log` or `--pg-log-file docker:<container>` | – |

**Postgres in Docker:** if a container publishes the DB port (e.g. `-p 5432:5432`), routeperf follows its logs with `docker logs -f` automatically. The `postgres` user in the official image is a superuser. CI exercises this path on every push.

### No admin rights? Proxy capture

`routeperf proxy` sits between your app and its database, speaks the Postgres and MySQL wire protocols, and records every statement with its bind parameters and timing. It needs no admin rights and no log access, so it works on RDS, Cloud SQL and shared dev servers.

```bash
# terminal 1: start the proxy in front of the real database
routeperf proxy --db-url postgres://app@db.internal:5432/app --allow-remote-db
#   routeperf proxy listening on 127.0.0.1:6543 (control 127.0.0.1:6544)
#   point the app at:  postgres://app:***@127.0.0.1:6543/app?sslmode=disable

# terminal 2: restart the app with that DATABASE_URL, then
routeperf run --capture proxy --db-url postgres://app@db.internal:5432/app --allow-remote-db
```

- The proxy keeps running between runs, so the app's connection pool stays connected. Without a running `routeperf proxy`, `run --capture proxy` starts one itself for apps that connect lazily.
- App ↔ proxy traffic is plain TCP on localhost. For Postgres, proxy → server uses TLS when the DB URL says `sslmode=require` (or `verify-*`).
- routeperf still connects to the database directly for `EXPLAIN` replays. Data subsets need the `CREATE` privilege; without it routeperf falls back to plan-only Big O and says so.

---

## Configuration

Precedence is **flags > `routeperf.yaml` > environment**. The config file is read from the current directory; override it with `--config` or `ROUTEPERF_CONFIG`. Every value supports `${ENV_VAR}` and `${ENV_VAR:-default}`.

A fully commented template is in [`routeperf.example.yaml`](routeperf.example.yaml); `routeperf init` writes one for you. The most-used keys:

```yaml
spec: http://localhost:3000/swagger.json   # or a GraphQL endpoint / .graphql file, or grpc://host:port / .proto files
api: { base_url: http://localhost:3000, timeout: 30s }
db:
  url: "${DATABASE_URL}"
  schemas: []                 # MySQL: other databases the app reads (Postgres sees every schema)

capture:
  mode: log                   # log | proxy
  traceparent: true           # send W3C traceparent; SQL tagged by sqlcommenter is attributed by trace id

run:
  methods: [GET, HEAD, POST, PUT, PATCH, DELETE]
  warmup: 2
  iterations: 10              # timed requests per endpoint
  include: []                 # operationIds, tag:<name>, "METHOD /path" or path globs (/admin/**)
  exclude: []
  dangerous_ops: []           # collection DELETEs / reset-style endpoints run only if listed here
  # as: [admin, driver]       # run every operation once per auth role (see Several user types)

scale:
  data_steps: [0.01, 0.03, 0.10, 0.30, 1.0]   # data subsets for growth in n
  k_steps: [1, 10, 100, 250, 500, 1000]       # values for limit/page_size params & bulk arrays
  repeats: 3                  # minimum repeats per point
  max_repeats: 12             # keep repeating until the slope's 95% CI is within ci_target
  ci_target: 0.1
  per_table: true             # also shrink one table at a time for multi-table queries

cache:
  cold: false                 # also measure first hits after evicting the tables from the cache
  # cold_cmd: "sync && sudo purge"

advice:
  verify: true                # prove index advice with HypoPG when it's installed (Postgres)

load:
  enabled: false
  concurrency: [1, 4, 16, 32]
  duration: 5s

writes:
  snapshot: tables            # tables (copy + restore) | none

thresholds:                   # turn routes into FAIL/WARN
  p95_ms: 300
  max_queries_per_request: 10

report:
  mask_literals: false        # replace SQL literals and parameters with ? in every report
  html: true
```

Commit a `routeperf.example.yaml` for your team. Keep `routeperf.yaml` out of git if it contains anything personal (the shipped `.gitignore` already excludes it).

### Authentication

Use any combination of the methods below. routeperf reads the spec's `securitySchemes` and sends whatever satisfies each operation. `check` shows which schemes are covered.

```yaml
auth:
  bearer: ${API_TOKEN}                           # Authorization: Bearer …   (flag: --token)
  bearer_command: gcloud auth print-access-token # run a command for the token; re-run on 401 (flag: --token-cmd)
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

`bearer_command` works with any CLI that prints a token: `gcloud auth print-access-token`, `aws sso …`, `az account get-access-token --query accessToken -o tsv`, `vault read -field=token …`. Credential values are masked (`***`) in every report and log.

#### Several user types (roles)

When different endpoints need different users (admin, customer, driver…), give each user type its own credentials under `auth.roles`, and list the operations it calls in `ops`:

```yaml
auth:
  bearer: ${CUSTOMER_TOKEN}          # default identity: operations no role claims

  roles:
    admin:                           # any auth method above works inside a role
      login:
        path: /auth/login
        body: { email: admin@test.dev, password: "${ADMIN_PASSWORD}" }
        extract: { bearer_from: $.token }
      ops: [tag:admin, "/admin/**"]
    driver:
      bearer: ${DRIVER_TOKEN}
      ops: ["/drivers/**", "GET /trips/*", listAssignedOrders]
```

- `ops` takes operationIds, `tag:<name>`, `"METHOD /path"` and path globs: `*` matches within one path segment, `**` across segments (`/admin/**` also matches `/admin`), and a method may be `*`. `"*"` alone means every operation.
- An operation that no role's `ops` matches uses the top-level credentials. An operation that several roles match runs **once per role**. The same endpoint often runs different SQL for an admin (every row) than for a customer (their own rows), so each role gets its own line in the report: a `ROLE` column in the terminal and Markdown, and `[role]` in findings, `results.json` and `routeperf diff`.
- Each role has its own login, cookie jar, 401 refresh and request state. IDs created by one role's `POST` are deleted by that same role, and links and cursors don't leak between roles.
- To give one role its own inputs, key a fixture `<operationId>@<role>` (for example `getTrip@driver`). Without one, the role uses the plain `<operationId>` fixture.
- `routeperf check` logs every role in, shows how many operations each one calls, and warns when a role's `ops` match nothing.

From the command line, `--role name=TOKEN` adds or overrides a role's bearer token (repeatable), and `--as admin,driver` runs **every** selected operation once per listed role, ignoring `ops` (`default` means the top-level credentials). For example, to compare one endpoint as two users:

```bash
routeperf run --role admin="$ADMIN_TOKEN" --role driver="$DRIVER_TOKEN" --as admin,driver --only "GET /orders"
```

### Inputs and fixtures

routeperf fills in request inputs automatically, in this order:
1. Spec `example` / `default` / `enum` values.
2. **OpenAPI links.** Values from earlier responses that the spec links to later operations (`POST /notes` → `$response.body#/id` → `DELETE /notes/{id}`). Writes use them to update and delete the resources routeperf created.
3. Real IDs sampled from your database. It maps `{id}` on `/users/{id}`, or `user_id`, to `users.id` (and `/archive/orders/{id}` to `archive.orders`), and picks rows that exist at every data scale.
4. Values generated from the schema.

Request bodies are sent as JSON, `application/x-www-form-urlencoded` or `multipart/form-data` (file fields with `format: binary` get a small generated file), whichever the spec declares first.

**Cursor pagination.** When an operation takes a cursor parameter (`cursor`, `after`, `page_token`, `starting_after`, …), the timed requests follow the cursor from each response (`next_cursor`, `meta.next_cursor`, `pageInfo.endCursor`, a `next` URL, or a `Link: rel="next"` header), so deep pages are measured too, not just page one.

To control inputs yourself, add `fixtures.yaml`, keyed by `operationId` (or `operationId@role` for one [role](#several-user-types-roles)):

```yaml
getUserOrders:
  path:  { id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1" }   # {anchor} = a row present at every data scale
  query: { status: paid }
createOrder:
  body:
    user_id: "sql:SELECT id FROM users WHERE {anchor} LIMIT 1"
    items: [{ sku: ABC-1, qty: 2 }]
```

Inputs that routeperf generated without a source are listed at the end of `report.md`. A `sql:` fixture that finds no row (or only NULL) is never sent as-is: the input falls back to the sources above and the report warns which fixture missed.

### Databases with several schemas, partitions and views

- **Schemas.** Postgres tables in every schema are cataloged; a name that exists in two schemas is reported as `schema.table` (e.g. `n_archive.orders`). For MySQL, list the other databases the app reads with `db.schemas` / `--db-schemas`.
- **Composite keys.** Primary and foreign keys with several columns keep data subsets consistent and get multi-column index advice.
- **Circular foreign keys.** Self-references (`category.parent_id`) and tables that reference each other (`customer.primary_member_id` ⇄ `member.customer_id`) can't all be followed when sampling, so one reference per cycle is left as-is — the nullable one where there is one — and the rest keep subsets consistent. `routeperf check` lists which ones.
- **Partitioned tables.** Subsets keep the partitioning (so the planner prunes as it does on the real table), and scans of individual partitions are reported against the parent table.
- **Views.** Subsets recreate the views over the subset tables, so a query through a view is measured at every scale.

---

## GraphQL and gRPC

routeperf measures GraphQL and gRPC APIs the same way: every operation is called, its SQL captured and replayed at scale.

**GraphQL.** Point `--spec` at the endpoint (schema via introspection) or a `.graphql` SDL file:

```bash
routeperf run --spec http://localhost:4000/graphql --db-url postgres://me@localhost:5432/app --token "$API_TOKEN"
routeperf run --spec schema.graphql --api-url http://localhost:4000 --db-url …    # endpoint: api.graphql_path (default /graphql)
```

- Every `Query` field is a read, every `Mutation` field a write (`QUERY users`, `MUTATION createUser` in the report).
- The generated document selects scalar fields two levels deep, so nested resolvers run. That's where GraphQL N+1s live: `users(first: k) { notes { … } }` shows up as `k + 1` queries per request.
- Arguments such as `first`, `limit`, `pageSize` are the output-size `k`; `id`/`userId` arguments get real IDs from the database.
- A response with `errors` counts as a failed request (400, or 401/403 from the error code).

**gRPC.** Use server reflection, or pass the `.proto` files:

```bash
routeperf run --spec grpc://localhost:50051 --db-url postgres://me@localhost:5432/app --token "$API_TOKEN"
routeperf run --spec api/shop.proto,api/common.proto --api-url grpc://localhost:50051 --db-url …
```

- Every unary method is an operation (`RPC Shop/ListUserOrders`); streaming methods are skipped. Methods named `Get…`, `List…`, `Search…`, `Find…` count as reads.
- Requests are built from the message schema; `page_size`/`limit` fields are `k`, `*_id` fields get real IDs.
- Auth goes in metadata: the bearer token as `authorization`, custom headers as metadata keys.
- Use `grpcs://` (or `api.grpc.tls: true`) for TLS.

---

## Commands

| Command | What it does |
|---|---|
| `routeperf init` | Interactive setup; writes `routeperf.yaml` (secrets as env-var references unless you choose otherwise) |
| `routeperf check` | Verifies spec, API, auth, DB, privileges, SQL capture and the API→DB link, and says what to fix. Exit code 1 if anything fails |
| `routeperf discover` | Lists the operations that will run and the ones skipped (with the reason) |
| `routeperf run` | Full run: check → requests → capture → EXPLAIN replay at every scale → restore → report |
| `routeperf load` | Concurrent load test per endpoint, without the data-scale experiments ([Load mode](#load-mode)) |
| `routeperf proxy` | Runs the SQL capture proxy ([Proxy capture](#no-admin-rights-proxy-capture)) |
| `routeperf diff base.json new.json` | Compares two runs; exit code 2 when an endpoint got worse ([CI](#ci)) |
| `routeperf report results.json` | Re-prints the table and re-writes `report.md` and `report.html` from a saved JSON (add `--mask-literals` to strip values) |
| `routeperf repair` | Cleans up after a killed run: log settings, data snapshot, temporary schemas |
| `routeperf version` | Version info |
| `routeperf completion zsh\|bash\|fish\|powershell` | Shell completion |

Useful `run` flags:

| Flag | Effect |
|---|---|
| `--no-writes` | GET/HEAD only (GraphQL queries, gRPC reads) |
| `-y`, `--yes` | don't ask before running write operations (required in non-interactive shells) |
| `--only <ids>`, `--exclude <ids>` | operationIds, `tag:<name>`, `"METHOD /path"` or path globs (`/admin/**`) |
| `--no-scale` | skip the scaling experiments; plan-only Big O, faster but lower confidence |
| `--no-per-table` | skip shrinking one table at a time |
| `--cold`, `--cold-cmd "<cmd>"` | also measure cold-cache first hits ([Cold cache](#cold-cache)) |
| `--load`, `--load-concurrency 1,8,32`, `--load-duration 5s` | add a concurrent load test ([Load mode](#load-mode)) |
| `--no-verify` | don't prove index advice with HypoPG |
| `--mask-literals` | replace SQL literals and parameters with `?` in every report |
| `--no-html` | skip `report.html` |
| `-n 20` | timed requests per endpoint |
| `-o dir` | output directory |
| `--ci` | exit code 2 if any endpoint FAILs |
| `--non-interactive` | never prompt |
| `--pg-plan-mode auto\|custom\|generic` | Postgres replay plans. `auto` (default) detects when the app's prepared statements run a generic plan and replays the same way |

Global flags: `--spec --api-url --db-url --db-schemas --token --token-cmd -H -b --cookie-jar --role --as --capture --proxy-listen --protocol --pg-log-file --pg-plan-mode --mask-literals --config --allow-remote-db -v`.

---

## Reading the report

| Column | Meaning |
|---|---|
| P50 / P95 | HTTP (or gRPC) latency over the timed requests, warm cache |
| COLD | first-hit latency after the endpoint's tables were evicted from the cache (with `--cold`) |
| DB | DB execution time per request: Postgres log durations, MySQL `performance_schema`, or wire time through the proxy. The report header names the source |
| Q/REQ | SQL statements per request. `k + 1` means one query plus one per returned item: an **N+1** |
| ROWS/REQ | rows the database examined per request |
| BIG O | how the endpoint's cost grows (see below) |
| CONF | confidence of the Big O: `high` / `medium` / `low` |
| STATUS | `FAIL` (grows with table size, or a threshold was exceeded), `WARN` (N+1, app-side superlinear work, errors), `OK` |

**Big O symbols:**
- `n_orders`: the number of rows in table `orders` (`n_archive.orders` for a table in another schema)
- `k`: the size of the output you ask for (`limit` / page size / `first`), or the number of items in a bulk body

So `O(k·log n_order_items + n_orders)` means: one full scan of `orders`, plus `k` index lookups into `order_items`.

**How the estimate is made.** routeperf combines four signals:
1. **Data scale.** Each query is replayed on nested 1%, 3%, 10%, 30% and 100% copies of your data, with foreign keys kept consistent. A curve fit of *rows examined* against table size gives the polynomial degree. Rows examined is deterministic, unlike time.
2. **One table at a time.** For a query over several large tables, each table is shrunk on its own while the others stay at 100%. That tells which table drives the cost, and whether costs multiply (a nested loop: `O(n_orders·n_users)`) or add (a hash join: `O(n_orders + n_users)`).
3. **Plan.** The `EXPLAIN` tree provides the log factors and names the table responsible.
4. **Output scale.** Requests are repeated with growing `limit` or body sizes. This catches N+1 queries and app-side work such as an O(k²) loop in code.

**Confidence intervals.** Every growth exponent comes with its 95% confidence interval, e.g. `slope 1.02 ± 0.04`. routeperf keeps adding repeats (up to `scale.max_repeats`) until the interval is within `scale.ci_target`, and decides the degree from the interval rather than a fixed cut-off: when exactly one integer lies inside it, that's the degree. App-side `k²` additionally needs the quadratic term to be significant, so a constant overhead can't masquerade as a curve.

Confidence is high when the signals agree, the fit is tight, the data spans 1.5+ orders of magnitude, and the plan stays the same across scales. When the planner switches plans at some data size, the report says where.

`report.md` and `report.html` also project p50 at 10× and 100× your current data, from the fitted curves.

**Proven index advice (Postgres).** When [HypoPG](https://github.com/HypoPG/hypopg) is installed on the server, every `CREATE INDEX` suggestion is checked before it reaches you: routeperf creates the index hypothetically, re-plans the query, and reports `✓ HypoPG: planner cost 2918 → 4.4 with this index (Seq Scan → Index Scan)`. Suggestions the planner wouldn't use are withdrawn and listed as such in the query details. The extension is created inside a rolled-back transaction, so nothing is left behind. Without HypoPG (and on MySQL) advice is reported unproven, and the report says so.

**Index advice** is also:
- ready to run on your database: named indexes valid on both Postgres and MySQL (`CREATE INDEX idx_orders_user_id_created_at ON orders (user_id, created_at DESC);`)
- checked against your existing indexes. When an index already covers the filter but wasn't used, routeperf suggests `ANALYZE` instead of a duplicate index. When an existing index covers only part of the filter, it says which one to replace.

**Background traffic.** Before the run and before each endpoint, routeperf watches the database while it sends nothing. Any SQL seen in that window comes from background workers, cron jobs or other clients. Those query shapes are left out of every endpoint's numbers, and the report says what was excluded.

**Exact attribution with sqlcommenter.** routeperf sends a W3C `traceparent` header with every request. If your app tags its SQL with it ([sqlcommenter](https://google.github.io/sqlcommenter/), built into OpenTelemetry instrumentations for Django, Rails, Spring, Express, database/sql and more), each statement is attributed to the request that caused it by trace id, so other traffic can't leak in, and per-endpoint SQL can be measured even under concurrent load. The report header says when this is active.

**Prepared statements (Postgres).** Apps that use prepared statements switch to a *generic* plan after a few executions. routeperf compares the app's real timing with its replay. When they differ by more than 3×, it replays with the generic plan too and notes this in the report. Force a mode with `--pg-plan-mode`.

**HTML report.** `report.html` is one self-contained file (no external assets) you can attach to a ticket or send to a teammate. It has a sortable, filterable endpoint table, and for each endpoint the fixes (with copy buttons), growth charts (rows examined vs table size, per table; latency vs `k`; throughput and p95 under load) with hover read-outs and table views, and the plans. It follows the reader's light/dark setting.

### Cold cache

Every number is a warm-cache measurement by default, and the report says so. With `--cold`, routeperf also measures each read endpoint's **first hit after its tables were evicted from the database cache**, and replays each query once cold:

- **Postgres 17+:** the endpoint's tables and indexes are evicted from `shared_buffers` with `pg_buffercache_evict` (inside a rolled-back transaction). The OS page cache stays warm; add `--cold-cmd` to drop it too, e.g. `--cold-cmd "sync && sudo purge"` (macOS) or `--cold-cmd "sync && echo 3 | sudo tee /proc/sys/vm/drop_caches"` (Linux).
- **MySQL:** InnoDB can't evict single tables, so cold numbers need `--cold-cmd`, and the report notes that the buffer pool stays warm.

Cold results appear as a `COLD` column, a cold line per endpoint, and per-query `cold … ms (N pages read)`. The report header states exactly what "cold" meant on your setup.

### Load mode

Serial runs can't show lock contention or connection-pool limits. `routeperf run --load` (or `routeperf load` for just this part) drives each endpoint with 1, 4, 16 and 32 concurrent clients for 5 s each while sampling the database (active sessions, connections, lock waits and what they wait on):

```
Under load
  GET  /orders/search   2211 req/s at c=32  p95 34.9ms  throughput stops scaling at concurrency 8 (peak 2211 req/s);
                                                        p95 grows 8×; the DB never sees more than 10 connections:
                                                        likely the app's connection-pool size
```

- Reads only by default; `--load-writes` adds POST/PUT/PATCH endpoints (DELETEs never run under load). Data is restored afterwards as usual.
- With sqlcommenter tags, each step also reports DB time and queries per request under concurrency.

### Sharing reports safely

SQL parameters can contain real user data. `--mask-literals` (or `report.mask_literals: true`) replaces every literal and bind parameter with `?` in the JSON, Markdown, HTML and terminal output, including plans and warnings. `routeperf report results.json --mask-literals` masks a report you already have. Credentials are always masked.

---

## What routeperf changes on your database

Everything below is temporary and undone at the end of the run, including when you press Ctrl-C.

- **Statement logging** (log capture only; the proxy changes nothing).
  - Postgres: `log_min_duration_statement=0`, `log_parameter_max_length=-1` and `log_line_prefix`, set via `ALTER SYSTEM`. The previous values are restored afterwards.
  - MySQL: `general_log=ON` and `log_output=TABLE`, and the `events_statements_current` / `events_statements_history_long` consumers of `performance_schema`; all restored afterwards. Rows already in `mysql.general_log` are set aside in `mysql.rp_general_log_saved` during the run (so capture stays fast) and put back afterwards.
- **Data subsets:** temporary schemas (Postgres) or databases (MySQL) named `_rp_s01` … `_rp_s30` (nested subsets) and `_rp_p01` … `_rp_p30` (one table shrunk at a time), dropped after the run. They need about 45% extra disk for the tables involved.
- **Write endpoints (POST/PUT/PATCH/DELETE, GraphQL mutations, gRPC writes):**
  - routeperf asks before running them (skip the prompt with `--yes`; skip writes entirely with `--no-writes`).
  - It copies every table into `_rp_snap` first.
  - Afterwards it restores the tables that changed (sequences and auto-increment counters included), checks the row counts, and drops the copy.
- **`EXPLAIN ANALYZE` replays** run inside transactions that are always rolled back. So do the HypoPG checks and cache evictions, which also roll back the `CREATE EXTENSION` they need.

If a run is killed with `kill -9` or a crash, run `routeperf repair`.

> **Never point routeperf at production.** `EXPLAIN ANALYZE` executes queries and write endpoints commit data. Use a local or disposable database.

---

## CI

**Fail a PR when an endpoint gets worse.** Run routeperf on the base branch and on the PR, then compare:

```bash
routeperf diff base/results.json pr/results.json -o diff.md
```

```
ENDPOINT                                     BIG O (base)                 BIG O (new)                            P95 ms      Q/REQ
GET /users/{id}/orders                       O(k·log n_order_items)       O(k·log n_order_items + n_o…     9.0→21.4      11→11
    ✗ Big O worse: O(k·log n_order_items) → O(k·log n_order_items + n_orders)
    ✗ p95 +137% (9.0 → 21.4 ms)
    ✗ status WARN → FAIL

✗ 1 of 3 endpoints regressed
```

(Here the PR dropped the index on `orders (user_id, created_at)`.)

`diff` exits with code 2 on a regression: a higher Big O (degree in table size or in `k`, then log factors), p95 up by `--p95-pct` (default 20%) and at least `--p95-min-ms` (default 5 ms), more queries per request, a new N+1, or a worse status. Choose what fails with `--fail-on bigo,p95,queries,nplusone,status,missing`. `-o diff.md` writes a Markdown table for a PR comment.

```yaml
# GitHub Actions example: API + seeded Postgres in services, then:
- run: curl -fsSL https://raw.githubusercontent.com/nazishasghar/routeperf/main/install.sh | sh
- run: routeperf run --non-interactive --yes --ci -o perf
  env:
    ROUTEPERF_CONFIG: routeperf.ci.yaml
    API_TOKEN: ${{ secrets.PERF_API_TOKEN }}
- run: routeperf diff baseline/results.json perf/results.json -o perf/diff.md
- uses: actions/upload-artifact@v4
  if: always()
  with: { name: routeperf-report, path: perf/ }
```

`run --ci` exits with code 2 when any endpoint is `FAIL`. Set `thresholds:` in your config to decide what counts as a failure. Keep a `results.json` from your main branch as the baseline (an artifact of the last main build, or a file you commit).

---

## Troubleshooting

| `check` says | Fix |
|---|---|
| `API reachable ✗ connection refused` | Start the API, or fix `--api-url` |
| `Spec ✗ 401` | The spec endpoint needs auth: configure auth (it is applied to the spec request too), or pass a local file |
| `Spec ✗ introspection …` (GraphQL) | Enable introspection in development, or pass the `.graphql` SDL file |
| `Spec ✗ server reflection …` (gRPC) | Register reflection on the server, or pass the `.proto` files |
| `DB connect ✗ database "x" does not exist` | Fix the database name in `--db-url` |
| `DB privileges ✗ role is not superuser` (Postgres) | Use a superuser on the local DB, e.g. `postgres://postgres@localhost:5432/app`, or use `--capture proxy` |
| `SQL capture ✗ cannot locate the Postgres server log` | `--pg-log-file /path/to/server.log`, or `--pg-log-file docker:<container>`, or enable `logging_collector`, or use `--capture proxy` |
| `SQL capture ✗ probe query not found` | The log file isn't this server's. Check which file the server writes to (Homebrew: `/opt/homebrew/var/log/postgresql@<ver>.log`) |
| `App → DB link ✗ all probe requests were rejected` | Wrong or missing credentials |
| `App → DB link ✗ … issued no SQL to this database` | The API uses a different DB than `--db-url`; compare it with the API's `DATABASE_URL` |
| `App → DB link ✗ … no SQL came through the proxy` | The app isn't connected through the proxy: set its database URL to the one `routeperf proxy` printed and restart it |
| `SQL proxy ✗ address already in use` | Another program uses port 6543 (or 6544 for the control API): `--proxy-listen 127.0.0.1:7543` |
| `Test data ! largest table has 800 rows` | Load more bulk data; growth can't be measured on tiny tables |
| `Previous run ! pending state` | Run `routeperf repair` |
| `Quiet DB ! N statement(s) … while idle` | Background jobs are hitting the DB. routeperf excludes those query shapes, but stopping workers or cron jobs, or tagging SQL with sqlcommenter, gives the cleanest numbers |
| An endpoint shows many non-2xx responses | Inputs were invalid: add a fixture for that `operationId` (run with `-v` to see the first error response) |
| `note: index advice is unproven` | Install HypoPG on the database server (`apt install postgresql-17-hypopg`, or build it from source) |

---

## Limitations

- App code that does CPU work without touching the database is visible only through the output-scale test, where it shows up as app time growing with `k`.
- Caches in the app (Redis, ORM identity maps) can hide queries on repeated requests.
- Exponential growth isn't modelled. Big O is an empirical estimate with a confidence level, not a proof.
- Without sqlcommenter tags, SQL is attributed by time window, so measurements send one request at a time and exclude background SQL by query shape. An endpoint that runs exactly the same query shape as a background job can lose that statement from its count. Load mode can attribute SQL per endpoint only with sqlcommenter tags.
- Proxy capture: the app must connect without TLS to the proxy, and MySQL clients that require TLS (or `caching_sha2_password` full authentication without RSA key retrieval) can't use it. The proxy can use TLS to a Postgres server.
- Per-table scaling covers tables a query references directly; tables read through a view or a foreign-key cascade are scaled together with the rest.
- GraphQL subscriptions and gRPC streaming methods are skipped.

---

## Development

```bash
make build          # bin/routeperf
make test           # go vet + unit tests
# end to end (see CONTRIBUTING.md for Docker one-liners)
RP_IT_PG=postgres://localhost:5432/routeperf_it RP_IT_MYSQL=mysql://root@127.0.0.1:3306/routeperf_it \
  go test -tags integration -count=1 -v -timeout 40m ./integration/
make testbed        # sample API with planted problems, for end-to-end checks
make testbed-seed DB=postgres://localhost:5432/routeperf_testbed
bin/testbed --db-url postgres://localhost:5432/routeperf_testbed --addr :8088 --grpc-addr :50051 --sqlcommenter &
bin/routeperf run --spec http://localhost:8088/swagger.json --api-url http://localhost:8088 \
  --db-url postgres://localhost:5432/routeperf_testbed --token testtoken
```

The integration suite runs in CI on every push, against Postgres 17 and MySQL 8.4 service containers. It has three scenarios: the full run over the testbed (`TestEndToEnd`), proxy capture with sqlcommenter attribution, load mode and masking (`TestProxyAndLoad`), and the GraphQL and gRPC APIs (`TestGraphQLAndGRPC`).

The testbed supports MySQL as well (`--db-url mysql://root@127.0.0.1:3306/routeperf_testbed`). It accepts `Bearer testtoken`, `X-Api-Key: testkey`, cookie `session=testsession`, or `POST /auth/login` with `{"email":"perf@test.dev","password":"secret"}`. Its planted problems are:
- background job noise (`--background-noise 120ms`)
- UUID primary keys (`/notes`, with OpenAPI links from `POST /notes`)
- an unindexed filter, also on a partitioned table with a composite key (`/events/search`)
- an N+1 query, in REST (`/users/{id}/orders`) and in a GraphQL nested resolver (`users { notes }`)
- an unindexed FK cascade
- a view over an unindexed join (`/users/{id}/totals`)
- a join that scans two tables (`/orders/by-country`)
- a same-named table in another schema or database (`/archive/orders/{id}`)
- a full-table aggregate
- an INSERT per item
- an O(k²) loop in the app
- cursor pagination, form and multipart bodies
- a 10-connection pool (visible in load mode)
- gRPC methods (`--grpc-addr`) and a GraphQL endpoint (`/graphql`)

Layout:

```
cmd/routeperf/        CLI (cobra): commands, prompts
internal/spec         Swagger 2.0 / OpenAPI 3 → operation catalog (bodies, links)
internal/gql          GraphQL schema (introspection / SDL) → operations and documents
internal/grpcspec     gRPC services (reflection / .proto) → operations and request schemas
internal/auth         bearer, token commands, headers, cookies, login, OAuth2; scheme mapping; redaction
internal/inputs       request building: fixtures, links, cursors, examples, anchor IDs, form/multipart
internal/sqlutil      SQL parsing (libpg_query via WebAssembly, Vitess), fingerprints, rewriting
internal/db           Postgres + MySQL: catalog, log capture, EXPLAIN, subsets, snapshot, HypoPG, eviction
internal/proxy        Postgres and MySQL wire-protocol capture proxy (+ control API)
internal/plan         plan model + Postgres JSON / MySQL TREE parsers
internal/analyze      curve fits and confidence intervals, plan algebra, verdicts, advisor
internal/runner       doctor (check), run lifecycle, replay, per-table scaling, cold cache, load
internal/report       terminal, Markdown, HTML, JSON; literal masking
internal/diff         run-to-run comparison for CI
testbed/              sample API (REST, GraphQL, gRPC) with planted problems
integration/          end-to-end tests against real Postgres + MySQL (go test -tags integration)
```

The design and the Big O method are described in [PLAN.md](PLAN.md). See [CONTRIBUTING.md](CONTRIBUTING.md) before sending a change.

## Publishing releases (maintainers)

1. **One time:** point the module path and docs at your repository, then push.
   ```bash
   scripts/set-repo.sh github.com/<owner>/routeperf
   git init && git add -A && git commit -m "routeperf" && git remote add origin git@github.com:<owner>/routeperf.git && git push -u origin main
   ```
2. **Release:** tag a version. The `release` workflow runs GoReleaser and publishes macOS, Linux and Windows archives (amd64 and arm64) plus `checksums.txt`. The installer and `go install` pick it up.
   ```bash
   git tag v0.3.0 && git push --tags
   ```
3. **Homebrew tap:** create the repository `<owner>/homebrew-tap`, then add a repository secret `HOMEBREW_TAP_GITHUB_TOKEN` holding a token that can push to it. The next release publishes a cask there (`brew install <owner>/tap/routeperf`). Without the secret the step is skipped.
4. **Repository look:** README images live in `docs/assets/` (regenerate the SVGs with `python3 scripts/gen-images.py`). For link previews, upload `docs/assets/social-preview.png` under *Settings → General → Social preview*.
5. **No GitHub:** run `make release` and upload `dist/*` to any HTTP server. Users install with `ROUTEPERF_BASE_URL=https://your-server/path sh install.sh`.

## License

[MIT](LICENSE)
