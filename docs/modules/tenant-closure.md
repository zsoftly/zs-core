# Closing a tenant: final billing, then block

## TL;DR

A tenant is closed in two parts. `StartClosure` moves it to `closing` and creates a closure settlement. The tenant sweeper then drives the settlement through four states: `cancelling`, `finance_pending`, `blocking` and `completed`. In `cancelling` the sweep opens billing if a suspension blocked it, sends the tenant's usage to Kill Bill, cancels every subscription immediately and reads the account back. In `finance_pending` it invoices what is still unbilled once, and never again if the outcome is unknown. In `blocking` it sets a blocking state on the Kill Bill account, and it never closes the account, because closing writes off unpaid invoices. Completion closes the tenant in the same transaction that completes the settlement. Anything Core cannot prove stops the closure at a named point and waits for staff: it never guesses, never retries an unknown outcome on its own, and never weakens a database rule. Every step is idempotent, audited and checked by the database as well as by the code.

## Who starts it and what it does

Staff start a closure with `POST /v1/staff/tenants/{id}/closure` and the `closure` grant, or an operator runs `zs-core tenant start-closure`. The request carries a reason and a request ID, which is scoped to the tenant. In one transaction under the tenant's lifecycle capability the start inserts the settlement at `cancelling`, sets the tenant to `closing`, and audits `tenant.closure_started`. The settlement keeps the actor, the reason, the request ID and the cutoff, and a database rule keeps them from changing.

The same request ID with the same reason is a replay. The same request ID with another reason is `409 idempotency_key_conflict`. Another request ID on a closing tenant gets the closure that exists, and is audited as `tenant.closure_start_ignored`. A tenant that another change is working on answers `409 tenant_busy` with `Retry-After`. A closed tenant answers `409 tenant_closed`.

A closing tenant answers nothing to its own members and API keys, which see `404 tenant_not_found`, and it takes no new raw usage. `Suspend`, `Reactivate` and the hard `tenant close` refuse it with `409 tenant_closing`. The hard close is for a tenant without a closure: it closes the Kill Bill account at once and skips the settlement.

## States and steps

| State | What the sweep does | It moves on when |
| --- | --- | --- |
| `cancelling` | Opens billing if the account is blocked by Core. Drains the usage. Cancels every subscription. Reads the account back. | The usage is drained and every subscription is `CANCELLED` with a day that is not after today. |
| `finance_pending` | Reads the tenant's live Kill Bill tenant, asks Kill Bill for a dry-run invoice, and triggers one invoice if there is something to bill. | The invoice is evidenced by a read, or the dry run says there is nothing to invoice. |
| `blocking` | Sets the blocking state `ZS_TENANT_CLOSED` on the account and reads it back. | The state is the latest of the service, and the closure review gate is clear. |
| `completed` | Closes the tenant, revokes its API keys and audits `tenant.closed`, in the same transaction. | Nothing: the settlement and the tenant are final. |

A step that waits is looked at again after five minutes. A step that fails backs off from ten seconds to an hour and records only the name of the error. A pass has a budget of 30 seconds and each cancel or block step 5 seconds, so a stuck tenant cannot keep the others from being reached. The sweep takes a tenant under its lifecycle capability, so a closure and a lifecycle command never run on one tenant together.

## The steps in detail

**Open billing.** A suspension blocks billing. Before anything else the sweep writes `ZS_TENANT_CLOSED`, which leaves billing open, and reads it back, so the final invoice is not computed under a billing block. This does not make the suspended period billable. Usage dated within a suspension is unbilled either way.

**Drain.** `metering.Service.Drained` rolls up the tenant's events and sends every due rollup to Kill Bill. It answers true when no protocol 1 event is unrolled, no protocol 1 rollup is pending, claimed or rejected, and no reservation is held. A send that started and whose outcome Kill Bill never confirmed does not hold the drain, because nothing more will ever be sent for it and no invoice shows its tracking ID before the cancel bills it. It gates completion instead. A claim that never started a send does hold the drain. Usage accepted before dispatch protocol 1 is read-only and never rolled up, and goes to the closure review.

**Cancel.** The drain goes first so that the usage is in the repair invoice that the cancel produces. The sweep cancels each subscription that does not satisfy `CancelledOn`, base subscriptions first, which take their add-ons with them. A cancel is never repeated blindly. A lost answer is settled by the read that follows. A cancel that did not take effect is an error, and the next pass reads first. A cancel Kill Bill refused because of the subscription's state, a subscription that is gone, and one cancelled for a day after today all end as a `conflict` that needs a person.

**Finance.** `RunClosureInvoice` records the invoice intent once, in an immutable attempt, and checks the live Kill Bill tenant against the one stored for the brand. A dry run that answers nothing to invoice is the outcome `no_invoice`. Otherwise it triggers one invoice and reads the account to find it. A trigger whose outcome is unknown is `post_unknown` and is never posted again: it goes to manual review.

