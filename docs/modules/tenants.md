# Tenants

## TL;DR

A tenant is a personal or organization account inside one brand. The contract is in [identity-and-tenants.md](identity-and-tenants.md). This page explains how Core implements it. Each tenant has a Keycloak Organization and a Kill Bill account, both carrying the Core tenant ID. Core writes the tenant row only after both exist. If an engine fails, Core writes nothing, audits the failure and returns a retryable 503. A sweeper in `serve` finishes such creations later. A person acts in a tenant only through an active membership. Permissions are the union of the role's permissions and the member's team grants. Nobody grants a permission they do not hold. Every write checks the caller again under the tenant row lock. A removed membership stays removed, with its API keys revoked. Closing is final. The code is `pkg/tenant`.

## Kinds, roles and statuses

| Kind | Members | Created by |
| --- | --- | --- |
| `personal` | Its creator only. It takes no invitations: `409 tenant_kind_unsupported`. | `POST /v1/tenants/personal`, one per identity and brand. |
| `organization` | Anyone invited and accepted. | `POST /v1/tenants` with an `idempotency_key`. |

| Role | Implies |
| --- | --- |
| `owner` | Every registered permission. |
| `admin` | Every registered permission except `billing:*`, `tenant:transfer`, `wallet:reserve`, `usage:write` and `quota:consume`. |
| `billing_admin` | `billing:*` only. |
| `member` | Nothing. A member has what its teams grant. |

A tenant is `active`, `suspended` or `closed`. A membership is `active`, `suspended` or `removed`. Only an active membership in an active tenant writes. A suspended tenant keeps its reads. Closed is terminal.

