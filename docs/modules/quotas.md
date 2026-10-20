# Quotas and entitlements

## TL;DR

Kill Bill decides which plans a tenant is on. Core decides how much of a feature each plan allows. A brand declares quotas in its manifest under `entitlements`: a feature, a plan or `*` for every plan, a limit, and a window, none, day, month or concurrent. An entitlement check reads the tenant's active Kill Bill subscriptions, picks the quota, counts the use and answers `{allowed, limit, used, remaining, reason, plan}`. It fails closed. Staff with the `support` grant set a limit for one tenant. A day or month limit counts what callers consume in the window, raised to the meter's recorded usage when that is more. A concurrent limit counts what callers consume and release, such as running jobs, as leases that expire unless they are extended. Every consumption runs under a lock per tenant and feature, and a lease belongs to the membership or API key that took it. Core never stores a price or a plan. The code is `pkg/quota`.

## What Core stores

| Table | Holds |
| --- | --- |
| `quotas` | Per brand: feature, plan or `*`, limit, window, and for a day or month window the meter it counts |
| `quota_overrides` | Append-only limits staff set for one tenant's feature, with a reason, an optional window and meter, and an optional expiry. A `null` limit clears the ones before it for good. |
| `quota_counters` | Per tenant, feature and window start. A concurrent counter is the sum of the tenant's leases. A day or month counter is what was consumed in the window, raised to the usage summaries when they count more. |
| `quota_leases` | What each caller reference holds of a quota, the holder that took it, and when it expires |

## Declaring quotas

```yaml
entitlements:
  - feature: api-call
    plan: core-metered-monthly
    limit: 100000
    window: month
    meter: api-calls
  - feature: concurrent-jobs
    plan: "*"
    limit: 2
    window: concurrent
```

A plan must be a plan of the brand's catalogue or `*`. A day or month window needs a meter of the brand. A concurrent window takes none. A none window names a meter if it needs one. It then counts that meter's usage since the first event. Limits run 0 to 10<sup>15</sup>. A limit of 0 refuses the feature on that plan.

With `entitlements` present, even empty, apply makes the brand's quotas exactly that list, and audits `brand.quotas.updated` with the diff. With it absent, staff `operator` manage them with `PUT /v1/brands/{id}/quotas`, which follows the same rules and refuses a brand whose manifest declares them with `409 conflict`.

## The check

`CheckEntitlement(ctx, scope, feature)` and `GET /v1/tenants/{id}/entitlements/{feature}`, with `tenant:read`, replace the Phase 4a check. The steps are:

1. Read the tenant's standing, its active Kill Bill subscriptions and the catalogue in force, as package billing does. A closed tenant, a hold that stops entitlement, no active subscription or an unreachable Kill Bill is `allowed: false` with `tenant_not_found`, `wallet_on_hold`, `no_active_subscription` or `billing_unavailable`.
2. Pick the quota: the largest limit among the quotas for the tenant's active plans, otherwise the quota for every plan. A staff override in force replaces its limit. See [Overrides](#overrides).
3. Grant the feature when a quota applies, or when an active plan's product has it, as its name or a usage unit of its plans. Otherwise `feature_not_in_plan`. An override caps a feature. It grants nothing.
4. Count the use. A day or month window takes the larger of its counter and the meter's usage summaries of the current UTC day or month, rounded up to whole units. When the summaries count more, the counter is raised to them and the drift is audited as `quota.counter.drift`, at most once a day per tenant and feature. A concurrent window reads its counter. A none window without a meter counts nothing.
5. Answer. Without a quota, `limit`, `used` and `remaining` are null and the feature is allowed. With one, `allowed` is false with `quota_exceeded` once `used` reaches `limit`. `window` names the quota's window.

Every check reads Kill Bill. Nothing about a subscription is cached, so a hold or a cancellation takes effect on the next check.

## Consuming

`Consume(ctx, tx, scope, call, feature, ref, n)`, `ConsumeFor`, which also takes the lease, and `POST /v1/tenants/{id}/entitlements/{feature}/consume`, with `quota:consume`, take `n` for a caller reference, such as a job ID. `n` is 1 by default. `quota:consume` is a workload permission no role but owner implies. Under the tenant-feature lock they check the entitlement, then:

