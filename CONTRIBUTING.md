# Contributing to routeperf

Thanks for helping. This guide covers the local setup, the tests that guard
correctness, and how changes are reviewed.

## Setup

You need Go 1.26+ and, for the end-to-end tests, a Postgres (13+) and a MySQL
(8.0.18+) you can throw away. Docker is the quickest way to get both:

```bash
docker run -d --name rp-pg -e POSTGRES_PASSWORD=postgres -p 55432:5432 postgres:17
docker run -d --name rp-my -e MYSQL_ALLOW_EMPTY_PASSWORD=yes -e MYSQL_ROOT_HOST=% -p 33306:3306 mysql:8.4
```

Homebrew or distro packages work too; routeperf finds their server logs on its own.

## Build and test

```bash
make build     # bin/routeperf
make test      # go vet + unit tests (no database needed)

# end to end: seeds the testbed, runs routeperf, asserts every planted problem
RP_IT_PG=postgres://postgres:postgres@localhost:55432/routeperf_it \
RP_IT_MYSQL=mysql://root@127.0.0.1:33306/routeperf_it \
  go test -tags integration -count=1 -v -timeout 40m ./integration/
```

The integration suite has three scenarios:

| Test | What it covers |
|---|---|
| `TestEndToEnd` | log capture, data-scale and per-table Big O, N+1, index advice, partitioned tables, composite keys, views, other schemas, UUID keys, write snapshot/restore, background-SQL exclusion, cursor pagination, form and multipart bodies |
| `TestProxyAndLoad` | wire-proxy capture, sqlcommenter `traceparent` attribution, load mode, literal masking |
| `TestGraphQLAndGRPC` | GraphQL (introspection) and gRPC (server reflection) APIs, token from a command |

CI runs all three against Postgres 17 and MySQL 8.4 service containers on every push.

## The testbed

`testbed/` is a small API with deliberately planted performance problems. When you
add a detection, plant the problem there (a handler + its OpenAPI entry, or a GraphQL
field / gRPC method) and assert it in `integration/integration_test.go`. A detection
without a planted case isn't protected against regressions.

```bash
make testbed-seed DB=postgres://postgres:postgres@localhost:55432/routeperf_testbed
bin/testbed --db-url postgres://postgres:postgres@localhost:55432/routeperf_testbed --addr :8088 --grpc-addr :50051
bin/routeperf run --spec http://localhost:8088/swagger.json --api-url http://localhost:8088 \
  --db-url postgres://postgres:postgres@localhost:55432/routeperf_testbed --token testtoken
```

## Where things live

See the layout at the end of the [README](README.md#development). In short: the
dialect layer is `internal/db`, the measurement lifecycle is `internal/runner`, the
math (curve fits, confidence intervals, plan algebra, advice) is `internal/analyze`.

## Pull requests

- Keep changes focused; one feature or fix per PR.
- Match the surrounding code: small functions, comments only where the reason isn't obvious.
- Run `gofmt` and `make test`. Run the integration suite for anything that touches
  capture, replay, subsets or analysis (or rely on CI's integration job).
- Update the README when flags, config keys or report columns change.

## Reporting bugs

Use the bug report template. Strip tokens, passwords and real user data from what you
paste; `routeperf run --mask-literals` produces reports without SQL literal values.

## License

By contributing you agree that your contributions are licensed under the [MIT License](LICENSE).
