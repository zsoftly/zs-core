# Foundation

## TL;DR

The foundation is the shared plumbing every module uses. It fixes the error envelope, the public ID format, the brand and tenant scope, idempotent commands, the event envelope and the transactional outbox. It also runs the migrations and serves the health probes. All of it is importable from `pkg/` and takes a `pgx.Tx` where it writes, so a product can call it inside its own transaction. The HTTP server in `internal/httpapi` only translates. Every `/v1` request needs a Keycloak token or, on tenant routes, a tenant API key. See [identity-and-tenants.md](identity-and-tenants.md), [brands.md](brands.md) and [tenants.md](tenants.md).

## Packages

| Package | Purpose |
| --- | --- |
| `pkg/errors` | Stable `ZS-xxxx` codes, their snake_case names, HTTP statuses and the error envelope. |
| `pkg/idgen` | UUIDv7 and type-prefixed public IDs. |
| `pkg/scope` | The brand and tenant scope that every tenant-owned query carries. |
| `pkg/idempotency` | Reserve, complete and release of `idempotency_key`, with a Postgres store. |
| `pkg/events` | The versioned event envelope. |
| `pkg/outbox` | Transactional outbox writer, reader and relay with retries. |
| `pkg/audit` | Append-only audit trail; see [audit.md](audit.md). |
| `pkg/tenant` | Tenants, memberships, teams, invitations, activation and the sweeper. See [tenants.md](tenants.md). |
| `pkg/apikey` | Tenant API keys. See [api-keys.md](api-keys.md). |
| `pkg/billing` | The billing surface on Kill Bill: catalogue, account summary, subscriptions, invoices, payments, payment methods, credit codes, credits and their expiry, entitlements, push notifications and the billing credential proxy. See [billing.md](billing.md). |
| `internal/config` | Environment configuration with strict validation. |
| `internal/database` | Pool and the embedded forward-only migration runner. |
| `internal/httpapi` | Router, middleware, health probes and the `/v1` routes. |
| `internal/postgrestest` | Isolated Postgres schemas for integration tests. |

## Errors

Every error response has this shape and nothing else:

```json
{"error": {"code": "idempotency_key_conflict", "message": "...", "retryable": false}}
```

Clients branch on `code`. `message` is safe to show. It never carries database, provider or stack text. The service logs the cause with the request ID. A dependency failure is a 503 with a `*_unavailable` code and `retryable: true`. Any error that is not a typed `pkg/errors` value becomes `500 internal_error`.

