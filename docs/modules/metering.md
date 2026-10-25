# Metering

## TL;DR

A product records usage in Core with its own event IDs. Core checks each event against the brand's meter. It keeps one copy per tenant, meter and event ID. It adds the event to the daily summary. A forwarder then sends the usage to Kill Bill as usage records. It rolls the events up per tenant, Kill Bill unit, subscription and day, and gives each rollup a tracking ID that is a digest of its events. A retry sends the same tracking ID, and Kill Bill records it once. A rollup Kill Bill would record and never bill, dated before its subscription started or on a cancelled subscription, is rejected and reported until staff reassign or discard it. Kill Bill rates and invoices the usage from the catalogue. Core never stores a price. An event that has not reached Kill Bill after 24 hours raises `metering.forward_backlog`. The code is `pkg/metering`.

## What Core stores

| Table | Holds | Never holds |
| --- | --- | --- |
| `meters` | Per brand: slug, unit, the Kill Bill unit it forwards to, the dimension keys an event may carry, and whether it is active or retired | A price |
| `usage_event_keys` | The idempotency record of each event: tenant, meter, event ID, a fingerprint of the payload, and the event it names | |
| `usage_events` | Each event, append-only and partitioned by month, with the Kill Bill unit of its meter at the time, and once forwarded, the tracking ID | A price or an amount of money |
| `usage_summaries` | Quantity and event count per tenant, meter, day and dimension set, derived from the events | |
| `usage_rollups` | Each set of events sent to Kill Bill as one usage record, its tracking ID, the subscription it went to, the start of its period, its retries, the forwarder's claim, and a rejection with its reason | |

Quantities are `int64` micro-units of the meter's unit: one million micro-units are one unit. A sum past the `int64` range is `422 usage_overflow` and changes nothing.

Foreign keys tie the rows together. An event key names its event, checked when the transaction commits, because the key is written first to find a replay. An event names the reservation that settled it and the rollup that sent it.

## Meters

A brand declares its meters in its manifest under `billing.meters`:

```yaml
billing:
  meters:
    - slug: api-calls
      unit: request
      killbill_unit: api-call
      dimensions: [region, method]
```

`unit` is `minute`, `second`, `byte`, `gib_hour`, `request`, `token` or `count`. `killbill_unit` must be a usage unit of the brand's catalogue: apply reads the units from the catalogue file it uploads, or from Kill Bill when the manifest names none, and refuses an unknown unit before anything changes. Up to 16 dimension keys of 1 to 40 lowercase letters, digits and underscores.

With `billing.meters` present, even empty, apply makes the brand's meters exactly that list. A meter left out is retired, never deleted, because events reference it. A retired meter takes no new usage: `404 meter_not_found`. With `billing.meters` absent, staff `operator` manage the meters with `PUT /v1/brands/{id}/meters`, which follows the same rules and audits `brand.meters.updated`. That route refuses a brand whose manifest declares them: `409 conflict`.

## Recording

`POST /v1/tenants/{id}/usage` and `/usage/batch` need `usage:write`, a workload permission no role but owner implies. The library calls are `RecordUsage` and `RecordUsageBatch`, which take a `pgx.Tx`.

| Rule | Answer |
| --- | --- |
| Same tenant, meter and `event_id` with the same payload | The recorded event, `200`, recorded once |
| Same key with another payload | `409 usage_event_conflict` |
| Unknown or retired meter | `404 meter_not_found` |
| A dimension key the meter does not declare, a value over 200 characters | `400 validation_failed` |
| `occurred_at` more than the backfill window ago or ahead | `400 usage_out_of_window` |
| A batch over 1 000 events | `400 validation_failed` |
| Any refused event of a batch | The whole batch is refused and nothing is recorded |

A batch runs in a savepoint of the caller's transaction. A refused event rolls the savepoint back, so a library caller that keeps its transaction after the error, and commits it, finds nothing of the batch.

