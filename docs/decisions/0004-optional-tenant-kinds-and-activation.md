# 0004. Tenant kinds and activation are optional per brand

Status: proposed (draft for review).

## TL;DR

Core today offers two tenant kinds, personal and organization, and an activation decision (pending, approved, rejected) on every tenant. Both came from the first product's onboarding. A second product may sell only to organizations, may have no review step, or may want only personal accounts. This note proposes a `tenancy` section in the brand manifest that names the allowed kinds and says whether activation is used. The defaults keep today's behaviour, so existing brands change nothing. Core keeps recording activation and still takes no action on it. The change is small, additive and needs one migration only if a brand later turns activation off after decisions exist. No product rule enters Core: the manifest chooses, and Core enforces only the choice.

## Context

`pkg/tenant` defines `KindPersonal` and `KindOrganization` and the three activation states. Every brand gets both kinds and both activation routes. A personal tenant takes no invitations (`409 tenant_kind_unsupported`). Activation is a staff decision recorded with a reason, and Core takes no action on it; products decide what an unapproved tenant may do (`docs/modules/tenants.md`, "Activation"). The OpenAPI contract lists both kinds and all three states as fixed enums.

A business that adopts Core as a platform needs to choose. A business-to-business product has no use for personal accounts. A self-service product with no manual review has no use for activation, and a permanently `pending` state would mislead its staff.

## Proposal

Add an optional `tenancy` block to the brand manifest:

```yaml
tenancy:
  kinds: [personal, organization]   # default: both
  activation: recorded              # recorded (default) | none
```

| Field | Values | Effect |
| --- | --- | --- |
| `kinds` | A non-empty subset of `personal`, `organization` | A request to create a tenant of a kind that is not listed answers `409 tenant_kind_unsupported`. Existing tenants of a removed kind keep working. Apply refuses to remove a kind that has no way to be retired, unless a flag says it is intentional. |
| `activation` | `recorded` or `none` | With `recorded`, nothing changes. With `none`, new tenants are created with activation `approved` by the system actor, the staff activation route answers `404 activation_not_enabled`, and no `activation` grant is needed. The tenant response still carries the field so clients keep one shape. |

Rules:

- The default of an absent block is `kinds: [personal, organization]` and `activation: recorded`. This is today's behaviour.
- The block is validated with the rest of the manifest by `brand apply`, and a change shows in the apply diff.
- Switching a brand from `recorded` to `none` does not rewrite history. Tenants in `pending` or `rejected` stay as they are until staff decide them or an operator runs a one-time command that approves them with a reason. Core never approves anyone silently.
- Switching from `none` to `recorded` marks nothing new. Tenants stay `approved`, and later tenants start `pending`.
- Core still takes no action on activation. This change does not make Core enforce an approval rule.

## What changes

- `pkg/brand`: manifest schema and validation for the new block.
- `pkg/tenant`: read the brand's `tenancy` at tenant creation and in the activation route. No change to tables.
- `api/openapi.yaml`: add the `activation_not_enabled` error and describe both fields' conditions. The enums stay.
- `docs/modules/tenants.md`, `docs/modules/brands.md`: document the block and the defaults.
- Tests: a brand with only `organization` refuses a personal tenant; a brand with `activation: none` creates approved tenants and refuses the staff route; a brand with the defaults is unchanged; changing the block between creations behaves as the rules above say.

## Alternatives

- **Remove personal tenants and activation from Core.** This breaks the first product and any brand that uses them. Rejected.
- **A feature flag in the environment.** It applies to all brands on one server, and Core serves many brands. Rejected.
- **Leave it and let products ignore the fields.** Staff would see meaningless states, and the personal route would remain open. Rejected.

## Consequences

- A product chooses its tenancy model in its manifest, with no code change in Core.
- The first product keeps its current model by default.
- One more manifest block to document, validate and test.
- Core stays free of product rules: it enforces the choice and never the reason for it.

## Open questions

- Should `kinds` be extensible later (for example a `partner` kind)? This note keeps the set closed because the contract enums are closed.
- Is an operator command for approving old pending tenants worth building, or is a staff loop enough? Decide when the first brand turns activation off.
