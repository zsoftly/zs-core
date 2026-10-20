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

Every `/v1` route needs a Keycloak access token from the realm of an applied brand, sent as `Authorization: Bearer <token>`. Tenant routes also take a tenant API key, `Bearer zs_<brand slug>_...`:

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
bin/zs-core tenant suspend --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
bin/zs-core tenant reactivate --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
bin/zs-core tenant close --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
bin/zs-core tenant sweep        # complete interrupted tenant creations and correct Organizations once
bin/zs-core credit-code add --brand zs-reference --code WELCOME10 --amount-minor 1000 --currency CAD \
  --reason '<why>' --operator <you>
bin/zs-core credit-code list --brand zs-reference
bin/zs-core credit-code disable --brand zs-reference --id ccd_... --reason '<why>' --operator <you>
bin/zs-core credit sweep        # reverse the unused part of expired credits once
bin/zs-core wallet sweep        # expire reservations past their lease and reconcile every wallet once
bin/zs-core usage forward       # forward recorded usage to Kill Bill once
```

`credit-code` needs the database only. `credit sweep`, `wallet sweep` and `usage forward` also need the Kill Bill admin settings and the brands' secret references. See [modules/billing.md](modules/billing.md), [modules/wallet.md](modules/wallet.md) and [modules/metering.md](modules/metering.md).

The `tenant` commands need the Keycloak and Kill Bill admin settings and the brand's secret references. See [modules/tenants.md](modules/tenants.md).

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

Integration tests are in files named `*_integration_test.go` with the `integration` build tag. Each test gets its own Postgres schema, named `zs_test_<random>`, with every migration applied, and the schema is dropped when the test ends. Tests in different packages therefore run in parallel against one server without seeing each other's rows. Run the suite in parallel, without `-p 1`.

Two rules keep parallel schemas apart on one server:

- Every advisory lock key Core takes names the schema, `current_schema()`, as the migration lock does. Tests reuse brand slugs and fixed keys, so without the schema their locks would collide across schemas. One deployment uses one schema, so its instances still share each lock.
- Migrating a schema and dropping it lock every table, index and partition in it. Many test binaries doing that at once exhaust Postgres's shared lock table, `out of shared memory`. The harness runs those two steps one at a time across every test process, under a server-wide advisory lock. The tests themselves still run in parallel.

The tenant, API key, billing, wallet, metering and quota integration tests in `pkg/tenant`, `pkg/apikey`, `pkg/billing`, `pkg/wallet`, `pkg/metering`, `pkg/quota`, `pkg/brand` and `internal/httpapi` need Postgres only. `internal/enginetest` serves the Keycloak Organizations endpoints and the Kill Bill endpoints Core uses from memory, over HTTP, so the real clients run against it. The Kill Bill endpoints are:

- accounts and catalogue
- subscriptions and invoices
- credits and external charges
- usage records, with Kill Bill's refusal of a tracking ID the subscription already holds, and rolled-up usage
- dry-run invoices, with the usage of active subscriptions that no invoice bills yet, priced from the catalogue, and no recurring charges
- an invoice run that bills an account's usage of a period as `USAGE` items with the period's dates, and lists the tracking IDs on the invoice
- subscription start dates, which a test can move
- failure modes on demand: a lost answer, a write that takes effect but answers 503, a write that answers 503 or 400 and takes no effect, and a slow write that waits before the fake serves it while other calls go on. A hook runs after every write.
- payments and refunds
- payment methods, with the external payment plugin's rules
- overdue and blocking states
- push notification callbacks
- KPM node commands

Its catalogue has the plan `core-metered-monthly`, which prices `api-call` per call with no trial, like the reference catalogue. It records the audit headers of every Kill Bill write. It makes either engine answer 503 on demand. It also inserts applied brands and identities and signs access tokens for them with a test key. The endpoints it serves are the ones the stack tests check against the real servers.

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

`internal/stacktest` holds the tests that need a running Keycloak and Kill Bill. They apply `brands/zs-reference.yaml` twice and expect an empty second diff. They change the login theme and the MFA policy and expect exactly those two fields in the diff and in audit. They use real tokens from the password grant against `GET /v1/me` and the staff routes. Over HTTP they check that a sensitive change without recent MFA is refused, that another brand's secrets and files are out of reach, and that changing only a payment plugin's `config_ref` is gated. They apply a second brand, `zs-second`, and check that it cannot act in `zs-reference`. They set `zs-second` to `mfa: required` and check in Keycloak that every identity provider runs the `zs-post-broker-mfa` flow, then set it back.

The tenant stack tests cover the Phase 3 gates with a real Keycloak and Kill Bill:

- `TestTenantsOnTheStack` runs these checks:
  - A tenant created while Kill Bill is unreachable gets 503, no tenant row and an Organization that carries the tenant ID.
  - The sweeper then completes the same tenant, and the Kill Bill account's external key is the tenant ID.
  - Creation is idempotent.
  - Each brand's Kill Bill accounts live in its own Kill Bill tenant.
  - A `zs-second` token reads, creates and accepts nothing in `zs-reference`.
  - An invitation fails for another email and succeeds for the same email in another case.
  - A member's live token and API key work, and the key shrinks when the member is demoted.
  - Both fail on the next request once the member is removed, and after the sweeper runs the Organization no longer lists the member.
  - Invited back, the member gets a new membership and the old key stays `401 api_key_invalid`.
  - Unreachable signing keys give 503.
  - An activation grant of `zs-second` acts in `zs-second` only.
  - Suspension adds the Kill Bill blocking state and disables the Organization, reads continue and writes stop.
  - Reactivation lifts both, and closure closes the Kill Bill account. A closed tenant cannot be reactivated: `tenant_closed`.
- `TestRequiredForStaffBindsBrokeredStaffLoginsOnTheStack` sets `zs-second` to `mfa: required_for_staff` with an extra identity provider. It creates a second realm that plays the provider and signs in through it as a browser would. The staff client is asked for a one-time password, the customer client gets an authorization code, and with `optional` the staff client gets a code too.
- `TestStaffClientRefusesSSOWithoutSecondFactorOnTheStack` sets `zs-second` to `mfa: required_for_staff`. It signs in to the customer client with a password only, as a browser would, then asks the staff client for a code in the same browser. Keycloak asks for the password and a one-time password instead of reusing the session. Core answers `403 mfa_required` to a staff token without a second factor. With `mfa: required`, a password login reaches the one-time password after the password form.

`TestBillingOnTheStack` covers the Phase 4a gates with a real Kill Bill. It applies `zs-reference` with the built-in `__EXTERNAL_PAYMENT__` plugin and `zs-second` with the Stripe plugin 8.0.3 and a dummy `sk_test_` key:

- A live Stripe key, `mode: live` and `paypal-express` are refused before anything changes. So are a Stripe configuration without a key, texts with a `\u` escape or a bare carriage return, an Adyen `LIVE` environment in a Stripe configuration, and the `config_key` of another plugin.
- Stripe 8.0.3 is installed through KPM after Core checks the jar's SHA-256, reaches `RUNNING` on every node and is recorded in `killbill_plugin_installs`. A second apply is empty.
- Two tenants are created through Core. The catalogue summary shows the usage-free plan `core-basic-monthly` with its trial.
- A subscription with a trial on that plan produces an invoice. Replaying its key returns it. The entitlement follows the plan. A plan change at the end of the term is accepted.
- A code is redeemed once per tenant and up to its cap. The credit shows on the Kill Bill account and in the billing summary.
- A staff credit with an expiry is partly used, and the sweeper reverses exactly the unused part in Kill Bill.
- A payment method is added on the external payment plugin, a hosted page is asked for, a payment taken outside Core is refunded by staff, and the refund replays. A refund past what is left is refused with `refund_exceeds_payment`.
- The subscription, plan change, payment method and refund are each audited with the tenant's Kill Bill account ID.
- Suspension puts billing on hold and reactivation lifts it.
- The credentials proxy reads the tenant's own account and refuses another tenant's account, by ID and by external key.
- With `ZS_TEST_CALLBACK_ADDR` set, Kill Bill's push notifications reach Core and become `billing.subscription.changed`, `billing.invoice.created` and `billing.payment.succeeded` events.

`TestExtensionsOnTheStack` covers the Phase 4b gates with a real Kill Bill:

- `zs-reference` applies its meter and its two quotas, a second apply is empty, and a meter on a unit outside the catalogue is refused.
- A tenant created through Core gets a Core credit and a subscription on `core-metered-monthly`.
- Usage recorded over HTTP is forwarded as one usage record and shows in `GET /1.0/kb/usages/{subscriptionId}`. A forced retry of the rollup meets Kill Bill's refusal of its tracking ID and records nothing more.
- A reservation settled into usage adds a usage record, and one settled into a charge lowers the Kill Bill credit by the charge.
- A settlement made while Kill Bill cannot be reached waits, the hold keeps counting in the wallet view, and the relay completes it. The settlement's holder is the owner's membership that made the hold over HTTP.
- The wallet view equals the Kill Bill credit less the unbilled usage of Kill Bill's dry-run invoice less the holds, and a reservation one minor unit past that is refused. A hold expires with its lease. A credit added in Kill Bill behind Core's back is exactly one reconciliation mismatch.
- The entitlement of `api-call` shows the month quota and the usage. `GET /v1/tenants/{id}/usage/billed` shows Kill Bill's total.
- An invoice run past the end of the first period bills the usage as a `USAGE` item that lists both tracking IDs. Kill Bill's test clock cannot move on the stack, so the run names a target date.
- The client reads those tracking IDs, and the dates of the usage item, from Kill Bill's invoices, and the invoiced usage reconciles with no new finding.

Kill Bill reaches the test only from inside its own network. If your build host's firewall drops traffic from containers to the host, run this test in Kill Bill's network namespace, after the full suite:

```bash
set -a; . deploy/compose/.env; set +a
docker run --rm --network container:zs-core-killbill-1 -v "$PWD":/src -w /src \
  -e ZS_TEST_POSTGRES_FORWARD=127.0.0.1:55432=postgres:5432 \
  -e ZS_TEST_POSTGRES_URL="postgres://postgres:$POSTGRES_PASSWORD@127.0.0.1:55432/zs_core_test?sslmode=disable" \
  -e ZS_TEST_KEYCLOAK_URL=http://keycloak:8080 -e ZS_TEST_KEYCLOAK_ADMIN_USER="$KEYCLOAK_ADMIN_USER" \
  -e ZS_TEST_KEYCLOAK_ADMIN_PASSWORD="$KEYCLOAK_ADMIN_PASSWORD" -e ZS_TEST_KILLBILL_URL=http://127.0.0.1:8080 \
  -e ZS_TEST_KILLBILL_ADMIN_PASSWORD="$KILLBILL_ADMIN_PASSWORD" -e ZS_TEST_CALLBACK_ADDR=127.0.0.1:18080 \
  -e BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET="$ZS_STAFF_CLIENT_SECRET" \
  -e BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET="$KILLBILL_TENANT_API_SECRET" \
  golang:1.26 go test -tags integration -count=1 -run TestBillingOnTheStack ./internal/stacktest/