**Block.** `ZS_TENANT_CLOSED` blocks changes and entitlements and leaves billing open, so the final invoice and any balance owed stay collectible. The final Kill Bill state of a closed tenant is blocked by design and never closed.

**Complete.** The completion goes through the review gate, then closes the tenant. Two deferred database rules keep the tenant and the settlement together at commit.

## Review gates and manual paths

Completion is refused, by the application and by the database, until all of these hold.

- Legacy usage events and rollups, accepted before protocol 1, have a closure review item that staff resolved with a note.
- Every usage send that started and was never confirmed has a staff resolution.
- The closure invoice outcome is settled, and any `post_unknown` or `read_mismatch` outcome has been dealt with by staff.

Staff resolve an unconfirmed send with `POST /v1/staff/tenants/{id}/usage/rollups/{rollup}/resolve` (the `support` grant) or `zs-core usage resolve-rollup`, with one of two decisions. `forwarded` needs a committed invoice that carries the tracking ID, which shows that Kill Bill billed the record and not its quantity. `not_billed` says Kill Bill has not billed the record and will not. It needs the settlement past its finance step and the cancel's own invoice, a committed invoice dated on or after the cancellation with a repair or usage item of the subscription of the rollup, or a finance step that found nothing to invoice, none of the committed invoices carrying the tracking ID. It records the lost quantity for finance. Neither sends anything. A rejected rollup is reassigned or discarded with the existing staff routes.

`zs-core doctor` lists what waits for a person: `closure_stuck`, `closure_reviews`, `usage_unconfirmed`, `usage_rejected` and `usage_claims_stranded`. They are warnings, and a list longer than 100 says how many more there are.

## What the database enforces

| Rule | Where |
| --- | --- |
| A settlement starts at `cancelling` with no evidence recorded. | Start trigger, migrations 28 and 29. |
| A settlement moves one state at a time and never back. | Order trigger, migrations 27 and 28. |
| Completion needs the review gate to be clear. | Completion trigger, migrations 26 and 28, and the gate function of migration 37. |
| A settlement is never deleted and its start record never changes. | Migrations 30 and 34. |
| A tenant is `closing` only with a settlement, `closed` is terminal, and a settlement is `completed` only with a closed tenant. | Deferred constraint triggers, migrations 32 and 33. |
| A staff resolution of a rollup is never changed, and it needs the right state and evidence shape. | Migrations 35, 37 and 38. |

The rules fire in every session replication role. A restore or a logical replication load into these tables meets them too. The harness of the tests turns the start rule and the tenant rules off on its databases so that fixtures can insert a settlement at a later state, and each test of the rules turns them on.

## Kill Bill facts measured on 0.24.22

- An invoice trigger with nothing to invoice answers a bare HTML 404 page. A dry run answers 204.
- A cancel with the IMMEDIATE policy answers 204. A repeat answers 500 with code 1031. Cancelling a base subscription cancels its add-ons.
- A subscription of another Kill Bill tenant answers 500 to a cancel.
- A cancel by requested date needs `useRequestedDateForBilling=true`.
- A tracking ID appears on an invoice only once the usage is billed, at the period end or at the cancel.
- Kill Bill accepts and bills usage on a cancelled subscription on the next invoice run.
- Usage dated on the day a subscription was both created and cancelled falls in a window of zero length and is never billed, measured on an account in the default timezone.
- A blocking state that leaves billing open keeps the final invoice collectible.

## Known limits and follow-ups

- Sending a rollup once more, with evidence that Kill Bill does not hold it, is not offered. It needs a change to the guard on the rollup table.
- The sync of Organizations and recipients walks every tenant on each pass with no cursor or budget, and the closure pass runs after it.
- The tables `tenant_closure_bundle_holds` and `closure_hold_resolution` are reserved and unused. A decision to drop them is open.
- A real-stack test of the closure on Kill Bill is written by the integrations owner.
- A `not_billed` decision relies on an invoice dated on or after the cancellation day, in UTC, and Kill Bill makes the invoice of a cancel a little after the cancel, which is why it also needs the finance step to be done. An account in another timezone can differ by a day.
- The closure never calls the Kill Bill account close, so the account of a closed tenant stays blocked for ever.

## Where the code is

`pkg/tenant/closure_start.go`, `closure_sweep.go`, `closure_engine.go`, `closure_run.go`, `closure_state.go`, `closure_review.go`, `lifecycle.go` and `lifecycle_guard.go`. `pkg/metering/closure_drain.go` and `rollup_resolution.go`. `internal/doctor/closures.go`. The migrations are 22 to 38, and 35, 37 and 38 are the ones for the rollup resolutions.
