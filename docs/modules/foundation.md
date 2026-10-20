# Foundation

## TL;DR

The foundation is the shared plumbing every module uses. It fixes the error envelope, the public ID format, the brand and tenant scope, idempotent commands, the event envelope and the transactional outbox, and it runs the migrations and serves the health probes. All of it is importable from `pkg/` and takes a `pgx.Tx` where it writes, so a product can call it inside its own transaction. The HTTP server in `internal/httpapi` only translates. Phase 1 has no authentication. See the trusted scope headers in [configuration.md](../configuration.md).

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
| ZS-1100 | `database_unavailable` | 503 | yes |
| ZS-1101 | `not_ready` | 503 | yes |
| ZS-1200 | `idempotency_key_invalid` | 400 | no |
| ZS-1201 | `idempotency_key_conflict` | 409 | no |
| ZS-1202 | `idempotency_key_in_progress` | 409 | yes |
| ZS-1203 | `idempotency_result_too_large` | 500 | no |
| ZS-1300 | `invalid_cursor` | 400 | no |
| ZS-1301 | `invalid_limit` | 400 | no |
| ZS-1400 | `scope_required` | 400 | no |
| ZS-1401 | `scope_invalid` | 400 | no |

Codes and names are stable. New codes are added; existing ones are never reused.

## Public IDs

Internally every ID is a UUIDv7. On the API it carries a type prefix and the UUID in lowercase Crockford base32, 26 characters: `tnt_01m5bzbkj6en5tr1ekx7m6xdae`. IDs sort by creation time. The prefixes are `brd_` brand, `tnt_` tenant, `mem_` membership, `inv_` invitation, `key_` API key, `grt_` grant, `rsv_` reservation, `usg_` usage, `ord_` order, `evt_` event and `aud_` audit event. `idgen.ParseAs` rejects an ID with the wrong prefix before any query runs.

## Scope

A `scope.Scope` holds a brand and a tenant. `Bind` turns it into query arguments with the brand as `$1` and the tenant as `$2`, and returns `scope_required` if either is missing. Queries in Core filter on both columns.

## Idempotency

A command that changes state takes an `idempotency_key` body field of 8 to 255 printable ASCII characters. The key is scoped to the brand, the tenant and the actor. `idempotency.Do` reserves the key, runs the command once and stores the result for 24 hours.

- Same key, same payload: the stored result is returned and the command does not run again.
- Same key, different payload: `409 idempotency_key_conflict`.
- Same key while the first request still runs: `409 idempotency_key_in_progress`, retryable.
- The command returns an error: the reservation is released so the client retries.

The payload fingerprint is a SHA-256 of the operation name and the canonical JSON body, so field order and whitespace do not matter. A reservation whose process died is taken over after two minutes, but only by an identical payload.

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

`outbox.Write` inserts the envelope in the caller's transaction. A `Relay` reads due events oldest first, hands each to a handler and marks it published on success. A failure is retried after 5 seconds, doubling up to 5 minutes, and the event is quarantined after 10 failures by default. Delivery is at least once; consumers deduplicate on the event ID.

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
| `GET /v1/audit/events` | Audit events of one tenant; see [audit.md](audit.md). |

## Known limits

These are accepted for Phase 1 and scheduled for later hardening.

- The audit trail is append-only because triggers reject `UPDATE`, `DELETE` and `TRUNCATE` on `audit_events`. The runtime database role owns the table, so it could drop the triggers. A separate migration-owner role, with the runtime role limited to `INSERT` and `SELECT`, is Phase 6 hardening.
- The migration runner ignores versions in `schema_migrations` that the binary does not embed. An older binary started against a newer schema, after a downgrade, therefore reports ready. Phase 6 adds a refusal to start when the binary is older than the schema.
- The Phase 1 tenant isolation proof runs only in the integration suite (`make test-integration`), which the build host runs. `make check` alone does not run it.