| ZS code | Name | Status | Retryable |
| --- | --- | --- | --- |
| ZS-1000 | `internal_error` | 500 | no |
| ZS-1001 | `validation_failed` | 400 | no |
| ZS-1002 | `bad_request` | 400 | no |
| ZS-1003 | `not_found` | 404 | no |
| ZS-1004 | `conflict` | 409 | no |
| ZS-1005 | `forbidden` | 403 | no |
| ZS-1006 | `unauthenticated` | 401 | no |
| ZS-1007 | `rate_limited` | 429 | yes |
| ZS-1008 | `method_not_allowed` | 405 | no |
| ZS-1009 | `payload_too_large` | 413 | no |
| ZS-1010 | `unsupported_media_type` | 415 | no |
| ZS-1011 | `reason_required` | 400 | no |
| ZS-1100 | `database_unavailable` | 503 | yes |
| ZS-1101 | `not_ready` | 503 | yes |
| ZS-1102 | `identity_provider_unavailable` | 503 | yes |
| ZS-1103 | `billing_unavailable` | 503 | yes |
| ZS-1200 | `idempotency_key_invalid` | 400 | no |
| ZS-1201 | `idempotency_key_conflict` | 409 | no |
| ZS-1202 | `idempotency_key_in_progress` | 409 | yes |
| ZS-1203 | `idempotency_result_too_large` | 500 | no |
| ZS-1300 | `invalid_cursor` | 400 | no |
| ZS-1301 | `invalid_limit` | 400 | no |
| ZS-1400 | `scope_required` | 400 | no |
| ZS-1401 | `scope_invalid` | 400 | no |
| ZS-1402 | `token_invalid` | 401 | no |
| ZS-1403 | `token_audience_unknown` | 401 | no |
| ZS-1404 | `authentication_unavailable` | 503 | yes |
| ZS-1405 | `email_not_verified` | 403 | no |
| ZS-1406 | `recent_mfa_required` | 403 | no |
| ZS-1407 | `mfa_required` | 403 | no |
| ZS-1500 | `brand_manifest_invalid` | 400 | no |
| ZS-1501 | `brand_apply_rejected` | 422 | no |
| ZS-1502 | `brand_apply_in_progress` | 409 | yes |
| ZS-1503 | `secret_unresolved` | 422 | no |
| ZS-1504 | `secret_ref_invalid` | 422 | no |
| ZS-1600 | `tenant_not_found` | 404 | no |
| ZS-1601 | `permission_denied` | 403 | no |
| ZS-1602 | `last_owner` | 409 | no |
| ZS-1603 | `invitation_not_found` | 404 | no |
| ZS-1604 | `invitation_email_mismatch` | 403 | no |
| ZS-1605 | `permission_unknown` | 400 | no |
| ZS-1606 | `team_locked` | 409 | no |
| ZS-1607 | `already_member` | 409 | no |
| ZS-1608 | `tenant_kind_unsupported` | 409 | no |
| ZS-1609 | `recent_auth_required` | 403 | no |
| ZS-1610 | `tenant_closed` | 409 | no |
| ZS-1700 | `api_key_invalid` | 401 | no |
| ZS-1701 | `api_key_owner_removed` | 401 | no |
| ZS-1800 | `billing_rejected` | 422 | no |
| ZS-1801 | `invoice_pdf_unavailable` | 404 | no |
| ZS-1802 | `plugin_unavailable` | 422 | no |
| ZS-1803 | `credit_code_invalid` | 404 | no |
| ZS-1804 | `credit_code_exhausted` | 409 | no |
| ZS-1805 | `wallet_on_hold` | 409 | no |
| ZS-1806 | `billing_token_invalid` | 401 | no |

Codes and names are stable. New codes are added. Existing codes are never reused. The ZS-1600 range covers tenants, memberships, teams and invitations, the ZS-1700 range covers API keys, and the ZS-1800 range covers billing. See [tenants.md](tenants.md), [api-keys.md](api-keys.md) and [billing.md](billing.md).

`billing_rejected` carries Kill Bill's numeric error code in its message and never Kill Bill's text. A Kill Bill 5xx or timeout is `billing_unavailable`.

## Public IDs

Internally every ID is a UUIDv7. On the API it carries a type prefix and the UUID in lowercase Crockford base32, 26 characters: `tnt_01m5bzbkj6en5tr1ekx7m6xdae`. IDs sort by creation time. The prefixes are `brd_` brand, `tnt_` tenant, `mem_` membership, `inv_` invitation, `key_` API key, `grt_` grant, `idn_` identity, `rsv_` reservation, `usg_` usage, `ord_` order, `evt_` event, `aud_` audit event, `ccd_` credit code and `crd_` credit. Objects that live in Kill Bill only, such as subscriptions, invoices and payments, keep their Kill Bill IDs. See [billing.md](billing.md). `idgen.ParseAs` rejects an ID with the wrong prefix before any query runs.

## Scope

A `scope.Scope` holds a brand and a tenant. `Bind` turns it into query arguments with the brand as `$1` and the tenant as `$2`, and returns `scope_required` if either is missing. Queries in Core filter on both columns. Brand-level routes carry a scope with the brand set and the tenant unset. `scope.BrandFromContext` reads it. `scope.FromContext` and `Bind` refuse it. A brand route therefore cannot reach tenant rows by accident.

Tenant routes carry a full scope and the caller's effective permissions, which scope resolution sets with `scope.WithPermissions`. Handlers call `scope.Require(ctx, "resource:action")`. It fails closed: without a full scope, or without the permission, it returns `permission_denied`.

## Idempotency

A command that changes state takes an `idempotency_key` body field of 8 to 255 printable ASCII characters. The key is scoped to the brand, the tenant and the actor. `idempotency.Do` reserves the key, runs the command once and stores the result for 24 hours.

- Same key, same payload: the stored result is returned and the command does not run again.
- Same key, different payload: `409 idempotency_key_conflict`.
- Same key while the first request still runs: `409 idempotency_key_in_progress`, retryable.
- The command returns an error: the reservation is released so the client retries.
- The command succeeds but Core cannot store the result: the key becomes `completed_unstored`. It replays `409 idempotency_key_in_progress` until it expires, so the command never runs twice.

