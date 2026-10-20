# 0001. Core ships as a library and a service from one module

## TL;DR

ZS Core is one Go module. Its packages under `pkg/` are importable and take a `pgx.Tx`. A product can run Core logic inside its own database transaction. The same packages are served over HTTP at `/v1` by `cmd/zs-core`. Every rule has one implementation, and a product picks library or HTTP per call site. Library callers share the Core schema and run Core migrations first.

Status: accepted, 2026-10-20.

## Context

The CI runner control plane reserves, settles and releases credits inside the scheduler's own Postgres transaction, together with admission and usage records. That is what gives it exactly-once accounting. A remote call cannot join that transaction. The ZCP Core API has no such need and uses HTTP.

## Decision

`github.com/zsoftly/zs-core` is one Go module. The packages under `pkg/` are importable and accept a `pgx.Tx` so a product can run Core logic inside its own transaction against a shared database. The binary `cmd/zs-core` serves the same packages over HTTP at `/v1`. Every rule has one implementation. The HTTP layer only translates. A product chooses library or HTTP per call site, and may use both.

## Consequences

- Library callers share the Core schema. Core migrations run before the product's and are forward-only.
- Reservation, settlement and release are keyed by caller-supplied IDs so a retry over HTTP is safe.
- Every rule is tested once, at the package level, with the HTTP tests covering translation only.
- Products that embed Core are pinned to a Core version and upgrade by running its migrations.