A suspended tenant still makes the writes that only give back what it holds. It may settle, release and extend its reservations, and release its quota leases, so the work it already runs can finish and pay. It never reserves, grows a reservation, consumes a quota or records usage over HTTP. See [wallet.md](wallet.md#reservations) and [quotas.md](quotas.md#leases).

`wallet:reserve`, `usage:write` and `quota:consume` are workload permissions. No role but `owner` implies them. A team grants them, or a service API key carries them. A hold or a quota lease belongs to the membership or API key that made it, so a customer role cannot settle, release or break a product's holds.

A tenant name is 1 to 100 characters. Kill Bill stores the account name, which is the tenant name, in a varchar(100), and refuses a longer one. A longer name is `400 validation_failed` before Core calls an engine.

A removed membership is never reactivated. It stays in `memberships` as history. When the person accepts a new invitation, Core inserts a new membership with a new `mem_` ID. An identity has at most one membership in a tenant that is not removed.

## Permissions

The registered permissions of a brand are the built-ins plus the `permissions` list of its manifest, as last applied. The built-ins are listed in the contract. A permission is a `resource:action` string. A grant of anything else is `400 permission_unknown`.

Core computes effective permissions on every request:

1. Take what the role implies, out of the registered permissions.
2. Add every permission granted to a team the member belongs to.
3. Keep only the registered permissions. A permission the brand stops registering stops working.
4. A membership that is not active has none.

Every tenant has three system teams: `owners`, `admins` and `members`. They are created with the tenant and locked. Core keeps their members in step with the roles, in the same transaction as each role change. An owner is in `owners` and `members`, an admin in `admins` and `members`, and everyone else in `members`. A removed member leaves every team. The `members` team starts with `tenant:read`, `member:read` and `team:read`, so every member can see the tenant, its members and its teams. The `owners` and `admins` teams carry no grants of their own. Locked means Core manages the team's name and members. Renaming or deleting a locked team, or changing its members, returns `409 team_locked`. Its grants still change.

Examples, for a brand that registers `vm:create` and `billing:refund`:

- An owner holds all 21 permissions.
- An admin holds 14 of them: no `billing:read`, `billing:manage`, `billing:refund`, `tenant:transfer`, `wallet:reserve`, `usage:write` or `quota:consume`.
- A service API key the owner creates with `wallet:reserve` and `quota:consume` holds those two, and owns the holds and leases it makes.
- A billing admin holds `billing:read`, `billing:manage` and `billing:refund`, plus `tenant:read`, `member:read` and `team:read` from the `members` team.
- A member in a team that grants `vm:create` holds `vm:create`, `tenant:read`, `member:read` and `team:read`.

Nobody gives out more than they hold:

- Changing a member's role, suspending or removing them needs every permission both the current and the new role imply. An admin has no authority over an owner and cannot create a billing admin. Only an owner can make another owner.
- An invitation needs every permission its role implies. The role is never `owner`.
- Changing a team, its name, members or grants, needs `team:manage` and every permission the team grants before and after the change. An admin cannot add billing to a team, and cannot manage a team an owner gave billing to.
- An API key holds a subset of its creator's permissions. See [api-keys.md](api-keys.md).

Scope resolution builds the caller before the handler's transaction starts. A concurrent change can make that caller stale. So every write locks the tenant row first, then reads the caller's membership, role and effective permissions again in the same transaction, and runs every check on what it read. For an API key it also reads the key, which must still be unrevoked and unexpired. Every change to a membership, a team or a grant takes the same lock, so the caller cannot change until the write commits. A caller that is no longer active gets `404 tenant_not_found`. A caller that lost the permission gets `403 permission_denied`.

Example: owners A and B. A demotes B while B promotes member M to owner. If A's change commits first, B's change then reads B as an admin. An admin has no authority over an owner role, so M stays a member. Two owners who demote each other at the same time leave one owner the same way.

## Owners

The last active owner cannot be removed, suspended or demoted: `409 last_owner`. Each such change locks the tenant row first, so two concurrent changes cannot both pass the check. Ownership moves with `POST /v1/tenants/{id}/ownership/transfer`. The caller must be an active owner with `tenant:transfer` and multi-factor authentication within the last 15 minutes. The target must be an active admin. The target becomes an owner and the caller an admin, in one transaction.

Member changes, removal, transfer and API key creation need multi-factor authentication within the last 15 minutes: `403 recent_mfa_required`. Core reads it from `amr` and `auth_time` as described in [brands.md](brands.md#token-verification). An API key never has it.

## What a removal or a suspension takes with it

Removing a member does three more things in the same transaction:

- It revokes every API key the member created, with `revoked_reason` `creator_removed`. Each key is audited as `tenant.api_key.revoked`.
- It revokes every pending invitation the member sent, audited as `tenant.invitation.revoked` with reason `inviter_removed`.
- It takes the member out of every team.

Suspending a member revokes its API keys with `creator_suspended`, and its pending invitations with `inviter_suspended`. Reactivating the member brings neither back. A role change revokes the pending invitations the member can no longer send, with `inviter_demoted`. So does an ownership transfer for the old owner.

## Engine mapping

| Core | Keycloak | Kill Bill |
| --- | --- | --- |
| Tenant | An Organization in the brand realm. Its alias and its `zs_tenant_id` attribute hold the tenant ID, and `zs_brand_id` holds the brand ID. Its name is the tenant name. Keycloak requires unique names, so a second tenant with the same name gets `<name> (<tenant ID>)`. | An account in the brand's Kill Bill tenant. Its external key is the tenant ID. It takes the tenant name, the billing country and the brand's default currency, the first of `billing.currencies`. |
| Active membership | A member of the Organization. | Nothing. |
| Suspension | The Organization is disabled. | A blocking state `ZS_TENANT_SUSPENDED` of service `zs-core-tenant` blocks changes, entitlement and billing. Reactivation adds `ZS_TENANT_ACTIVE`, which blocks nothing. |
| Closure | The Organization is disabled and loses its members. | The account is closed: subscriptions cancelled, unpaid invoices written off, future notifications removed. |

The tenant slug comes from the name plus the last 8 characters of the tenant ID. Users never set it.

Brand apply enables Organizations in each realm, `identity.organizations_enabled`. Keycloak answers 404 to every Organizations endpoint until it is on. Keycloak groups and realm roles never create a Core membership. Core never reads Organization membership to authorize anything.

## Creating a tenant

Creation runs in steps, because one database transaction cannot cover both engines:

1. Core records the creation in `tenant_provisioning` with a new tenant ID and claims it for two minutes.
2. It finds the Keycloak Organization by its `zs_tenant_id` attribute, or creates it, and records its ID. It adds the creator as a member.
3. It finds the Kill Bill account by its external key, or creates it, and records its ID.
4. In one transaction it inserts the tenant, the system teams and the owner membership, binds the billing recipient to that owner, queues reconciliation, audits `tenant.created`, and marks the creation completed.

Step 3 sends the tenant name as the account name. Kill Bill refuses names over 100 characters, so Core checks that limit first.

When step 2 or 3 fails, no tenant row exists. Core releases the claim, schedules the next attempt, audits `tenant.created` with outcome `failure` and the stage, and answers `503 identity_provider_unavailable` or `503 billing_unavailable`. Each later attempt reuses the upstream objects already recorded, so an engine never gets a second Organization or account for one tenant.

`tenant_provisioning` is also the idempotency record of `POST /v1/tenants`. The key is scoped to the brand and the creator. The same key and payload return the same tenant, also after the sweeper completed it. A different payload is `409 idempotency_key_conflict`. While a request or the sweeper holds the creation, a retry gets `409 idempotency_key_in_progress`. Completed records are kept. A personal tenant uses the same record without a key, unique per identity.

`POST /v1/tenants/personal` never answers `409 idempotency_key_in_progress`. When another request holds the creation, it looks again every 100 milliseconds for up to 5 seconds. It returns the tenant once it exists, or takes over the creation once the claim is released. After 5 seconds it answers a retryable `503 not_ready`. The personal tenant takes the caller's display name, cut to 100 characters on a character boundary, or `Personal`.

## The sweeper

`serve` runs the sweeper every `ZS_TENANT_SWEEP_INTERVAL`, one minute by default, when the Keycloak and Kill Bill admin settings are present. `zs-core tenant sweep` runs one pass by hand. One pass runs at a time across every Core instance, through a database advisory lock. A pass does three things:

- It claims due creations and runs the steps above again. A failed attempt waits 10 seconds, then twice as long each time, up to an hour.
- It reads every tenant of every applied brand and corrects its Organization towards Core. The name follows the tenant. The Organization is enabled only while the tenant is active. Its members become exactly the identities with an active membership, or none once the tenant is closed.

- It compares every account's primary billing email with current Core membership, including accounts previously marked ready. Failed recipient checks wait five seconds, then twice as long up to five minutes. Ready accounts stay eligible for the next periodic check, so legacy accounts, changed last-verified email and external drift are repaired. A Keycloak failure does not skip the recipient check.

Each Organization correction is audited as a system action: `tenant.keycloak.organization.updated`, `tenant.keycloak.member.added` and `tenant.keycloak.member.removed`.

## The Organization mirror

The sweeper corrects Organizations and billing recipients once a minute. It reads each tenant again under its own capability before it syncs the Organization, so a tenant suspended or closed since the page was read is synced with the status it has now. Known limit: the sync of Organizations and recipients walks every tenant of every applied brand on each pass, with no cursor and no budget, so a pass takes as long as the fleet is large, and the closure pass runs after it. A cursor and a budget for that sync are a follow-up. Changes reach the engines sooner through the mirror. Tenant-name updates and invitation acceptances queue `tenant.mirror` in the handler transaction. The tenant library itself queues that event for role/status changes, removals, ownership transfers and completed provisioning, including direct library calls. The event commits with Core state and contains no recipient address. No goroutine starts per write.

`serve` runs 4 mirror relays next to the sweeper, when the engines are configured. Each claims up to 10 `tenant.mirror` events at a time and handles them one by one, so at most 4 Organization updates are in flight per Core instance. Each independently attempts `SyncOrganization` and `ReconcileBillingRecipient`, with separate bounded timeouts, so a Keycloak error does not suppress recipient repair. The relays claim only `tenant.mirror` events. A failed update is retried with the outbox backoff and quarantined after 10 attempts. The sweeper corrects whatever the mirror misses. Library membership changes still commit their pending recipient state and event without reachable engines. Delivery waits for configured engines. A recipient deferred by its backoff keeps its event unpublished. The periodic check remains the repair path even after an event is quarantined.

## Billing recipient projection

Core membership selects the managed primary email on the tenant's Kill Bill account. An eligible recipient is an active owner in the same brand and tenant with a nonempty last-verified identity email. An ownership transfer binds its named new owner. Otherwise an eligible bound owner stays selected. If that binding is ineligible, Core selects the oldest eligible owner by membership `created_at`, then ID. With none, the desired primary email is empty. `created_by_identity_id` remains immutable provenance.

Role/status changes, removals and transfers commit Core membership first. Kill Bill availability is never an access-reduction precondition, and reconciliation failure cannot turn that committed API success into an apparent rollback. After commit the mirror reads current eligibility, reads the exact account under the brand's credentials, compares and sends a primary-email PUT only on difference. It checks account ID and external key before writing and verifies readback before recording success. The tenant lock covers only short Core selection/status transactions, never network requests.

Migration `000019_tenant_billing_recipients.up.sql` stores a scoped membership binding, status, last successful check time, retry attempts, due time and safe error code. It stores no address. Existing tenants start pending and the sweep repairs their accounts.

Tenant responses expose `billing_recipient_status` without an email address: `pending` means scheduled work or an upstream check/write failure, `ready` means the scoped account matched an eligible owner at its last successful check, and `no_eligible_owner` means Core observed a clear primary email with no eligible owner. These are projection observations, not mail-delivery receipts. `zs-core doctor` reads current eligibility and actual account email, reports drift or missing eligibility, and never repairs either system. `zs-core tenant sweep` performs repair.

Duplicate or delayed requests can temporarily restore an older address. A later pass repairs it from current membership. There is no instantaneous revocation, cancellation of accepted upstream requests or recall of sent, queued or already materialized mail. The projection manages only primary email. Kill Bill's additional account emails remain unmanaged and can be CC recipients. Doctor reads that collection under the scoped account credentials and warns with the account ID whenever any exist, without printing an address or deleting entries. An unavailable or invalid additional-email response fails inspection; it never establishes an empty collection. The reconciler writes primary email only. The pinned SMTP path rejects blank primary To before sending, rather than taking the listener's null-primary skip branch. Its SES path bypasses that SMTP validation, so clearing primary email is not proof of universal no-send behavior. Actual plugin/Mailpit acceptance must distinguish newly generated mail after convergence from earlier queued mail.

Tenant closure keeps its existing behavior. It does not remove memberships or define a new recipient/final-invoice policy. Whether closure should generate a final invoice remains an open owner decision outside this change.

## Invitations

An owner or admin with `member:invite` invites an email address with a non-owner role. The clear token is 32 random bytes in base64url. It is in the creation response only. Core stores its SHA-256 digest and compares digests in constant time. An invitation stays valid for 7 days.

`POST /v1/invitations/accept` needs three things. A customer token of the same brand. A verified email equal to the invited address, compared without case. An authentication within the last 15 minutes, read from `auth_time`. A different address is `403 invitation_email_mismatch`. An old authentication is `403 recent_auth_required`. An expired, revoked, accepted or unknown invitation, or one of another brand, is `404 invitation_not_found`. So is one of a tenant that is no longer active. A database failure while Core locks the tenant is a retryable `503 database_unavailable`, never `404`.

An invitation is only as good as its inviter's authority when it is accepted. Under the tenant lock, Core reads the inviter's membership. It must be active, hold `member:invite`, and hold every permission the invited role implies. Otherwise Core revokes the invitation, audits `tenant.invitation.refused_stale_inviter` and answers `404 invitation_not_found`. The route commits that record before it answers. A library caller gets the error wrapped in `tenant.RefusalRecorded` and commits its transaction to keep it.

Two acceptances of one invitation at the same time add one membership. The second finds the invitation accepted and gets `404 invitation_not_found`.

A removed member who accepts a new invitation gets a new membership with a new ID and the new role. The old membership stays removed, and its API keys stay revoked.

## Activation

Activation is a decision staff make about a tenant: `pending`, `approved` or `rejected`. `POST /v1/staff/tenants/{id}/activation` needs a staff token with an active `activation` grant in the tenant's brand. Anyone else gets `404`. Each decision needs a reason and a `request_id` of 8 to 255 printable characters. The same request ID with the same state and reason returns the stored decision with `200`. With a different state or reason it is `409 idempotency_key_conflict`. Decisions are append-only: a trigger refuses `UPDATE`, `DELETE` and `TRUNCATE` on `tenant_activations`. The current state is copied to the tenant in the same transaction.

`GET /v1/tenants/{id}/activation` returns the state and the latest 50 decisions to a member with `tenant:read`. Core records activation and takes no action on it. Products decide what an unapproved tenant is allowed to do.

## Suspension and closure

No HTTP route suspends, reactivates or closes a tenant in v1, except the staff closure route below. An operator runs:

```bash
zs-core tenant suspend --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
zs-core tenant reactivate --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
zs-core tenant close --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you>
zs-core tenant start-closure --brand zs-reference --tenant tnt_... --reason '<why>' --operator <you> [--request-id <8 to 255 characters>]
```

`tenant start-closure` is the way to end a customer: it starts the closure settlement, so the final usage is invoiced and the unused prepaid credit returned before the account is blocked. `start-closure` needs no engine settings, because it only writes Core. `tenant close` is the immediate hard close and must not be used once a closure exists, as described below.

Each command changes Kill Bill first, then the Keycloak Organization, then Core, and audits `tenant.suspended`, `tenant.reactivated` or `tenant.closed`. When an engine fails, nothing changes in Core and the command can run again. Library callers use `tenant.Suspend`, `tenant.Reactivate` and `tenant.Close`.

`tenant close` is the immediate hard close: it closes the Kill Bill account at once, cancels its subscriptions and writes off its unpaid invoices, so there is nothing left to invoice. Do not use it once a closure exists, or to end a customer who has final billing: it is refused as `tenant_closing` for a closing tenant, and as `tenant_busy` while another command or a closure start holds the tenant. Each of `suspend`, `reactivate` and `close` holds the lifecycle capability of the tenant from its first read to its last write, engine calls included, and answers `tenant_busy` at once when something else holds it.

Closing is terminal. It revokes every API key of the tenant with `tenant_closed`, in the same transaction. Each command on a closed tenant answers `409 tenant_closed`. Core checks again under the tenant lock before it writes. A reactivation that called the engines while a close committed is refused there with `tenant_closed`, the tenant stays closed, and Core disables the Organization again. If that last call fails, the sweeper disables it.

A closing tenant that holds usage accepted before dispatch protocol 1 needs a manual review before closure can complete. That usage is read-only: the forwarder never rolls it up, sends it or marks it billed, and closure never discards, resends or bills it either. `RecordClosureReviews` records a required review item for each kind the tenant holds, once, and audits it as `tenant.closure_review_required`:

| Kind | Reason shown to staff | Held when |
| --- | --- | --- |
| `legacy_usage_events` | `legacy_usage_unbilled` | The tenant has raw events of protocol 0 that are in no rollup. |
| `legacy_usage_rollups` | `legacy_rollup_unsent` | The tenant has rollups of protocol 0 that are still pending or were rejected. |

Staff resolve an item with `ResolveClosureReview`, a note of 1 to 500 characters and their identity, audited as `tenant.closure_review_resolved`. `ListClosureReviews` returns the items with who resolved each and the note. The database refuses to move the closure settlement to `completed` while the tenant holds either kind of usage and no resolved item of that kind exists, so the rule holds even when no item was ever recorded. A review covers the count it recorded: usage that appears afterwards needs another item, which `RecordClosureReviews` records for the larger count. An item for a smaller count that a larger item has superseded stays listed as open, but it no longer gates completion: only a resolved item that covers the count held now does. Resolving an item records the review. It does not change the usage.

A closing tenant's closure invoice is recorded as an outcome and never assumed. `no_invoice` means Kill Bill's dry run for the account and target date found nothing left to invoice at that moment. Invoices that Kill Bill already generated are evidenced at settlement, not by this outcome.

`RunClosureInvoice` runs the closure invoice of one closing tenant under the tenant's lifecycle capability. In order, it records the review items, commits the invoice intent and executes the invoice. It never completes the closure. Every database step is a short transaction, so no transaction and no row lock is open while Kill Bill is called: only the capability's advisory lock is held. The capability treats any statement error as its end, so a database failure ends the pass. The pass returns the error and the next pass resumes, because every outcome is durable and the invoice is never posted twice. The result says `Busy` when another owner holds the tenant, in which case nothing was called, and `ManualReview` when a review item is unresolved or the invoice outcome is unknown or did not match. The pass that records an unknown invoice outcome reports `ManualReview` at once and returns no error, so a worker does not retry for nothing. A caller that loops on the error must back off, because an error is a database or Kill Bill failure and a loop that does not wait only adds load to it.

`StartClosure` starts a closure for a tenant that is active or suspended. In one transaction under the lock of the tenant row it moves the tenant to `closing`, creates the settlement at `cancelling` with its cutoff at the database clock, and writes the audit entry `tenant.closure_started` with the reason, which is required, and the request ID, which is 8 to 255 printable characters. The settlement keeps the actor, the reason and the request ID, and a database rule keeps them from changing. The same request ID with the same reason is a replay, the same request ID with another reason is `idempotency_key_conflict`, and another request ID on a closure that has started gets the closure that exists. It takes no step against Kill Bill or Keycloak: those are the steps of the settlement, and the sweeper takes the tenant from there and disables its Organization on its next pass. A closure that has started is left as it is and a retry or a second caller gets `Started: false` with the same cutoff. A closed tenant is refused as `tenant_closed`. The start, `Suspend`, `Reactivate` and `Close` serialise on the lifecycle capability of the tenant: each holds it from its read to its last write, engine calls included, so a command that is running makes a start answer `tenant_busy`, and a start or another command makes a command answer `tenant_busy` without calling any engine. Without that, a closure could start while `Close` was calling Kill Bill, and the account would be closed, its subscriptions cancelled and its unpaid invoices written off, with nothing left for the closure to invoice. Staff start a closure over HTTP with `POST /v1/staff/tenants/{id}/closure`, which needs a staff token with an active `closure` grant in the tenant's brand and answers `404` to anyone else. The body is `{reason, request_id}`. The answer is `201` with the closure `{state, cutoff_at, reason, request_id}` when it started, and `200` with the closure that exists on a replay or another request ID. The same request ID with another reason is `409 idempotency_key_conflict`, a closed tenant is `409 tenant_closed`, and a tenant that another change is working on is `409 tenant_busy`, which is retryable and carries a `Retry-After` header. The request ID is scoped to the tenant: the same ID on two tenants is two requests. The actor is audited as staff. A request that finds the closure already started under another request ID is ignored and audited as `tenant.closure_start_ignored`, with its reason and request ID. A closing tenant answers nothing in the API, and `Suspend`, `Reactivate` and `Close` refuse it with `409 tenant_closing`, which is not retryable,, including one that was already in flight when the closure started: its closure is the only thing that ends it.


A closure settlement moves through four states in order: `cancelling`, `finance_pending`, `blocking`, `completed`. `AdvanceClosureSettlement` moves it one state at a time and audits each move as `tenant.closure_state_changed`. A move that is already done is a no-op, and a move that would skip a state is refused. A database trigger refuses to skip or go back as well, and checks what each state needs:

| Move to | Needs |
| --- | --- |
| `finance_pending` | The cancellation and the usage drain done, and a finance target date. |
| `blocking` | A settled invoice: evidenced, or nothing to invoice. An unknown outcome, an unfinished attempt and no attempt all wait for a person. |
| `completed` | The finance step recorded, the account marked blocked with `MarkClosureBlocked`, a settled invoice, and the review guard: no unresolved legacy usage. |

`EngineClosureSteps` are the real `ClosureSteps`, against Kill Bill. `Prepare` first asks the `ClosureDrain` whether the usage of the tenant is all with Kill Bill, and does nothing else until it says so. The usage goes first so that it is in the repair invoice that the cancel produces, and does not depend on a later invoice run: Kill Bill accepts and bills usage on a cancelled subscription, as measured on 0.24.22, so this is for the invoice and not because it would refuse. A closing tenant takes no new usage, so none arrives after the drain. Without a drain nothing is cancelled and the closure waits. Before the drain, if the account's billing is blocked by Core, as a suspension does, `Prepare` writes the closed state, which leaves billing open, and reads it back, so the final invoice is not computed under a billing block. That does not make the suspended period billable: usage recorded while the tenant was suspended is unbilled whether billing is opened or not, so a closure never bills it; it writes nothing for an account whose billing is open. Once the usage is drained, `Prepare` reads the bundles of the account and, for every subscription that does not satisfy the receipt `killbill.CancelledOn`, cancels it with the IMMEDIATE policy: the base subscriptions first, which take their add-ons with them, then any add-on that is left. It reads the account again and reports the cancellation done only when every subscription is `CANCELLED` with a day that is not after today, and an account with no subscriptions is done at once. A cancel is never repeated blindly: one whose answer is lost is settled by the read that follows, and one that did not take effect is returned as an error and is made by the next call, which reads first. A subscription that cannot be cancelled again, such as one cancelled for a day after today, one that Kill Bill says is gone, or one whose cancel Kill Bill refused because of its state and that is still not `CANCELLED` after the read, is a case for a person and fails with `conflict`, never a silent wait. The finance target date is today. Usage recorded on the same UTC day as the creation and the cancellation of a subscription falls in a window of zero length and is never billed.

The drain is `metering.Service.Drained`. It rolls up the tenant's events and sends every due rollup to Kill Bill, and it answers true only when all of these hold: no event accepted under dispatch protocol 1 is in no rollup; no rollup of protocol 1 is pending, claimed, started or in flight, and none is rejected, so an unconfirmed send, which stays pending until staff resolve it against Kill Bill, keeps the tenant undrained; and no wallet reservation is held, each being settled or released. Usage accepted before protocol 1 is read-only, is never rolled up, and does not hold the drain: the closure review takes it to a person and completion waits for that. A tenant that is closing is refused raw usage, as a closed one is, so the closure invoices what was accepted before it, while the settlement of a reservation held before it is still taken. A tenant whose usage cannot be drained waits at `cancelling`, and is looked at again every five minutes. `serve` gives the sweeper these steps only when Kill Bill billing is configured, because the drain needs the usage forwarder.

A closure settlement is never deleted: a database rule of migration 34 refuses the delete in every session replication role.

The tables `tenant_closure_bundle_holds` and `closure_hold_resolution` of migration 23 are reserved and unused. The closure never holds a bundle: it cancels every subscription of the tenant after the drain, and anything it cannot cancel cleanly, a cancel that Kill Bill refused with the subscription still not cancelled, a subscription that is gone, or one cancelled for a day after today, becomes a manual-review `conflict` instead. Nothing writes the two tables, and they are kept so that a structure is not removed without a decision to.

`zs-core doctor` lists what a closure leaves for a person, as warnings, because it is work waiting and not a stack that differs from its release, and it reads and changes nothing. `closure_stuck` names a tenant whose closure has not finished after an hour, with where it is, how long it has been closing, its attempts and the name of its last error. `closure_reviews` counts the unresolved closure review items of a tenant, which staff resolve before the closure can complete. `usage_rejected` counts the rollups that were rejected, which Kill Bill will not bill: staff reassign or discard them with the staff rollup routes, and a closing tenant is not drained until they do. `usage_unconfirmed` counts the usage rollups of a tenant whose send Kill Bill never confirmed, with the reason code: staff resolve them against Kill Bill, and the tenant is not drained until they do. `usage_claims_stranded` counts rollups claimed by a forwarder that has gone for more than 15 minutes, which the usage forwarder resolves on its next pass. With nothing to report it prints one `closures` line that says so. A finding names a tenant, a state and a code, never usage or an address. If the database cannot be read the check fails.

`Block` sets the blocking state `ZS_TENANT_CLOSED` on the account, which blocks changes and entitlements and leaves billing alone, reads it back as the latest state of the service, and writes nothing when it is already in force. The reason is in the paragraph that follows. A write whose answer is lost is settled by the read after it, and a write that did not take effect is returned as an error for the next call to make.

The final state of the Kill Bill account of a tenant that closes through a closure is blocked, by design, and never closed. The `Block` step sets a blocking state that stops new entitlements and changes and leaves billing alone, and nothing in the closure calls `CloseAccount`. Closing an account writes off its unpaid invoices, which would forfeit the final closure invoice and any balance still owed, so the invoices and the payments of a closed tenant stay collectible. Only the hard `tenant close`, for a tenant without a closure, closes the account.

Completing the settlement closes the tenant in the same transaction: the status becomes `closed` with its time, `tenant.closed` is audited with the actor of the completion and `via: closure`, and every API key of the tenant is revoked with `tenant_closed`, as a hard close does. A repeat of the completion on a closed tenant changes nothing and is not an error. Two deferred database rules, checked at commit, keep the two together (migration 32): a settlement that is `completed` needs a tenant that is `closed`, and a tenant leaves `closing` only by closing, and with a completed settlement. A tenant that has a settlement is closed only with a completed one, and a hard `tenant close` of a tenant that has a settlement is refused with `tenant_closing` before it calls Kill Bill. A tenant with no settlement can still be hard closed. The harness turns these two rules off on its databases, as it does the start rule, so fixtures can insert a settlement at a later state, and `postgrestest.EnableClosureInvariants` turns them on for a test of the rules.

`MarkClosureBlocked` records that the account is blocked, audited as `tenant.closure_blocked`, and leaves the settlement in `blocking`.

The tenant sweeper drives closing tenants too. Each pass reads a page of closing tenants that are due from a durable cursor, `sweep_cursor` of kind `closure`, and drives each under its own lifecycle capability, up to `ClosureWork` tenants (50 by default) and `ClosureScan` looked at per pass (500 by default). A tenant whose capability another owner holds is skipped, and the cursor moves past every tenant looked at and wraps when none is left, so each is reached in turn. A tenant is driven one state at a time as far as its conditions allow: the steps before finance, the invoice, the move to blocking, and, once the account is blocked, completion. The steps the sweep cannot do itself, the cancellation and the usage drain before finance and the account block after it, come from the `ClosureSteps` the sweeper is given. Without them a tenant waits at those steps. Both steps must be idempotent: a pass can be stopped or fail at any point, and the next pass calls the same step again, so a step reads what is already done and does only what is missing. The sweep writes the schedule of a tenant, its recheck or backoff, with a short context of its own, so a pass that ends right then does not lose it. A tenant that waits for a step or for a person is looked at again after five minutes, and a failure backs off from ten seconds to an hour and records only the name of the error. The report counts closing tenants checked, advanced, waiting, in manual review, busy and failed. The pass has a budget of `PassBudget` (30 seconds by default) and each closure step call one of `CallBudget` (5 seconds by default). A step that overruns its call budget fails that tenant with `step_timeout` and the pass goes on. A tenant that uses up the pass budget pays for it with a backoff, and the cursor moves past it, so a stuck tenant cannot keep the others from being reached. A pass whose budget runs out ends without an error, at the next tenant boundary or as soon as the step in hand returns. The call budget bounds the cancellation and the account block, not the invoice step: its reads end with the pass context, and its post to Kill Bill runs under a bound of its own, 25 seconds, so a pass can run past its budget by that long. A pass stopped by its caller, as on shutdown, records nothing against the tenant it was in and leaves the cursor there, so after the restart that tenant is looked at first.

The review and order rules are also functions the code asks before it writes, so a refusal is an answer and not a statement error: a statement error ends a lifecycle capability, and with it the sweep pass that holds it. A closure settlement starts at `cancelling`. A recorded step, the cancellation, the drain, the finance step, the block and the finance invoice, is never changed or cleared, and a completed settlement is frozen whole.

A closure settlement starts empty: at `cancelling`, with no step recorded, no finance data, no error and no attempt. The database refuses an insert that claims anything else, so nothing can start a settlement that is already partly done. The start, order, completion and cutoff rules fire in every session replication role, and disabling one needs ownership of the table, which the application role lacks. Only the code that starts a closure inserts settlements, and a test fails if any other non-test Go file or migration does. Because these rules fire in every replication role, a restore or a logical replication apply into the table meets them too: such a load must insert settlements in the state they started in, or an owner of the table disables the rules for the load and enables them again after it.

## Scope resolution

Every `/v1/tenants/{id}/...` route passes through the same step before its handler. The tenant must belong to the caller's brand, and must not be closed. A write needs it active. A customer token needs an active membership of its identity. A staff or service token has none. An API key must belong to the tenant. Anything else is `404 tenant_not_found`, so a tenant's existence is never confirmed to an outsider. A database failure in this step is `503 authentication_unavailable`. The handler then calls `scope.Require(ctx, "resource:action")`, and a missing permission is `403 permission_denied`.

## Audit actions

| Action | When |
| --- | --- |
| `tenant.created` | A tenant was created, or a creation attempt failed (outcome `failure`). |
| `tenant.updated` | Name or billing country changed. |
| `tenant.member.added`, `tenant.member.updated`, `tenant.member.removed` | Membership changes. |
| `tenant.ownership.transferred` | An ownership transfer. |
| `tenant.team.created`, `tenant.team.renamed`, `tenant.team.deleted`, `tenant.team.members.set`, `tenant.team.grants.set` | Team changes. |
| `tenant.invitation.created`, `tenant.invitation.revoked` | Invitations. Acceptance is `tenant.member.added`. A revocation Core makes itself carries the reason `inviter_removed`, `inviter_suspended` or `inviter_demoted`. |
| `tenant.invitation.refused_stale_inviter` | An acceptance refused because the inviter lost the authority. The invitation is revoked. |
| `tenant.activation.decided` | An activation decision, with its reason. |
| `tenant.suspended`, `tenant.reactivated`, `tenant.closed` | Status changes, with their reason. |
| `tenant.api_key.created`, `tenant.api_key.revoked` | API keys. A revocation Core makes itself carries the reason `creator_removed`, `creator_suspended` or `tenant_closed`. See [api-keys.md](api-keys.md). |
| `tenant.keycloak.organization.updated`, `tenant.keycloak.member.added`, `tenant.keycloak.member.removed` | Sweeper and mirror corrections. |
| `tenant.billing_recipient.reconciled` | An observed recipient projection status change. Contains status, never an address. |

Members with `audit:read` read them with `GET /v1/tenants/{id}/audit/events`. Staff with `operator` read them with `GET /v1/audit/events?tenant_id=...`. See [audit.md](audit.md).

## Known limits

- The sweeper reads every tenant on each pass and calls Keycloak twice per tenant. That suits the v1 scale. A change feed is later work.
- A creation that keeps failing retries every hour, with no limit. Its last error is in `tenant_provisioning.last_error` and in the audit trail.
- Closing a tenant does not delete its Keycloak Organization or its Kill Bill account. Core never deletes upstream objects.
- Keycloak 26.8.1 puts no `auth_time` and no `amr` in password grant tokens. Such a token never passes the recent authentication or recent MFA checks.
