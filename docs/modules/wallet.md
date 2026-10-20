# Wallet and reservations

## TL;DR

Kill Bill holds every customer credit. The wallet is a view. Available credit is the Kill Bill credit, minus what Kill Bill will invoice and has not yet, minus the holds Core keeps, plus the brand's overdraft allowance. Kill Bill rates the unbilled amount itself, in a dry-run invoice. A reservation holds credit before a job starts. The caller's `external_ref` and `attempt` key it, so a retry is safe, and it belongs to the membership or API key that made it. A settlement is checked against its hold, written to the reservation as an intent, then written to Kill Bill as a usage record or an external charge. Core then stores the Kill Bill reference. If Kill Bill cannot take it, the settlement waits in the reservation and the sweeper completes it once Kill Bill answers. A waiting hold keeps counting and never expires. A reconciliation sweep reports mismatches and never writes to Kill Bill. Core never stores a balance, a payment, an invoice or a price. The code is `pkg/wallet`.

## What Core stores

| Table | Holds | Never holds |
| --- | --- | --- |
| `reservations` | The caller's key, the holder that made it, the amount held in minor units of the account currency, the status and the lease. While a settlement waits: its request, its fingerprint, the chosen subscription, its estimate and the worker's claim. After settlement: the Kill Bill reference it produced. | A balance or a price |
| `reservation_events` | Append-only history: reserve, grow, extend, settle, a queued, failed or stale settlement, release, expire, with the actor and the correlation ID | Amounts other than the hold |
| `wallet_versions` | A version per tenant, moved on by every change to the tenant's Kill Bill credit or unbilled amount that Core makes or detects | Amounts |
| `tenant_alerts` | When a rate-limited event, such as `wallet.low_balance`, was last emitted for a tenant | Amounts |
| `wallet_reconciliation_findings` | Each Kill Bill object the reconciliation could not explain, once | Corrections |
| `wallet_reconciliations` | When each tenant was last reconciled | Amounts |

