# Provenance

## TL;DR

Some foundation code in this repository started in the ZCP Core API, which ZSoftly also owns and publishes under Apache 2.0. Each entry below names the file it came from and the commit it was taken at. Lifted code was renamed to this module, its error codes moved from the old prefix to `ZS-`, and anything specific to the original product was removed. Everything not listed here was written for this repository.

## Entries

Source repository: `zsoftly/zcp-cmp-core`, commit `c5c7fe3cefb6d6b0f4024230cfb55d9c7430006a`.

| This repository | Source path | Changes |
| --- | --- | --- |
| `pkg/errors/errors.go` | `zcp/api/pkg/errors/errors.go` | Kept the `Code`, `Error`, `New`, `Wrap` and `IsCode` design. Replaced the code table with new `ZS-` codes, added snake_case names, HTTP statuses, the retryable flag and the envelope mapping. |
| `pkg/idgen/public.go` | `zcp/api/pkg/idgen/idgen.go` | Kept the prefixed public ID idea and prefix validation. The UUIDv7 generator, the Crockford encoding and the prefix registry are new; the source used an external UUID package. |
| `pkg/idempotency/idempotency.go`, `memory.go`, `postgres.go` | `zcp/api/pkg/idempotency/idempotency.go`, `memory.go`, `postgres.go` | Kept the reserve, complete and release model, the lease takeover rule and the upsert reservation SQL. Rekeyed from a principal to brand, tenant and actor, dropped stored response headers, added `Do`, canonical JSON fingerprints and the transaction-capable store. |
| `pkg/events/events.go` | `zcp/api/pkg/events/events.go` | Kept the envelope validation helpers for event types and `traceparent`, and the actor type list. Replaced the envelope fields with brand and tenant, and dropped the legacy envelope version. |
| `pkg/outbox/outbox.go`, `reader.go`, `relay.go` | `zcp/api/internal/platform/outbox/outbox.go`, `postgres_reader.go`, `relay.go` | Kept the writer, the reader, the retry and quarantine SQL and the relay loop. The writer now takes a `pgx.Tx` and stores the whole envelope as JSON. |
| `migrations/000002_outbox.up.sql` | `zcp/api/internal/platform/postgrestest/migrations/identity/000001_outbox.sql` | Reduced to the columns the new envelope needs, with brand and tenant columns. |
| `internal/postgrestest/postgrestest.go` | `zcp/api/internal/platform/postgrestest/database.go` | Kept the container start-up and the loopback check for an external URL. Replaced per-test databases with per-test schemas and the embedded migration runner. |