```

`ZS_TEST_POSTGRES_FORWARD` makes the test forward a loopback port to the stack's Postgres, because the Postgres harness accepts loopback servers only. The test reads realm issuers from Keycloak discovery, so it accepts Keycloak at `http://keycloak:8080`. Only this test does: run the others on the host network.

Keycloak 26.8.1 puts no `auth_time` and no `amr` in password grant tokens, so those tokens never pass a recent authentication or recent MFA check. The stack tests run the steps that need one, member changes, API key creation and invitation acceptance, through the library as the identities the real tokens recorded.

The tests skip unless `ZS_TEST_KEYCLOAK_URL` is set. They change the stack. Run them only on a build host stack after `make stack-up` and `make stack-bootstrap`. Run `make stack-reset` afterwards.

| Variable | Value on the reference stack |
| --- | --- |
| `ZS_TEST_KEYCLOAK_URL` | `http://127.0.0.1:8180` |
| `ZS_TEST_KEYCLOAK_ADMIN_USER`, `ZS_TEST_KEYCLOAK_ADMIN_PASSWORD` | `KEYCLOAK_ADMIN_USER`, `KEYCLOAK_ADMIN_PASSWORD` from `deploy/compose/.env` |
| `ZS_TEST_KILLBILL_URL` | `http://127.0.0.1:8081` |
| `ZS_TEST_KILLBILL_ADMIN_PASSWORD` | `KILLBILL_ADMIN_PASSWORD` |
| `BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET` | `ZS_STAFF_CLIENT_SECRET` |
| `BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET` | `KILLBILL_TENANT_API_SECRET` |
| `ZS_TEST_CALLBACK_ADDR` | Optional. An address Kill Bill reaches, such as `127.0.0.1:18080` inside Kill Bill's network namespace. The billing test serves Core there. |
| `ZS_TEST_POSTGRES_FORWARD` | Optional. `<loopback addr>=<host:port>`, a forward the stack tests open to the stack's Postgres. |

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
