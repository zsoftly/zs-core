# 0001. Core ships as a library and a service from one module

Status: accepted, 2026-10-20.

## Context

The CI runner control plane reserves, settles and releases credits inside the scheduler's own Postgres transaction, together with admission and usage records. That is what gives it exactly-once accounting. A remote call cannot take part in that transaction. The ZCP Core API has no such need and is happy with HTTP.

## Decision

`github.com/zsoftly/zs-core` is one Go module. The packages under `pkg/` are importable and accept a `pgx.Tx` so a product can run Core logic inside its own transaction against a shared database. The binary `cmd/zs-core` serves the same packages over HTTP at `/v1`. There is one implementation of every rule; the HTTP layer only translates. A product chooses library or HTTP per call site, and may use both.

## Consequences

- Library callers share the Core schema. Core migrations run before the product's and are forward-only.
- Reservation, settlement and release are keyed by caller-supplied IDs so a retry over HTTP is safe.
- Every rule is tested once, at the package level, with the HTTP tests covering translation only.
- Products that embed Core are pinned to a Core version and upgrade by running its migrations.
