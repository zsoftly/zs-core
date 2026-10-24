# Wallet, reservations, metering and quotas as extensions on Kill Bill

## TL;DR

Kill Bill is the financial authority: it holds every customer credit, invoice and payment, and nothing in Core competes with that. Products also need things Kill Bill does not provide. Core adds them as extensions with their own transactional records: a reservation that holds credit before a job starts, a usage path that ends in Kill Bill usage records, and quotas that answer whether a tenant may act now. The wallet a product sees is a view: Kill Bill's credit balance minus what Kill Bill will invoice and has not yet, minus Core's active holds, plus the overdraft allowance. Settlement always ends in Kill Bill, as usage on a subscription or as consumed credit. A reconciliation sweep reports mismatches between Core's records and Kill Bill. This is the Phase 4 extension contract. Where this page and the Phase 4b section differ, the Phase 4b section is right.

## Rule

Core extension tables may hold holds, counters, idempotency and audit. They never hold a customer balance of their own, never record a payment, and never issue an invoice. Every amount that changes what a customer owes or owns is written to Kill Bill, and Core's records reference the Kill Bill object they produced. A reconciliation sweep compares the two and raises `wallet.reconciliation_mismatch` on any drift. It never changes Kill Bill's records.

## Units

Credit and usage quantities are `int64` micro-units. Money is `int64` minor units with an ISO 4217 currency. Prices live in the Kill Bill catalogue, not in Core. Core converts a usage quantity to a credit amount only by asking Kill Bill to rate it (dry-run invoice) or by reading the catalogue price for the unit, and records which it used.

## Wallet view

`GET /v1/tenants/{id}/wallet` returns, per currency: `killbill_credit_minor` (the account's credit balance from Kill Bill), `reserved_minor` (sum of Core reservations in `held`), `available_minor` (credit minus unbilled minus reserved plus the overdraft allowance, which can be negative when a tenant is overspent), `overdue` (from Kill Bill), and `low_balance` (available below the brand's threshold, the only threshold that exists). Credits are granted only through Kill Bill. Promo code redemption and staff credits shipped in Phase 4a as `POST /v1/tenants/{id}/credits/redeem` and `POST /v1/staff/tenants/{id}/credits`. See [billing.md](billing.md). The wallet view reads the credits those routes write. Each credit source works as follows:

- Promo codes: shipped in Phase 4a, see [billing.md](billing.md). A promo or voucher code (`credit_codes` table in Core: brand, code digest, first four characters, amount, currency, cap, per-tenant limit, expiry, credit validity, reason, status) resolves to one Kill Bill credit with the code as the reason.
- Staff grants: shipped in Phase 4a, see [billing.md](billing.md). A staff grant writes a Kill Bill credit with a reason.
- Purchases: a purchase arrives as a Kill Bill payment against a credit invoice through the brand's payment plugin.
- Expiry: Kill Bill refuses zero and negative credits (code 4009), so a sweeper writes an external charge against the account for the unused remainder of Core-issued credits.

## Reservations

Table `reservations`: `id` (`rsv_`), `brand_id`, `tenant_id`, `currency`, `external_ref` (caller key, unique per tenant), `attempt`, `label`, `amount_minor`, `status` (`held`, `settled`, `released`, `expired`), `lease_expires_at`, `settled_minor`, `killbill_ref` (the usage record tracking ID or credit ID that settlement produced), timestamps. Table `reservation_events`: append-only history of reserve, grow, extend, settle, release, expire with actor and correlation ID.

- `Reserve(ctx, tx, scope, {ExternalRef, Attempt, Label, AmountMinor, LeaseDuration})` locks the tenant (advisory lock), reads `available_minor` (credit and unbilled amount cached in process for `ZS_WALLET_CREDIT_CACHE_TTL` and refreshed on a miss, minus held reservations in the same transaction, plus the overdraft allowance, with `503 wallet_unavailable` when the dry run fails), and refuses with `insufficient_credit` and the shortfall when the request exceeds it, or `wallet_on_hold` when the tenant is suspended or the Kill Bill account is blocked.
- Reserve replay: a replay with the same ref and attempt returns the hold when the amount is equal or smaller, and grows it when larger. It never shrinks.
- `Settle(ctx, tx, scope, ExternalRef, Attempt, {Usage: {subscription, unit, quantity_micro, event_id} | CreditMinor})` writes the result to Kill Bill first (a usage record on the subscription, or an external charge when the brand bills prepaid credit directly, because Kill Bill refuses zero and negative credits with code 4009), stores the Kill Bill reference, marks the hold `settled`, releases the remainder.
- Settle with Kill Bill down: if Kill Bill is unreachable the settlement is queued in the outbox and the hold stays `held` with `settlement_pending: true`. The relay completes it, and the hold cannot be reserved against again.
- `Release` returns the hold. `Extend` renews the lease. A sweeper expires holds past their lease and emits `reservation.expired`.
- Library entry points take `pgx.Tx` so a scheduler reserves inside its own transaction (decision 0001). Over HTTP the same calls are idempotent on `external_ref` and `attempt`.

