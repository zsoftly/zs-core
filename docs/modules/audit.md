# Audit

## TL;DR

The audit module records who did what, to which tenant, and with what result. Every write in Core appends one event in the same transaction as the change, so the event exists exactly when the change does. Each event carries the actor, brand, tenant, action, reason, outcome and a correlation ID. The table refuses `UPDATE`, `DELETE` and `TRUNCATE`. A correction is a new event. Tenant queries always name one brand and one tenant. Brand events, such as a brand apply or a staff grant, belong to one brand and no tenant and are queried separately. Queries filter by actor, action and time, and page newest first with an opaque cursor. Export is Phase 6.

## Writing events

```go
err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
    // ... the change itself ...
    _, err := audit.Write(ctx, tx, sc, audit.Entry{
        Actor:         events.Actor{Type: events.ActorUser, ID: subjectRef},
        Action:        "tenant.member.added",
        Reason:        "Invited by owner",
        Outcome:       audit.OutcomeSuccess,
        CorrelationID: requestID,
    })
    return err
})
```

| Field | Rule |
| --- | --- |
| Actor type | `user`, `api_key`, `staff`, `service`, `provider` or `system`. |
| Actor ID | Required except for `system`. At most 255 bytes. Never an email address. |
| Action | Dotted lowercase name, such as `wallet.grant.created`. |
| Reason | Optional, at most 1000 characters. |
| Outcome | `success`, `failure` or `denied`. |
| Correlation ID | Required, at most 128 bytes. Usually the request ID. |
| Occurred at | Defaults to now; stored with microsecond precision. |

`Write` requires a `pgx.Tx`. If the caller rolls back, the audit event disappears with the change.

## Querying events

`audit.Query(ctx, db, scope, filter)` returns events of the scope's brand and tenant only. The scope must hold both. A partial scope returns `scope_required` before any SQL runs.

| Filter | Meaning |
| --- | --- |
| `ActorID` | Exact actor ID. |
| `Action` | Exact action. |
| `OccurredAfter` | Inclusive lower bound. |
| `OccurredBefore` | Exclusive upper bound. |
| `Limit` | 1 to 200, default 50. |
| `Cursor` | `NextCursor` of the previous page. |

Results are ordered by `occurred_at` then ID, newest first. The cursor encodes the position after the last event of the page. It cannot widen a query. It moves the position inside the brand and tenant the query names.

## HTTP

`GET /v1/audit/events` exposes the same queries to staff with an `operator` grant in the token's brand. With `tenant_id` it returns that tenant's events inside the caller's brand. Without it, it returns the brand events. `GET /v1/tenants/{id}/audit/events` returns one tenant's events to a member or an API key of that tenant. The caller needs `audit:read`. It takes the same parameters except `tenant_id`. The tenant audit actions are listed in [tenants.md](tenants.md#audit-actions). The other query parameters are `limit`, `cursor`, `actor_id`, `action`, `occurred_after` and `occurred_before` (RFC 3339). The response is:

```json
{"data": [{"id": "aud_...", "brand_id": "brd_...", "tenant_id": "tnt_...",
           "actor": {"type": "user", "id": "..."}, "action": "tenant.member.added",
           "reason": "...", "outcome": "success", "correlation_id": "...",
           "occurred_at": "2026-10-20T12:00:00.123456Z", "details": {}}],
 "next_cursor": null}
```

`tenant_id` is null for a brand event. `details` is present when the event carries structured detail.

## Brand events

Some writes belong to a brand and no tenant. These are each change of a brand apply, the recorded manifest, staff grants and new identities. `audit.WriteBrand(ctx, tx, brand, entry)` stores them with a NULL tenant, and `audit.QueryBrand(ctx, db, brand, filter)` reads them. A tenant query never returns them, because it filters on `tenant_id = $2`. `Entry.Details` holds structured detail, such as the diff of a change, up to 64 KiB. It never holds a secret. Brand apply writes redacted fields without values.

## Isolation

`pkg/audit/audit_integration_test.go` holds the isolation test. It writes identical events, same actor, action and time, for brand A, for brand B with the same tenant UUID, and for a second tenant in brand A. Each scoped query returns only its own three events, a cursor from one scope does not leak into another, and a partial scope is refused for both writes and reads.

## Storage

Table `audit_events`, created by `migrations/000003_audit_events.up.sql`. `migrations/000007_brand_audit_events.up.sql` allows a NULL tenant for brand events and adds `details`. Indexes cover brand, tenant and time, with variants for actor and action. A trigger rejects every `UPDATE`, `DELETE` and `TRUNCATE`.
