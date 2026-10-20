# Development

## TL;DR

You need Go 1.26.1 or later and a Postgres server. Postgres 18 is what the stack and the test harness run. Earlier majors are not tested. Docker runs the integration tests unless you point them at your own server. `make check` gates every change. It checks formatting, runs `go vet`, runs the unit tests and fails when the generated API types are stale. There is no hosted CI workflow, so run `make check` before you push, and run `make test-integration` when you touch SQL, migrations or anything under `pkg/` that talks to Postgres. Configuration is environment only; [configuration.md](configuration.md) lists every variable. The service applies its migrations before it reports ready.

## Make targets

| Target | What it does |
| --- | --- |
| `make fmt` | Formats every Go file with `gofmt`. |
| `make fmt-check` | Fails if any Go file needs `gofmt`. |
| `make vet` | Runs `go vet` with and without the `integration` build tag. |
| `make lint` | Runs `make vet`, then `staticcheck` if it is installed. Without `staticcheck` it prints a notice and runs vet only. |
| `make test` | Runs the unit tests. No database or Docker needed. |
| `make test-race` | Runs the unit tests with the race detector. |
| `make test-integration` | Runs the unit and integration tests against Postgres. Runs only where `ZS_BUILD_HOST=1` or `ZS_TEST_POSTGRES_URL` is set. |
| `make build` | Builds `bin/zs-core`. |
| `make generate` | Regenerates `internal/httpapi/gen/types.gen.go` from `api/openapi.yaml`. |
| `make generate-check` | Fails if the generated types do not match the OpenAPI document. |
| `make check` | `fmt-check`, `vet`, `test` and `generate-check`. This is the merge gate. |
| `make stack-up`, `make stack-down`, `make stack-reset`, `make stack-logs`, `make stack-bootstrap`, `make doctor` | The reference stack. They run `docker compose` in the current shell, and only where `ZS_BUILD_HOST=1` is set. Use them on a build host. See [../deploy/compose/README.md](../deploy/compose/README.md). |

## Run the service

```bash
cp .env.example .env            # then edit ZS_DATABASE_URL and ZS_KEYCLOAK_URL
set -a; . ./.env; set +a
make build
bin/zs-core serve
```

`serve` applies pending migrations, then listens on `ZS_LISTEN_ADDR`. Check it with:

```bash
curl -i http://127.0.0.1:8080/healthz
curl -i http://127.0.0.1:8080/readyz
```

Every `/v1` route needs a Keycloak access token from the realm of an applied brand, sent as `Authorization: Bearer <token>`:

```bash
curl -s http://127.0.0.1:8080/v1/me -H "Authorization: Bearer $TOKEN"
```

A brand is applied with `zs-core brand apply` (below) before its tokens are accepted. Staff routes also need a staff grant from `zs-core grant add`.

## Operator commands

```bash
bin/zs-core doctor              # one line per finding; exit 1 if any is FAIL or SKIP
bin/zs-core doctor --json       # the same report as JSON
bin/zs-core doctor --allow-skips   # exit 0 despite SKIP findings (a pin not checked)
bin/zs-core upgrade check       # newer upstream versions; changes nothing
bin/zs-core upgrade check --strict   # exit 1 if a release feed cannot be read
bin/zs-core brand diff brands/zs-reference.yaml      # what apply would change; changes nothing
bin/zs-core brand apply --operator <you> --reason '<why>' brands/zs-reference.yaml
bin/zs-core grant add --brand zs-reference --subject <keycloak user id> --scope operator \
  --reason '<why>' --operator <you>
bin/zs-core grant revoke --brand zs-reference --subject <keycloak user id> --scope operator \
  --reason '<why>' --operator <you>
bin/zs-core grant list --brand zs-reference
```

`brand diff` and `brand apply` print the diff as JSON. They need the database with migrations applied and the Keycloak and Kill Bill admin settings. `grant add` and `grant revoke` read the realm issuer from Keycloak discovery unless you pass `--issuer`. They refuse an issuer of another realm. `brand apply` reads the manifest's secrets from `BRAND_<SLUG>__` variables, such as `BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET`. See [modules/brands.md](modules/brands.md).

`doctor` checks the database (reachable, migrations, Postgres major version, clock skew), Keycloak, Kill Bill and KAUI against `components.yaml`. It needs `ZS_DATABASE_URL` and the component settings listed in [configuration.md](configuration.md). `upgrade check` needs no configuration. It reads GitHub releases for Keycloak and Kill Bill and Docker Hub tags for KAUI, prints a warning and exits 0 when a feed fails, and uses `GITHUB_TOKEN` if set to raise the GitHub rate limit. Both read `components.yaml` from the working directory unless `--components` says otherwise.

## Migrations

Migrations live in `migrations/` as `NNNNNN_name.up.sql`. They are embedded in the binary and forward only. There are no down files. To correct a migration, add a new one. Never edit a released file. The runner stores a SHA-256 checksum of each applied file and refuses to start if one changes.

The runner takes a Postgres advisory lock per schema, so you can start several instances at once and each migration still runs once. Each migration runs in its own transaction with its history row in `schema_migrations`.

```bash
bin/zs-core migrate     # apply pending migrations and exit
```

`serve` runs the same step unless `ZS_MIGRATE_ON_START=false`. `/readyz` returns 503 while any embedded migration is missing from the database.

