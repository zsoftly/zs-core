# Wallet, reservations, metering and quotas as extensions on Kill Bill

## TL;DR

Kill Bill is the financial authority: it holds every customer credit, invoice and payment, and nothing in Core competes with that. Products also need things Kill Bill does not provide. Core adds them as extensions with their own transactional records: a reservation that holds credit before a job starts, a usage path that ends in Kill Bill usage records, and quotas that answer whether a tenant may act now. The wallet a product sees is a view: Kill Bill's credit balance minus Core's active reservations. Settlement always ends in Kill Bill, as usage on a subscription or as consumed credit, so the two never disagree for longer than one sweep. This is the Phase 4 extension contract.

## Rule

Core extension tables may hold holds, counters, idempotency and audit. They never hold a customer balance of their own, never record a payment, and never issue an invoice. Every amount that changes what a customer owes or owns is written to Kill Bill, and Core's records reference the Kill Bill object they produced. A reconciliation sweep compares the two and raises `wallet.reconciliation_mismatch` on any drift; it never changes Kill Bill's records.

## Units

Credit and usage quantities are `int64` micro-units. Money is `int64` minor units with an ISO 4217 currency. Prices live in the Kill Bill catalogue, not in Core; Core converts a usage quantity to a credit amount only by asking Kill Bill to rate it (dry-run invoice) or by reading the catalogue price for the unit, and records which it used.

## Wallet view

`GET /v1/tenants/{id}/wallet` returns, per currency: `killbill_credit_minor` (the account's credit balance from Kill Bill), `reserved_minor` (sum of Core reservations in `held`), `available_minor` (the difference, floored at the brand's overdraft allowance), `overdue_state` (from Kill Bill), and `low_balance` (available below the brand or tenant threshold). Credits are granted only through Kill Bill. Each credit source works as follows:

- Promo codes: a promo or voucher code (`credit_codes` table in Core: brand, code digest, amount, currency, caps, expiry, reason) resolves to one Kill Bill credit with the code as the reason.
- Staff grants: a staff grant writes a Kill Bill credit with a reason.
- Purchases: a purchase arrives as a Kill Bill payment against a credit invoice through the brand's payment plugin.
- Expiry: a sweeper writes the negative Kill Bill credit for the unused remainder of Core-issued credits.

## Reservations

Table `reservations`: `id` (`rsv_`), `brand_id`, `tenant_id`, `currency`, `external_ref` (caller key, unique per tenant), `attempt`, `label`, `amount_minor`, `status` (`held`, `settled`, `released`, `expired`), `lease_expires_at`, `settled_minor`, `killbill_ref` (the usage record tracking ID or credit ID that settlement produced), timestamps. Table `reservation_events`: append-only history of reserve, grow, extend, settle, release, expire with actor and correlation ID.

- `Reserve(ctx, tx, scope, {ExternalRef, Attempt, Label, AmountMinor, LeaseDuration})` locks the tenant (advisory lock), reads `available_minor` (Kill Bill balance cached with a short TTL and refreshed on a miss, minus held reservations in the same transaction), and refuses with `insufficient_credit` and the shortfall when the request exceeds it, or `wallet_on_hold` when the tenant is suspended or the Kill Bill account is blocked.
- Reserve replay: a replay with the same ref and attempt returns the hold when the amount is equal or smaller, and grows it when larger. It never shrinks.
- `Settle(ctx, tx, scope, ExternalRef, Attempt, {Usage: {subscription, unit, quantity_micro, event_id} | CreditMinor})` writes the result to Kill Bill first (a usage record on the subscription, or a negative credit when the brand bills prepaid credit directly), stores the Kill Bill reference, marks the hold `settled`, releases the remainder.
- Settle with Kill Bill down: if Kill Bill is unreachable the settlement is queued in the outbox and the hold stays `held` with `settlement_pending: true`. The relay completes it, and the hold cannot be reserved against again.
- `Release` returns the hold. `Extend` renews the lease. A sweeper expires holds past their lease and emits `reservation.expired`.
- Library entry points take `pgx.Tx` so a scheduler reserves inside its own transaction (decision 0001). Over HTTP the same calls are idempotent on `external_ref` and `attempt`.

## Metering