The backfill window is `billing.usage_window` of the manifest: `backfill_hours`, 168 by default, and `future_seconds`, 300 by default. `occurred_at` defaults to now and is kept to the microsecond, so a replay sends back the stored value. `subscription` optionally names the Kill Bill subscription the usage goes to. A closed tenant records nothing. Library callers still record work a suspended tenant already ran. The HTTP route refuses it, like every other write that adds to what a tenant holds.

Each recording call that records something writes one `metering.usage.recorded` event to the outbox and one audit event, `metering.usage.recorded`, with the count per meter. A replay writes neither. `metering.usage.recorded` is an internal event type that only the forwarder claims. See [foundation.md](foundation.md#events-and-outbox).

## Forwarding

`serve` forwards every `ZS_METERING_FORWARD_INTERVAL`, and a relay that claims only `metering.usage.recorded` forwards a tenant as soon as it records. `zs-core usage forward` runs one pass. A pass does three things:

1. It puts every event that is in no rollup into one, per tenant, Kill Bill unit, requested subscription and day. Only events accepted under dispatch protocol 1 are rolled up, and each rollup stores a fingerprint of the event set it holds. The rollup's tracking ID is `zs-usg-` and a digest of the brand, the tenant, the unit, the requested subscription, the day and the IDs of its events. Its record date is the latest `occurred_at` of its events, and its period starts at the earliest.
2. It sends each due rollup with `POST /1.0/kb/usages`. A rollup without a requested subscription goes to the tenant's first active subscription whose plan prices the unit and that started by the start of the rollup's period. That choice is stored before the first send and never changes, so a retry of the tracking ID never reaches another subscription. A requested subscription must belong to the tenant, be active and price the unit. Kill Bill checks none of this. Core checks it and never sends usage to another account.
3. It emits `metering.forward_backlog`, at most once a day per tenant, for a tenant with an event received more than 24 hours ago that Kill Bill does not hold yet. The events of a discarded rollup do not count.

A forwarder claims one due rollup at a time, under a claim token, for two minutes, and sends it before it claims the next. Only an unsent rollup can be claimed, and only once. The claim is the durable record that a send may follow, so an expired claim is never taken over. A forwarder that stops gives back the rollups it claimed and never started. A crash strands at most the one rollup it held. Every change the forwarder makes to a rollup carries the token, so two forwarders never send one rollup at once, however slow Kill Bill answers.

Before it sends, the forwarder stores the Kill Bill endpoint, tenant and account the usage will land in, once. A rollup already bound to another target waits with `usage_target_unconfirmed` and an audit event, `metering.usage.target_mismatch`, and is never rebound. The forwarder then stores the request body and the start time, and only then posts exactly those bytes.

A failure before the send starts proves Kill Bill saw nothing, so the rollup goes back to unsent. It waits 10 seconds, then twice as long each time, up to a day. Its last error is one of four fixed codes:

| Code | Meaning |
| --- | --- |
| `usage_preflight_unavailable` | Kill Bill is down or unreachable, or the tenant account could not be loaded |
| `usage_subscription_unconfirmed` | Kill Bill did not confirm the subscription |
| `usage_target_unconfirmed` | The Kill Bill target could not be read or does not match the stored one |
| `usage_payload_not_ready` | The request body could not be built |

After the send starts, only two answers are final. A `2xx`, or Kill Bill's refusal of a tracking ID it already holds (`400`, error code 50000), marks the rollup and its events forwarded. Core tries that database write three times. Anything else is an unknown outcome: a timeout, a connection closed after the write, a `5xx`, a `408` or `429`, any other `4xx`, or a refused connection. An unknown outcome is never retried. The rollup stays pending with its stored body and a fixed reason, `usage_send_unconfirmed`, or `usage_transport_unconfirmed` when the answer proves Kill Bill recorded nothing but the start was already stored. It is audited as `metering.usage.send_unconfirmed`, counted in the `Unconfirmed` figure of the forward report and in the `unconfirmed` field of the pass log, and staff resolve it against Kill Bill by hand. This is why Kill Bill recording a rollup twice cannot happen on a retry: there is no retry.

Every pass starts with a sweep that never sends. A claim that ran out before any send started becomes `usage_claim_unconfirmed`, and a started send that nobody finished within ten minutes becomes `usage_send_unconfirmed`. Each is audited as `metering.usage.send_unconfirmed` and counted.

Usage accepted before dispatch protocol 1 is read-only under the database guard. The forwarder never rolls it up, claims a legacy rollup or changes it. `LegacyUnrolled` counts a tenant's legacy events that are in no rollup, so unbilled legacy usage is never silent. A closing tenant that holds such usage cannot complete closure until staff resolve a review item for it. See [tenants.md](tenants.md#suspension-and-closure).

Each forwarded rollup is audited as `metering.usage.forwarded` with the subscription, the tracking ID, the unit and the quantity, and moves the tenant's wallet version on, since Kill Bill's unbilled amount grew.

Kill Bill 0.24.22 records usage dated before its subscription started, and usage on a cancelled subscription, and bills neither. So the forwarder rejects such a rollup instead of sending it:

| Rollup | Reason |
| --- | --- |
| Its period starts before the subscription started, or before every active subscription that takes the unit started | `before_subscription_start` |
| Its subscription is cancelled or expired | `subscription_cancelled` |

A rejected rollup is audited as `metering.rollup_rejected` with its quantity, its events and the reason, and the wallet reconciliation reports it as `rejected_usage` until it is resolved. Staff with the `support` grant resolve it, with a reason:

| Route | Effect |
| --- | --- |
| `POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/reassign` with `subscription` | The rollup goes to that subscription on the next pass. It must be an active subscription of the tenant that prices the unit and started by the start of the rollup's period. Audited as `metering.rollup_reassigned`. |
| `POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/discard` | The rollup is never sent and its usage is not billed. Audited as `metering.rollup_discarded` with its quantity and events. |

Either route refuses a rollup that is not rejected: `409 conflict`. A rollup ID is `rol_`. The reconciliation finding and the audit name it.

Usage a reservation settles is sent by the settlement and is never in a rollup. Its usage event is written only when the settlement completes, already forwarded. A settlement that waits for Kill Bill or that Kill Bill refused leaves no event, so the summaries and the backlog never count usage Kill Bill does not hold. See [wallet.md](wallet.md#settlement).

## Partitions

`usage_events` is partitioned by month of `occurred_at`. The migration creates every month from 2026-01 through 2028-12. A partition sweeper creates the partitions of the current month and the next three every `ZS_USAGE_PARTITION_INTERVAL`, under an advisory lock, so two instances do not race. Creating one that exists does nothing. The sweeper needs only the database, so it runs whether or not Kill Bill is configured.

An event of a month without a partition lands in the default partition, `usage_events_default`. Move such a row by hand before you create that month's partition, because Postgres refuses to create a partition whose range already has rows in the default one.

The events are append-only. A trigger refuses an update of anything but the forwarding columns, and a delete. A `TRUNCATE` trigger of a partitioned table does not fire for a partition truncated directly, so every partition, the default one included, carries its own `TRUNCATE` guard. An operator who must remove rows drops the guard by hand and records why.

## What Kill Bill does with it

Verified on Kill Bill 0.24.22 on the reference stack:

| Behaviour | Found |
| --- | --- |
| `POST /1.0/kb/usages` | `201` with an empty body. Body `{subscriptionId, trackingId, unitUsageRecords: [{unitType, usageRecords: [{recordDate, amount}]}]}`. |
| Amounts | Decimals, kept with nine digits after the point. Zero and negative amounts are accepted. |
| Tracking IDs | Unique per subscription. A second record with a known tracking ID is `400` with error code 50000 and records nothing, whatever its amount. The same tracking ID on another subscription is accepted. |
| Checks Kill Bill does not make | An unknown unit, and a cancelled subscription, are accepted. |
| Another tenant's subscription | `500`, so a Kill Bill server error. Core never sends one. |
| `GET /1.0/kb/usages/{subscriptionId}?startDate&endDate` | The rolled-up amount per unit. Both dates are required. `GET .../{subscriptionId}/{unitType}` narrows it to one unit. No tracking IDs. |
| Record dates | Kill Bill sums a record dated on a day into that day. An invoice leaves out a record dated before the subscription started, even on the same day. Core therefore dates each record at the latest `occurred_at` of its rollup. |
| Invoices | An invoice run with a target date past the end of the first period, `POST /1.0/kb/invoices?accountId&targetDate`, bills the usage as a `USAGE` item with the period's start and end dates, and lists the tracking IDs on the invoice. |
| Dry-run invoice | `TARGET_DATE` a month and a day ahead returns the usage of the current period. See [wallet.md](wallet.md#unbilled-usage). |
| Test clock | `GET /1.0/kb/test/clock` answers. Moving it answers `500`: the reference stack does not run Kill Bill in test mode. |

## Reads

| Route | Reads |
| --- | --- |
| `GET /v1/tenants/{id}/usage/events` | Core's events, newest first, by meter and time |
| `GET /v1/tenants/{id}/usage/summary` | Core's summaries per meter, day and dimension set, and a total per meter, for up to 366 days, the current month by default |
| `GET /v1/tenants/{id}/usage/billed` | Kill Bill's rolled-up usage for one subscription of the tenant or every one, so a product can show both side by side |

All three need `usage:read`. `RebuildSummaries` recomputes a tenant's summaries for a range of days from its events.

## Known limits

- Usage of a day reaches Kill Bill in as many records as there were forward passes with new events that day. Kill Bill sums them.
- A meter's change of Kill Bill unit applies to events recorded after it. Events already recorded keep the unit of their time.
- A rollup whose period starts before its subscription started is rejected as a whole, also when some of its events came after the start. Staff reassign it, or discard it.
- A rollup that keeps failing is retried once a day without end. Its last error and the backlog event say why.

## Closing tenants

Once a tenant's closure has started, `RecordUsage` and `RecordUsageBatch` refuse its usage with `tenant_not_found`, as they do for a closed tenant: the closure invoices what was accepted before it. The settlement of a reservation held before the closure is still taken. `Service.Drained` is what a closure asks before it cancels the subscriptions: it forwards the tenant's usage and answers true only when no protocol 1 event is unrolled, no protocol 1 rollup is pending or rejected, and no reservation is held. See [tenants.md](tenants.md).

## Resolving an unconfirmed rollup

A rollup whose send started and whose outcome Kill Bill never confirmed is frozen: it is never sent again and its record never changes. It does not hold the drain of a closing tenant, but the closure cannot complete while it is unresolved. Staff resolve it with `POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/resolve`, which needs the `support` grant, or `zs-core usage resolve-rollup --brand --tenant --rollup --reason --operator [--decision forwarded|not_billed]`. The evidence is Kill Bill's, which Core reads, and without it the answer is `409`.

- `forwarded`: the tracking ID of the rollup is on a committed invoice of the tenant's account. An invoice that is not committed is no evidence. This shows that Kill Bill billed the record, not its quantity.
- `not_billed`: the cancellation of the closure is done, the account has committed invoices dated on or after the day of it, and none of the committed invoices carries the tracking ID. The quantity of the frozen rollup is recorded as lost, and the audit entry carries it.

The decision is kept in `usage_rollup_resolutions` beside the rollup, with the invoice or the quantity, who and why, and cannot be changed or removed. It is audited as `metering.rollup_resolved`, it is idempotent, and it sends nothing. A resolved rollup is no longer reported by `zs-core doctor`. Only a send that started can be resolved this way: a claim that never started sent nothing, holds the drain, and is sent by the forwarder. Sending a rollup once more is not offered, because it would need a change to the guard on the rollup table.