The payload fingerprint is a SHA-256 of the operation name and the canonical JSON body, so field order and whitespace do not matter. Each reservation carries a random 16-byte owner token. Complete and release only work with that token. A reservation is a two-minute lease. The command runs under a context that ends with the lease. After the lease ends, an identical payload can take the key over. A different payload cannot. The request that lost the lease cannot write to or delete the new owner's reservation. Core instances must keep their clocks well within two minutes of each other.

## Events and outbox

An event envelope carries these fields:

- `envelope_version`
- an `evt_` ID
- a dotted `type`
- a `schema_version`
- `occurred_at`
- the `producer` service and instance
- the `actor`
- the `brand_id`
- the `tenant_id`, for tenant-owned events
- the `aggregate`
- a `correlation_id`
- an optional `causation_id`
- the W3C `traceparent` of the request that caused it

`outbox.Write` inserts the envelope in the caller's transaction. A `Relay` reads due events oldest first, hands each to a handler and marks it published on success. A failure is retried after 5 seconds, doubling up to 5 minutes, and the event is quarantined after 10 failures by default. A relay claims the events it reads for 5 minutes. If the relay crashes, the events become due again when the claim ends. A relay whose claim expired cannot mark an event published. Delivery is at least once. Consumers deduplicate on the event ID.

A reader made with `WithEventTypes` claims only the listed event types. The first consumer is the Keycloak Organization mirror of tenants, which claims only `tenant.mirror` events. A relay for another consumer must not claim them. See [tenants.md](tenants.md#the-organization-mirror).

## HTTP behaviour

Every response carries `Cache-Control: no-store`, `X-Content-Type-Options: nosniff` and `X-Request-ID`. The server also follows these rules:

- A valid inbound `X-Request-ID` is echoed. Otherwise the server assigns a UUIDv7.
- An inbound `traceparent` is continued. Otherwise the server starts a new trace so logs and events still carry one.
- No tracing exporter is configured in Phase 1.
- Request bodies are limited to 1 MiB.
- A panic becomes `500 internal_error` and its stack is logged.
- Unknown routes and wrong methods return the error envelope.

| Route | Purpose |
| --- | --- |
| `GET /healthz` | 200 while the process serves HTTP. |
| `GET /readyz` | 200 when the database answers and every migration is applied, otherwise 503. |
| `GET /v1/me` | The caller's identity; see [identity-and-tenants.md](identity-and-tenants.md). |
| `GET /v1/brands/{id}`, `POST /v1/brands/{id}/apply` | A brand and brand apply, staff `operator` only; see [brands.md](brands.md). |
| `GET /v1/audit/events` | Audit events of the caller's brand or one of its tenants, staff `operator` only; see [audit.md](audit.md). |
| `POST /v1/tenants/personal`, `POST /v1/tenants`, `/v1/tenants/{id}/...`, `POST /v1/invitations/accept`, `POST /v1/staff/tenants/{id}/activation` | Tenants, members, teams, invitations, activation, API keys and the tenant audit trail. See [tenants.md](tenants.md), [api-keys.md](api-keys.md) and `api/openapi.yaml`. |
| `GET /v1/brands/{id}/catalogue`, the billing routes under `/v1/tenants/{id}/...`, `POST /v1/staff/tenants/{id}/credits`, `POST /v1/billing/webhooks/killbill/{brand}/{token}` | The billing surface on Kill Bill. See [billing.md](billing.md). |

## Known limits

These are accepted today and scheduled for later hardening.

- The audit trail is append-only because triggers reject `UPDATE`, `DELETE` and `TRUNCATE` on `audit_events`. The runtime database role owns the table, so it could drop the triggers. A separate migration-owner role, with the runtime role limited to `INSERT` and `SELECT`, is Phase 6 hardening.
- The migration runner ignores versions in `schema_migrations` that the binary does not embed. An older binary started against a newer schema, after a downgrade, therefore reports ready. Phase 6 adds a refusal to start when the binary is older than the schema.
- The tenant isolation proof runs only in the integration suite (`make test-integration`), which the build host runs. `make check` alone does not run it.