Table `meters`: per brand, `slug`, `unit` (`minute`, `second`, `byte`, `gib_hour`, `request`, `token`, `count`), `killbill_unit` (the catalogue usage unit it maps to), `dimensions` (allowed keys), `status`. Table `usage_events`: `id` (`usg_`), `brand_id`, `tenant_id`, `meter_id`, `event_id` (unique per tenant and meter), `quantity_micro`, `dimensions` (JSONB, keys restricted), `resource_ref`, `occurred_at`, `received_at`, `forwarded_at`, `killbill_tracking_id`, `reservation_id`. Append-only, partitioned by month. Table `usage_summaries`: per tenant, meter, dimension set and day; rebuilt from events.

- `RecordUsage` and `RecordUsageBatch` (up to 1 000, all or nothing) are idempotent on `(tenant, meter, event_id)`; a different payload for the same key returns `409 usage_event_conflict`. `occurred_at` outside the brand's backfill window (default 7 days back, 5 minutes forward) is rejected.
- Forwarding: events are rolled up per tenant, subscription, unit and period by the forwarder and written to Kill Bill as usage records with `trackingId` set to a deterministic digest of the rollup, so Kill Bill deduplicates a retry. The forwarder runs from the outbox; an event not forwarded within 24 hours raises `metering.forward_backlog`.
- Rating and pricing are Kill Bill's: the catalogue defines the usage unit prices, tiers and billing mode. Core never stores a price.
- `GetUsageSummary` and `ListUsageEvents` read Core's tables; `GET /v1/tenants/{id}/usage/billed` reads Kill Bill's rolled-up usage for the period so a product shows both.

## Quotas and entitlements

Table `quotas`: per brand, `feature`, `plan` (catalogue plan name or `*`), `limit` (integer), `window` (`none`, `day`, `month`, `concurrent`), `meter_slug` (optional, for usage-backed limits). Table `quota_overrides`: per tenant, feature, limit, reason, expiry, staff actor. Table `quota_counters`: per tenant, feature, window start, current value; updated transactionally by `Consume` and `Release` for `concurrent` windows and rebuilt from usage summaries for `day` and `month`.

- `CheckEntitlement(ctx, scope, feature) -> {allowed, limit, used, remaining, reason, plan}` reads the tenant's active Kill Bill subscriptions (cached, short TTL), picks the matching quota rows (override, then plan, then `*`), and compares with the counter. It fails closed: no active subscription, blocked account, suspended tenant or Kill Bill unreachable all return `allowed: false` with a reason.
- `Consume(ctx, tx, scope, feature, n)` and `Release` are for `concurrent` windows (for example, running jobs) and are what a product calls at admission.
- Quotas are declared in the brand manifest under `entitlements:`, next to the catalogue, so plan limits and plan prices change together.

## Routes

| Method and path | Permission |
| --- | --- |
| `GET /v1/tenants/{id}/wallet` | `billing:read` |
| `POST /v1/tenants/{id}/wallet/redeem` `{code}` | `billing:manage` |
| `POST /v1/staff/tenants/{id}/wallet/credits` | staff `billing` |
| `GET, POST /v1/tenants/{id}/reservations`; `POST .../reservations/{ref}/settle`, `.../release`, `.../extend` | `wallet:reserve` |
| `POST /v1/tenants/{id}/usage`, `/usage/batch`; `GET /v1/tenants/{id}/usage/events`, `/usage/summary`, `/usage/billed` | `usage:write`, `usage:read` |
| `GET /v1/tenants/{id}/entitlements/{feature}`; `POST .../entitlements/{feature}/consume`, `.../release` | `tenant:read`; `quota:consume` |
| `GET, PUT /v1/brands/{id}/meters`, `/quotas`; `POST /v1/staff/tenants/{id}/quota-overrides` | staff `operator`; staff `support` |

Built-in permissions added: `wallet:reserve`, `usage:write`, `usage:read`, `quota:consume`.

## Tests that gate this extension

- Reserve, grow, settle under concurrent callers for one tenant yields consistent holds, and the settled amounts appear in Kill Bill as usage records with no duplicates after a forced retry.
- The wallet view equals Kill Bill balance minus held reservations after a randomised sequence; the reconciliation sweep reports no mismatch, and reports exactly one when a Kill Bill credit is added behind Core's back.
- Settlement with Kill Bill down queues and completes on relay; the hold cannot be reused meanwhile.
- Usage idempotency, dimension validation, backfill window, deterministic tracking IDs on forward.
- Quota: plan limit, tenant override wins, `concurrent` consume and release, fail closed on Kill Bill down.
- Brand A's meters, quotas and codes do not resolve in brand B; tenant X cannot read tenant Y's holds or usage.