## Metering

Table `meters`: per brand, `slug`, `unit` (`minute`, `second`, `byte`, `gib_hour`, `request`, `token`, `count`), `killbill_unit` (the catalogue usage unit it maps to), `dimensions` (allowed keys), `status`. Table `usage_events`: `id` (`usg_`), `brand_id`, `tenant_id`, `meter_id`, `event_id` (unique per tenant and meter), `quantity_micro`, `dimensions` (JSONB, keys restricted), `resource_ref`, `occurred_at`, `received_at`, `forwarded_at`, `killbill_tracking_id`, `reservation_id`. Append-only, partitioned by month. Table `usage_summaries`: per tenant, meter, dimension set and day, rebuilt from events.

- `RecordUsage` and `RecordUsageBatch` (up to 1 000, all or nothing) are idempotent on `(tenant, meter, event_id)`. A different payload for the same key returns `409 usage_event_conflict`. `occurred_at` outside the brand's backfill window (default 7 days back, 5 minutes forward) is rejected.
- Forwarding: events are rolled up per tenant, subscription, unit and period by the forwarder and written to Kill Bill as usage records with `trackingId` set to a deterministic digest of the rollup, so Kill Bill deduplicates a retry. The forwarder runs from the outbox. An event not forwarded within 24 hours raises `metering.forward_backlog`.
- Rating and pricing are Kill Bill's: the catalogue defines the usage unit prices, tiers and billing mode. Core never stores a price.
- `GetUsageSummary` and `ListUsageEvents` read Core's tables. `GET /v1/tenants/{id}/usage/billed` reads Kill Bill's rolled-up usage for the period so a product shows both.

## Quotas and entitlements

Table `quotas`: per brand, `feature`, `plan` (catalogue plan name or `*`), `limit` (integer), `window` (`none`, `day`, `month`, `concurrent`), `meter_slug` (optional, for usage-backed limits). Table `quota_overrides`: per tenant, feature, limit, reason, expiry, staff actor. Table `quota_counters`: per tenant, feature, window start, current value. It is updated transactionally by `Consume` and `Release` for `concurrent` windows and rebuilt from usage summaries for `day` and `month`.

- `CheckEntitlement(ctx, scope, feature) -> {allowed, limit, used, remaining, reason, plan}` reads the tenant's active Kill Bill subscriptions on every check, picks the matching quota rows (override, then plan, then `*`), and compares with the counter. It fails closed: no active subscription, blocked account, suspended tenant or Kill Bill unreachable all return `allowed: false` with a reason.
- `Consume(ctx, tx, scope, call, feature, ref, n)` and `Release` are for `concurrent` windows (for example, running jobs) and are what a product calls at admission.
- Quotas are declared in the brand manifest under `entitlements:`, next to the catalogue, so plan limits and plan prices change together.

## Routes

| Method and path | Permission |
| --- | --- |
| `GET /v1/tenants/{id}/wallet` | `billing:read` |
| `GET, POST /v1/tenants/{id}/reservations`, and `POST .../reservations/{ref}/settle`, `.../release`, `.../extend` | `wallet:reserve` |
| `POST /v1/tenants/{id}/usage`, `/usage/batch` | `usage:write` |
| `GET /v1/tenants/{id}/usage/events`, `/usage/summary`, `/usage/billed` | `usage:read` |
| `GET /v1/tenants/{id}/entitlements/{feature}` | `tenant:read` |
| `POST /v1/tenants/{id}/entitlements/{feature}/consume`, `.../release`, `.../extend` | `quota:consume` |
| `GET, PUT /v1/brands/{id}/meters`, `/quotas` | staff `operator` |
| `POST /v1/staff/tenants/{id}/quota-overrides` | staff `support` |
| `POST /v1/staff/tenants/{id}/reservations/{ref}/settle`, `.../release`, `.../extend`, `POST /v1/staff/tenants/{id}/entitlements/{feature}/release`, and `POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/reassign`, `.../discard` | staff `support`, with a reason |