The Kill Bill credit and unbilled amount a reservation relies on are cached in the process memory of the Core instance. They are never written to the database. See [The credit cache](#the-credit-cache).

## The view

`GET /v1/tenants/{id}/wallet` needs `billing:read`. Every field comes from Kill Bill on every call, except the holds, which come from Core. Core reads the unbilled amount first, then the credit, so usage invoiced in between is counted twice rather than never.

| Field | Meaning |
| --- | --- |
| `killbill_credit_minor` | The account credit, Kill Bill's `accountCBA` |
| `unbilled_minor` | What Kill Bill will invoice by the end of the current period and has not yet: the `amount` of a dry-run invoice. `null` when the dry run fails. |
| `reserved_minor` | The sum of the held reservations, pending settlements included |
| `available_minor` | Credit minus unbilled minus reserved plus the brand's overdraft allowance, the same headroom Reserve uses. Negative when the tenant is overspent. `null` when `unbilled_minor` is. |
| `overdraft_minor` | `billing.wallet.overdraft_minor` of the brand manifest, 0 by default |
| `pending_settlements` | Held reservations whose settlement waits for Kill Bill |
| `overdue`, `on_hold`, `hold_reasons` | Kill Bill's overdue state and the holds, as `GET /v1/tenants/{id}/billing` reports them |
| `low_balance`, `low_balance_threshold_minor` | True when available is below `billing.wallet.low_balance_minor`. 0 turns it off. |

In short, `available_minor = killbill_credit_minor - unbilled_minor - reserved_minor + overdraft_minor`. The view and `Reserve` use this one function, `wallet.Available`.

## Unbilled usage

Kill Bill invoices arrears usage at the end of its period. Only then does it take the amount from the credit. Until then the credit looks untouched. So the wallet asks Kill Bill what it will invoice. It calls the dry-run invoice with a target date one month and one day ahead. Nothing is invoiced. The call is `POST /1.0/kb/invoices/dryRun?accountId=<account>&targetDate=<today plus a month and a day>` and the body `{"dryRunType": "TARGET_DATE"}`. Verified on Kill Bill 0.24.22:

| Dry run | Answer |
| --- | --- |
| `TARGET_DATE` today | `204`. Usage in arrears is not due before the end of its period. |
| `TARGET_DATE` a month and a day ahead | The `USAGE` item of the current period and the `RECURRING` items due by then, such as the next period of a monthly plan. `amount` is their sum. Kill Bill reports the credit it would apply as a separate `CBA_ADJ` item. `amount` leaves it out. |
| `UPCOMING_INVOICE` | Only the next invoice. When a trial ends before the usage period, it holds the recurring charge and not the usage, so Core does not use it. |
| Nothing to invoice | `204`, read as 0 |
| Without `X-Killbill-CreatedBy` | `400`, so Core sends the audit headers |

`unbilled_minor` is that `amount`. It includes recurring charges due by the end of the period, so it errs towards less available credit, never more. When the dry run fails, the view answers with `unbilled_minor` and `available_minor` set to `null`, and `Reserve` fails closed with `503 wallet_unavailable`. A hold is never granted against an unknown amount.

When the wallet is low, `wallet.low_balance` is written to the outbox, at most once every 24 hours per tenant. A reservation that leaves the wallet low emits it too.

## The credit cache

`Reserve` may rely on the Kill Bill credit and unbilled amount it last read for a tenant, for at most `ZS_WALLET_CREDIT_CACHE_TTL`, and only while the tenant's wallet version is the one it read them at. Under the tenant lock, `Reserve` reads the sum of the holds and the version in one statement, so both come from one snapshot. A different version means another instance changed the credit: `Reserve` reads Kill Bill again, the unbilled amount first. Hold reasons, a suspended tenant and a blocked or overdue Kill Bill account, are read live on every call and never cached.

The version moves on in the same transaction as each of these:

| Change | Where |
| --- | --- |
| A settlement completes, as a charge or as usage | `pkg/wallet` |
| A credit code is redeemed, or a staff credit granted | `pkg/billing` |
| A credit expires. The expiry charge is written under the tenant's wallet lock, so no `Reserve` reads between the charge and the new version. | `pkg/billing` |
| Forwarded usage raises the unbilled amount | `pkg/metering` |
| The reconciliation reports a credit or charge it did not expect | `pkg/wallet` |

So two Core instances never reserve against a credit one of them already spent.

## Reservations

| Call | Rule |
| --- | --- |
| `Reserve` | Takes the tenant's advisory lock in the caller's transaction. Available is the Kill Bill credit, less the unbilled amount, minus the held reservations read in that transaction, plus the overdraft allowance, as in the view. When the unbilled amount is unknown it is `503 wallet_unavailable`. More than that is `409 insufficient_credit`. The message names the shortfall, and library callers read it with `wallet.Shortfall`. A suspended tenant, a blocked Kill Bill account or an overdue state that disables entitlement is `409 wallet_on_hold`. |
| Reserve replay | The same `external_ref` and `attempt` with an equal or smaller amount returns a held reservation unchanged. A larger amount grows it by the difference, checked against what is available. A hold never shrinks. A reservation that was settled, released or expired is `409 reservation_closed`, whatever the amount. One whose settlement waits for Kill Bill cannot grow: `409 reservation_settlement_pending`. |
| `Settle` | See below. |
| `Release` | Returns a held reservation. Releasing a released one returns it. A settled or expired one is `409 reservation_not_held`. One whose settlement waits is `409 reservation_settlement_pending`. |
| `Extend` | Renews the lease of a held reservation from now. It works while a settlement waits. |
| Sweeper | Marks held reservations past their lease `expired` and emits `reservation.expired`. A reservation whose settlement waits for Kill Bill never expires and is never abandoned. The settlement completes, or Kill Bill refuses it and the hold is held again. |

Leases run 10 seconds to 24 hours, 15 minutes by default. Amounts are 1 to 10 000 000 000 minor units. A reference is 1 to 200 letters, digits, dots, colons, dashes and underscores.

Over HTTP, a suspended tenant may still settle, release and extend its reservations, which only give back what it holds. It never reserves or grows one. See [tenants.md](tenants.md).

## Holders

A reservation belongs to its holder: over HTTP the API key or the membership that made it, in a library call the call's `Holder`, by default the actor's type and ID. Only the holder may settle, release, extend or grow it. Anyone else, the tenant's owner included, gets `404 reservation_not_found`. A replay of the key by another holder is `404` too.

`wallet:reserve` is a workload permission. No role but `owner` implies it. A team grants it, or a service API key carries it. So a product's scheduler holds credit with its own key, and no customer role can settle, release or break its holds.

Staff with the `support` grant of the brand may settle, release or extend another holder's reservation, with a reason, through the staff routes. Each such call is audited as `wallet.reservation.staff_override` with the holder. Staff never reserve.

## Settlement

A settlement is one of two things.

| Settlement | Kill Bill object | Reference stored | `settled_minor` |
| --- | --- | --- | --- |
| `usage`: a unit, a quantity in micro-units, and optionally a subscription and an event ID | A usage record on the subscription, `POST /1.0/kb/usages` | The tracking ID `zs-rsv-<digest>` | The catalogue price of the subscription's phase, as an estimate, with `rated_by: catalogue_price`. Kill Bill's invoice is the authority. |
| `credit_minor` | An external charge, `POST /1.0/kb/invoices/charges/{accountId}`, which Kill Bill pays from the account credit | The invoice item ID | The amount Kill Bill charged, with `rated_by: external_charge` |

A settlement goes through these steps:

1. Core resolves it, before any lock. A usage settlement goes to the subscription an earlier send of this reservation used, otherwise the requested one, otherwise the tenant's first active subscription whose plan prices the unit. The subscription must belong to the tenant, be active, price the unit and have started. Kill Bill 0.24.22 records usage of any unit, on any subscription, in any amount, so Core checks first. A usage settlement is priced with the first tier of the subscription's current phase. A unit that phase does not price cannot settle a reservation: `422 unit_unrated`. Resolving a usage settlement reads Kill Bill, so while Kill Bill cannot be read it is `503 billing_unavailable` and nothing changes.
2. Under the tenant lock, Core checks it against the hold. Its estimate, or its `credit_minor`, may not exceed the amount held: `409 settlement_exceeds_hold`, and nothing is written. When a meter maps to the unit, the usage event the settlement will record must be free to record: another event under its ID is `409 usage_event_conflict`, before Kill Bill is called.
3. Core writes the intent to the reservation: `settlement_pending`, the request, its fingerprint, the chosen subscription and the estimate. A retry reuses that subscription, so a retry of the tracking ID never reaches another subscription.
4. Core writes to Kill Bill.
5. Core completes the settlement under the tenant lock: it marks the reservation settled with the Kill Bill reference, which releases the rest of the hold, and moves the wallet version on. When a meter of the brand maps to the unit, the usage is recorded as a usage event in that same transaction, named after the reservation and already forwarded, so the forwarder never sends it twice.

The HTTP route and the sweeper commit the intent, release the lock, call Kill Bill and take the lock again to complete. `Reserve` is never blocked by a Kill Bill call there. A library `Settle` runs every step in the caller's transaction, so it holds the tenant lock through the Kill Bill call. A library caller that settles often uses `SettleNow` instead, which takes its own transactions.

A retry never charges twice. Kill Bill's keys come from the caller's keys only: the tracking ID and the charge description are digests of the brand, the tenant, the `external_ref`, the `attempt` and the kind of object, never of the reservation's own ID. Kill Bill keeps one record per tracking ID and subscription. A charge is found again by its description, `zs-core settlement <digest>`, before a new one is written. The same settlement again returns the reservation. Another settlement is `409 reservation_conflict`. A duplicate answer from Kill Bill counts only for the amount Core sent under the key before: a charge Kill Bill holds under the key with another amount, or a second send of another quantity after an earlier send whose outcome is unknown, is `409 settlement_conflict`.

When Kill Bill cannot take the settlement, the reservation stays held with `settlement_pending: true`. The HTTP route answers `202`. While it waits, the hold keeps counting and never expires. You cannot grow it, settle it differently or release it. Each of those is `409 reservation_settlement_pending`. You can extend it.

- The sweeper drives every pending settlement whose backoff ended: 10 seconds after the first attempt Kill Bill could not take, doubling up to an hour. One worker at a time holds a settlement, under a claim token, for two minutes. A worker whose claim ran out records nothing.
- `wallet.settlement.requested` wakes the settlement relay of `serve` the first time a settlement waits, so it completes at once when Kill Bill is back. It is an internal event type. See [foundation.md](foundation.md#events-and-outbox). While Kill Bill stays down, the relay defers the event without spending an attempt. The sweeper drives the settlement whatever becomes of the event, a quarantined one included.
- A settlement that still waits 24 hours after it was asked for is audited once as `wallet.settlement_stale`, gets a `settlement_stale` event in its history, and is reported by the reconciliation as `pending_settlement_stale` until it completes.
- A settlement whose answer Kill Bill lost waits the same way. The next attempt finds the charge by its description, or meets Kill Bill's refusal of the known tracking ID. It records the object Kill Bill made and makes no second one.
- A crash between the Kill Bill write and Core's commit leaves the intent. The sweeper recovers it the same way. A library caller whose transaction rolled back retries with the same ref and attempt, and recovers the same way.
- A settlement Kill Bill refuses is recorded as failed. The hold stays held until it is released or expires, and `reservation.settlement_failed` is emitted. No usage event is left behind: the event is written only when the settlement completes. A library `Settle` that Kill Bill refuses leaves nothing in the caller's transaction and returns the error.

## Reconciliation

The sweeper reconciles each tenant once an hour, and `zs-core wallet sweep` reconciles every tenant at once. A reconciliation reads the unbilled amount, then the credit, then the tenant's invoices from Kill Bill, and reports what Core's records do not explain:

| Finding | When |
| --- | --- |
| `unknown_credit` | A Kill Bill credit no Core credit explains, such as one added in KAUI |
| `missing_credit` | A Core credit that Kill Bill does not hold with its amount |
| `unknown_charge` | A settlement charge in Kill Bill that no settled reservation names |
| `missing_charge` | A settled reservation whose charge Kill Bill does not hold with its amount |
| `unknown_usage` | A tracking ID on an invoice that Core never sent, or usage Kill Bill holds for a subscription and unit in a month beyond what Core forwarded and settled, such as a record written outside Core |
| `missing_usage` | A tracking ID Core sent that the invoice of its period does not list, or usage of a month that Kill Bill does not hold |
| `held_mismatch` | A held reservation whose amount is not the hold its own history records, or whose history ended it, so the held total does not match the rows |
| `pending_settlement_stale` | A settlement waiting for Kill Bill for more than 24 hours, with when it was asked for and its attempts |
| `rejected_usage` | A usage rollup the forwarder refused to send, with its quantity, events and reason. See [metering.md](metering.md#forwarding). |
| `over_reserved` | Informational. Holds above the Kill Bill credit, less the usage part of the unbilled amount, the sum of the dry run's `USAGE` items, plus the overdraft allowance. Recurring charges are left out, so holds that only crowd out a coming renewal are not reported. |

Usage is compared two ways. Where Kill Bill exposes tracking IDs, on an invoice read with its components, each tracking ID is compared on its own. So a missing record and an unknown one of the same month are reported apart, even when their amounts cancel out. Kill Bill's usage reads carry no tracking ID, so usage not yet invoiced is compared per subscription, unit and month. That window runs from the month of the brand's backfill window less two months to the current month. It covers every subscription that is active, and every one cancelled or expired within the last three months.

Each finding is emitted once as `wallet.reconciliation_mismatch` and audited, with its kind, the Kill Bill object or reservation ID, and the amounts on both sides. Reporting a credit or charge finding moves the wallet version on. Core drops a finding once it no longer holds. If it returns, Core reports it again. The sweep never writes to Kill Bill. Purchases and charges that do not name a reservation are Kill Bill's business and are not findings.

## Routes

| Method and path | Permission |
| --- | --- |
| `GET /v1/tenants/{id}/wallet` | `billing:read` |
| `GET, POST /v1/tenants/{id}/reservations` | `wallet:reserve` |
| `POST /v1/tenants/{id}/reservations/{ref}/settle`, `.../release`, `.../extend` | `wallet:reserve`, and the reservation's holder. A suspended tenant may call them. |
| `POST /v1/staff/tenants/{id}/reservations/{ref}/settle`, `.../release`, `.../extend` | Staff `support` of the brand, with a reason. Anyone else gets `404`. |

Each write takes `attempt` in its body, 0 by default. `api/openapi.yaml` is the contract.

## Library use

```go
err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
	hold, _, err := w.Reserve(ctx, tx, sc, call, wallet.ReserveRequest{ExternalRef: jobID, AmountMinor: 500})
	if err != nil {
		return err
	}
	// The product's own admission runs in the same transaction.
	return admit(ctx, tx, jobID, hold.ID)
})
```

A scheduler reserves, settles and releases inside its own transaction. Every call reads through that transaction and never needs a second connection. Set `call.Holder` to the scheduler's own name, so the holds are its own.

A caller that rolls its transaction back after `Settle` returned must retry with the same `external_ref` and the same `attempt`, and the same settlement. Kill Bill already holds the charge or the usage record of the first try, under keys derived from those. The retry finds it and records it, and Kill Bill holds one. A retry under another attempt is another settlement, and Kill Bill holds two.

## Audit and events

| Action or event | When |
| --- | --- |
| `wallet.reservation.created`, `.grown`, `.extended`, `.released`, `.expired` | The reservation writes. Released and expired also emit `reservation.released` and `reservation.expired`. |
| `wallet.reservation.settled` | A settlement, with the Kill Bill subscription, tracking ID or charge ID. Emits `reservation.settled`. |
| `wallet.reservation.settlement_queued`, `wallet.reservation.settlement_failed` | A settlement that first waits for Kill Bill, and one Kill Bill refused. The second emits `reservation.settlement_failed`. |
| `wallet.settlement_stale` | A settlement still waiting 24 hours after it was asked for, once |
| `wallet.reservation.staff_override` | Staff settled, released or extended another holder's reservation, with the holder and the reason |
| `wallet.low_balance` | Audit and event, at most once a day per tenant |
| `wallet.reconciliation_mismatch` | Audit and event, once per finding |

## Known limits

- A usage rollup whose answer Kill Bill lost, and that the forwarder has not resent yet, is an `unknown_usage` finding until it is resent, when the month's totals are compared.
- Unbilled usage counts recurring charges due by the end of the period. A plan billed over a period longer than a month and a day shows its usage only when the period nears its end.
- `settled_minor` of a usage settlement uses the first tier of the phase's price, and so does the check against the hold. Tiered and blended prices are Kill Bill's to compute.
- A library caller that rolled back a usage settlement and retries finds Kill Bill's duplicate with no earlier send recorded in Core, so Core cannot compare the quantities. It records the retry. A different quantity shows as `missing_usage` or `unknown_usage`.
- The credit cache is per Core instance. A change to the Kill Bill credit that Core neither makes nor has yet detected, such as a credit added in KAUI, reaches a cached entry only when the entry ends or the reconciliation reports it.
- A low-balance threshold per tenant is not supported. The threshold is the brand's.
- Reconciliation reads every invoice of an account. It suits the v1 scale.