| Window | Consumption |
| --- | --- |
| `concurrent` | A lease of `n` that expires after `lease_seconds`, one hour by default, 10 seconds to 24 hours |
| `day`, `month` | `n` against the window's counter, which admission trusts. The counter is first raised to the meter's usage summaries when they count more. |
| `none` | Refused: `409 quota_not_concurrent` |

Past the limit is `409 quota_exceeded`. The answer is the entitlement after the consumption, so `allowed: false` there means the last unit was just taken. A refused entitlement fails closed with `503 billing_unavailable`, `409 wallet_on_hold`, `404 tenant_not_found` or `409 quota_exceeded`. The same reference again, from the same holder and in the same window, takes nothing more. Another amount under it is `409 conflict`.

Ten consumers at once never pass a limit together: each takes the lock, reads the counter and adds to it before the next one reads.

## Leases

A lease belongs to the holder that took it: over HTTP the API key or the membership, in a library call the call's `Holder`, by default the actor's type and ID. Another holder, the owner included, gets `404 not_found` when it consumes, releases or extends under the same reference.

| Call | Rule |
| --- | --- |
| `Release`, `POST .../release` | Returns what the reference holds of a concurrent quota. A reference that holds nothing returns 0. A day or month consumption is never given back: releasing it returns 0. Release never calls Kill Bill, so it always goes through, even while Kill Bill is down. A suspended tenant may release. |
| `Extend`, `POST .../extend` | Renews a concurrent lease to `lease_seconds` from now. Audited as `quota.lease.extended`. |
| Lease sweeper | Every `ZS_QUOTA_SWEEP_INTERVAL`, releases each concurrent lease past its expiry and audits it as `quota.lease.expired`. A day or month consumption is dropped when its window is over, without an audit. |
| `POST /v1/staff/tenants/{id}/entitlements/{feature}/release` | Staff with the `support` grant release another holder's lease, with a reason. Audited as `quota.lease.staff_override`. |

## Overrides

`POST /v1/staff/tenants/{id}/quota-overrides` needs a staff token with the `support` grant in the tenant's brand. Anyone else gets `404`. The body names the feature, the limit or `null`, a reason, which is required, an optional window and meter, and an optional expiry within 10 years. Each one is audited as `quota.override.set` and gets a `qta_` ID.

- The latest override in force wins over the plan's limit.
- An override without a window takes the window of the plan's quota. It needs a quota of the brand that covers the feature: otherwise `400 validation_failed`.
- An override that names its window applies only while the plan's quota has that window, or while no quota covers the feature. A day or month override names an active meter of the brand.
- A `null` limit clears every override before it for good. An override made before the latest clearing one never applies again, even once the clearing one expires. A clearing override names no window.

## Audit

| Action | When |
| --- | --- |
| `quota.consumed`, `quota.released` | A consumption or a release, with the feature, the reference, the amount and the window |
| `quota.lease.extended`, `quota.lease.expired`, `quota.lease.replaced` | A lease renewed, released by the sweeper, or replaced by a consumption of the same reference in a later window |
| `quota.lease.staff_override` | Staff released another holder's lease, with the holder and the reason |
| `quota.counter.drift` | The usage summaries counted more than a day or month counter, which was raised to them |
| `quota.override.set` | A staff override, with its reason |
| `brand.quotas.updated` | A change of the brand's quotas, by apply or by the route |

## Known limits

- Usage counts against a day or month limit once it is recorded. The counter follows it on the next check or consumption. Kill Bill's billed usage may trail it.
- A product that consumes a day or month quota and also records the usage counts each unit once: the counter takes the larger of the two.
- A none window without a meter returns its limit for the product to compare with its own count.
- Every check reads Kill Bill four times, for the overdue state, the blocking states, the account bundles and the catalogue. If you check at a high rate, check once per admission and hold a concurrent quota for the job's duration.