Built-in permissions added: `wallet:reserve`, `usage:write`, `usage:read`, `quota:consume`.

## Where Phase 4b differs

Phase 4b built this contract on Kill Bill 0.24.22, as [wallet.md](wallet.md), [metering.md](metering.md) and [quotas.md](quotas.md) describe. It differs from the text above in these places, on purpose:

- One currency per wallet. A Kill Bill account has one currency, the brand's first. The view answers for that currency only, and adds `overdraft_minor` and `pending_settlements`.
- Unbilled usage. Usage billed in arrears leaves the Kill Bill credit untouched until it is invoiced. So the view and `Reserve` also subtract `unbilled_minor`, the amount of a Kill Bill dry-run invoice a month and a day ahead, which Kill Bill rates. When the dry run fails, the view shows `null` and `Reserve` fails closed with `503 wallet_unavailable`.
- Pending holds never expire. A settlement waiting for Kill Bill keeps its hold past its lease, and no settlement is ever abandoned. The sweeper completes it once Kill Bill answers, with a backoff per reservation, whatever became of its outbox event. A settlement waiting a day is audited as `wallet.settlement_stale` and reported as `pending_settlement_stale`.
- Credit cache in memory. `Reserve` relies on a Kill Bill credit cached in the process for `ZS_WALLET_CREDIT_CACHE_TTL`, at most a minute, and only while the tenant's wallet version, `wallet_versions`, is unchanged. Every change to the credit or the unbilled amount that Core makes or detects moves the version on, so two instances never reserve against a credit one of them spent. Hold reasons are read live. The credit is never written to the database, so Core holds no copy of a balance. The view always reads Kill Bill.
- Holders. A reservation and a quota lease belong to the membership or API key that made them. Only that holder settles, releases, extends or grows them. Staff with the `support` grant and a reason may act on another holder's, audited as a staff override.
- Settlement checked against the hold. Under the tenant lock, the settlement's catalogue value, or its `credit_minor`, may not exceed the hold: `409 settlement_exceeds_hold`. A unit the subscription's phase does not price cannot settle a reservation: `422 unit_unrated`.
- Settlement in steps. Core writes the settlement's intent and the chosen subscription to the reservation, calls Kill Bill, then completes. The HTTP route and the sweeper release the tenant lock during the Kill Bill call, so `Reserve` is never blocked by it. A library `Settle` runs in the caller's transaction.
- Kill Bill keys from the caller's keys. The usage tracking ID and the charge description are digests of the brand, the tenant, `external_ref`, `attempt` and the kind, never of the reservation's ID. A library caller that rolled back after `Settle` and retries with the same ref and attempt meets the object of its first try. A duplicate answer counts only for the amount sent before: otherwise `409 settlement_conflict`.
- Usage settlement. The subscription and the event ID are optional. Without a subscription, the tenant's active subscription whose plan prices the unit takes the usage. When a meter maps to the unit, the usage is also a usage event, written only when the settlement completes and marked forwarded, so the forwarder never sends it twice and no failed settlement leaves one behind. `settled_minor` is the catalogue price of the first tier, recorded as `rated_by: catalogue_price`, because Kill Bill rates usage only on the invoice.
- Settlement waiting. The HTTP route answers `202` while the settlement waits for Kill Bill. The relay defers the event while Kill Bill stays down, so a settlement is never quarantined for it. `wallet.settlement.requested` is an internal event type that a generic outbox reader never claims.
- Suspended tenants. Over HTTP a suspended tenant still settles, releases and extends its reservations and releases its quota leases. It never reserves, grows or consumes.
- Usage record dates. Kill Bill 0.24.22 leaves a usage record dated before the subscription started off the invoice, even on the same day. So a record is dated at the latest event of its rollup, not at the start of the day.
- Unit checks. Kill Bill 0.24.22 accepts usage of any unit, in any amount, on any subscription, cancelled ones included. Core checks the subscription's owner, its state, its start and its plan before every send.
- Rejected usage. A rollup whose period starts before its subscription started, or whose subscription is cancelled, is rejected with its reason, audited as `metering.rollup_rejected` and reported as `rejected_usage`, because Kill Bill would record it and never bill it. Staff reassign it to another subscription or discard it.
- Forwarder claims. A forwarder claims one rollup at a time under a claim token, stores the request body before it sends, and never retries an unknown outcome: that rollup waits for staff as unconfirmed. So two forwarders never send one rollup at once, and Kill Bill never records one twice. See [metering.md](metering.md#forwarding).
- Partitions. A partition sweeper keeps the `usage_events` partitions three months ahead, with or without Kill Bill, and the migration creates them through 2028-12. No partition can be truncated.
- Consume and release take a reference. `Consume(ctx, tx, scope, call, feature, ref, n)` holds `n` for `ref`, such as a job ID, and `Release` returns what that reference holds. A retry of either changes nothing, and a release never returns more than was consumed.
- Leases expire. A concurrent lease lasts an hour by default unless it is extended or released. The quota sweeper releases it after that and audits `quota.lease.expired`.
- Day and month windows are consumed. `Consume` counts against a day or month window's counter under the tenant-feature lock. The counter decides admission. The usage summaries only raise it, and the drift is audited.
- Override windows. An override takes the window of the plan's quota, or names its own, and applies only to the matching window. A clearing override cuts off the ones before it for good.
- No subscription cache. `CheckEntitlement` reads Kill Bill on every call, so a hold or a cancellation fails closed at once. Consume and the check share that read.
- Reconciliation. The view comes from Kill Bill, so it cannot drift. The sweep checks Core's records against Kill Bill instead. It compares:
  - Kill Bill credits with Core's credits
  - settlement charges with settled reservations
  - each tracking ID an invoice lists with the tracking IDs Core sent
  - Kill Bill's usage per subscription, unit and month with what Core forwarded and settled, for the backfill window plus two months, cancelled subscriptions of the last three months included
  - each held reservation with its own history
  - the settlements waiting for more than a day, and the rejected rollups
  - the holds with the credit less the usage part of the unbilled amount, as an informational finding

  It runs hourly per tenant. `zs-core wallet sweep` runs it for every tenant.
- Meters and quotas through HTTP. `PUT /v1/brands/{id}/meters` and `/quotas` refuse a brand whose manifest declares them, so configuration as code stays the source.
- Low-balance threshold. The threshold is the brand's, `billing.wallet.low_balance_minor`. A tenant threshold waits for a route that sets it.
- `insufficient_credit` is `409`, like the other wallet refusals, and the shortfall is in the message and, for library callers, in `wallet.Shortfall`.
- Names. The API field is `overdue`, the manifest field is `meter` and the column is `meter_slug`. The reconciliation kinds in code are `unknown_credit`, `unknown_charge`, `missing_credit`, `missing_charge`, `unknown_usage`, `missing_usage`, `held_mismatch`, `pending_settlement_stale`, `rejected_usage` and `over_reserved`.
- Permissions. `wallet:reserve`, `usage:write` and `quota:consume` are workload permissions. Only owners hold them through the role. A team grants them, or a service API key carries them. Admins hold `usage:read`. Billing admins hold none of the four. Library callers still record usage for a suspended tenant, while HTTP refuses it.
- Usage idempotency. `usage_events` is partitioned by month, and a unique index there must include the month. So the key of each event is kept in `usage_event_keys`.

## Tests that gate this extension

- Reserve, grow, settle under concurrent callers for one tenant yields consistent holds, and the settled amounts appear in Kill Bill as usage records with no duplicates after a forced retry.
- The wallet view equals the Kill Bill credit, minus unbilled usage, minus held reservations, plus the overdraft allowance after a randomised sequence. The reconciliation sweep reports no mismatch, and reports exactly one when a Kill Bill credit is added behind Core's back.
- Settlement with Kill Bill down queues and completes once Kill Bill is back, the hold cannot be reused meanwhile, and it never expires.
- Usage idempotency, dimension validation, backfill window, deterministic tracking IDs on forward.
- Quota: plan limit, tenant override wins, `concurrent` consume and release, fail closed on Kill Bill down.
- Brand A's meters, quotas and codes do not resolve in brand B. Tenant X cannot read tenant Y's holds or usage.