Migrations use unqualified names, so they apply to the first schema on the connection's `search_path`. Library callers that embed Core run its migrations before their own.

## Tests

Unit tests sit next to the code and need nothing but Go.

Integration tests are in files named `*_integration_test.go` with the `integration` build tag. Each test gets its own Postgres schema, named `zs_test_<random>`, with every migration applied, and the schema is dropped when the test ends. Tests in different packages therefore run in parallel against one server without seeing each other's rows.

The harness in `internal/postgrestest` chooses the server like this:

1. If `ZS_TEST_POSTGRES_URL` is set, it uses that server. The URL must point at a loopback host, because the tests create and drop schemas there.
2. Otherwise it starts one `postgres:18-alpine` container per test binary through testcontainers. The testcontainers reaper removes the container when the tests finish.
3. If neither is available, each integration test is skipped with a message that says so.

Testcontainers checks `DOCKER_HOST` first, then other Docker sockets. A wrong `DOCKER_HOST` does not stop it. On Rancher Desktop, set `DOCKER_HOST=unix://$HOME/.rd/docker.sock`. In a container that shares the host network and Docker socket, set `TESTCONTAINERS_HOST_OVERRIDE=127.0.0.1`.

`make test-integration` and the `make stack-*` targets need a positive marker before they start containers. They run only where `ZS_BUILD_HOST=1` is set in the environment, which the build VM does in `/etc/environment`, or, for `make test-integration` only, where `ZS_TEST_POSTGRES_URL` is set. Without either they refuse and exit non-zero. Never set `ZS_BUILD_HOST` on a workstation. See [../deploy/compose/README.md](../deploy/compose/README.md#the-build-host-marker).

```bash
make test-integration
ZS_TEST_POSTGRES_URL='postgres://zs:zs-test@127.0.0.1:55432/zs_core_test?sslmode=disable' make test-integration
```

### Tests against the reference stack

`internal/stacktest` holds the tests that need a running Keycloak and Kill Bill. They apply `brands/zs-reference.yaml` twice and expect an empty second diff. They change the login theme and the MFA policy and expect exactly those two fields in the diff and in audit. They use real tokens from the password grant against `GET /v1/me` and the staff routes. Over HTTP they check that a sensitive change without recent MFA is refused, that another brand's secrets and files are out of reach, and that changing only a payment plugin's `config_ref` is gated. They apply a second brand, `zs-second`, and check that it cannot act in `zs-reference`. They set `zs-second` to `mfa: required` and check in Keycloak that every identity provider runs the `zs-post-broker-mfa` flow, then set it back. The tests skip unless `ZS_TEST_KEYCLOAK_URL` is set. They change the stack. Run them only on a build host stack after `make stack-up` and `make stack-bootstrap`. Run `make stack-reset` afterwards.

| Variable | Value on the reference stack |
| --- | --- |
| `ZS_TEST_KEYCLOAK_URL` | `http://127.0.0.1:8180` |
| `ZS_TEST_KEYCLOAK_ADMIN_USER`, `ZS_TEST_KEYCLOAK_ADMIN_PASSWORD` | `KEYCLOAK_ADMIN_USER`, `KEYCLOAK_ADMIN_PASSWORD` from `deploy/compose/.env` |
| `ZS_TEST_KILLBILL_URL` | `http://127.0.0.1:8081` |
| `ZS_TEST_KILLBILL_ADMIN_PASSWORD` | `KILLBILL_ADMIN_PASSWORD` |
| `BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET` | `ZS_STAFF_CLIENT_SECRET` |
| `BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET` | `KILLBILL_TENANT_API_SECRET` |

```bash
set -a; . deploy/compose/.env; set +a
ZS_TEST_POSTGRES_URL="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:5432/zs_core_test?sslmode=disable" \
ZS_TEST_KEYCLOAK_URL=http://127.0.0.1:8180 ZS_TEST_KEYCLOAK_ADMIN_USER="$KEYCLOAK_ADMIN_USER" \
ZS_TEST_KEYCLOAK_ADMIN_PASSWORD="$KEYCLOAK_ADMIN_PASSWORD" ZS_TEST_KILLBILL_URL=http://127.0.0.1:8081 \
ZS_TEST_KILLBILL_ADMIN_PASSWORD="$KILLBILL_ADMIN_PASSWORD" \
BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET="$ZS_STAFF_CLIENT_SECRET" \
BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET="$KILLBILL_TENANT_API_SECRET" \
  go test -tags integration -count=1 ./internal/stacktest/
```

The `zs_core_test` database must exist on the stack's Postgres; the tests create and drop their own schemas in it.

## OpenAPI and generated types

`api/openapi.yaml` (OpenAPI 3.1) is the source of truth for the HTTP contract. Go types are generated from it with `oapi-codegen`, which is pinned as a Go tool in `go.mod`, and committed in `internal/httpapi/gen/types.gen.go`. After changing the document, run `make generate` and commit both files. `make check` and the `gen` package test fail when the committed file is stale.

## Repository conventions

- Standard library `net/http` with method patterns, `log/slog`, `pgx/v5`. No web framework and no ORM.
- Every tenant-owned query goes through `scope.Scope.Bind`, which refuses a scope without both a brand and a tenant.
- Errors returned to clients are `pkg/errors` values. Their cause is logged and never written to a response.
- Library functions that write take a `pgx.Tx`, so a caller runs them inside its own transaction.
