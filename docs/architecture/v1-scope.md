# ZSoftly Core v1 scope

## TL;DR

ZSoftly Core v1 is the managed distribution of Keycloak and Kill Bill plus the extension framework and the thin API that make them one platform for every ZSoftly product. Core pins one version of each component, ships their deployment, brands and secures them per product, and upgrades them on one schedule. Keycloak remains the identity provider and Kill Bill remains the billing engine. Core forks neither and keeps no ledger. A product is a brand: one Keycloak realm, one Kill Bill tenant and one manifest. The manifest declares theme, locales, authentication policy, catalogue, payment plugin and templates. Core applies the manifest and verifies tokens. It maps tenants and memberships, issues API keys, aggregates audit and relays signed webhooks. One REST API gives brand-scoped access to both engines. The build is six phases, each with an OpenAPI contract, a configuration schema, isolation tests and an upgrade path, and the first consumers are the ZCP Core API and the CI runner control plane.

## What Core is and is not

| Core is | Core is not |
| --- | --- |
| The pinned, packaged, upgraded distribution of Keycloak and Kill Bill | A fork of either |
| The extension framework: Keycloak SPIs and themes, Kill Bill plugins, brand manifests | A second billing engine or identity service |
| The per-brand configuration layer: themes, locales, MFA policy, catalogue, payment plugin, templates | The place where a product's own business rules live |
| The unified API: brands, tenants, memberships, API keys, staff grants, audit, webhooks, brand-scoped passthrough | A proxy that hides the upstream APIs; products call them directly with Core-issued, brand-scoped credentials |
| The ZSoftly extensions Kill Bill lacks: wallet view, reservations, metering, quotas and entitlements, branding automation, notifications | A competing customer balance or payment ledger; those stay in Kill Bill |
| The release and upgrade cadence for all of it | A hosted service; every product self-hosts a Core release |

## Where this comes from

Two codebases each carry their own identity and billing glue:

| Capability | ZCP Core API (`zcp-cmp-core/zcp/api`) | CI runner control plane |
| --- | --- | --- |
| Identity | Keycloak JWT, `(issuer, subject)`, verified email required | Keycloak JWT with `azp` and broker checks, `(issuer, subject)` |
| Tenancy | personal and organization accounts, three roles | tenants, three roles, system teams, `resource:action` permissions |
| API keys | none | digest only, expiry, capped at creator permissions |
| Billing | Stripe customer, invoices, payment methods, credit notes | Stripe and Creem, prepaid credits, reservations, orders |
| Notifications | none | email, SMS, Slack, Teams, signed webhooks with outbox |
| Audit | activation audit table | activity log, staff shadowing audit |

Core takes the identity glue, the tenancy and API key rules, the audit and webhook conventions, and the reusable Go libraries (`pkg/errors`, `pkg/idempotency`, `pkg/events`, `pkg/idgen`, outbox, Postgres test harness). Billing logic is not ported: it moves into Kill Bill's catalogue, subscriptions, usage and credits, with the payment plugin each product chooses. Nothing ZCP-specific (CloudStack mapping, domains, DNS) and nothing runner-specific (scheduling, runner catalogue, source control integration) comes in.

## Approved architecture

```
             ZSoftly Platform Core
                      |
                Unified APIs
                      |
         +------------+------------+
         |                         |
      Keycloak                  Kill Bill
      Identity                  Billing
         |                         |
      Themes                 Payment plugins
      SSO / MFA              Stripe / PayPal / Creem
      Users / Roles          Invoices / Subscriptions
      Email templates        Usage / Payments
         |                         |
         +------------+------------+
                      |
             ZSoftly Extensions
           Wallets / Reservations
           Metering / Entitlements
           Branding / Notifications
                      |
       Compute / Storage / Networking / DNS
     App Hosting / Kubernetes / Databases / CI
```

Four rules apply:

1. Pin and deploy the official releases and never fork them.
2. Kill Bill stays the financial authority. No extension creates a competing balance or payment ledger.
3. Each product configures its own payment plugins per Kill Bill tenant.
4. Everything is white-label and configured as code.

## Decisions

- 0001: one Go module, importable packages and a service binary. Still applies to the Core API layer.
- 0002: Core packages and extends Keycloak and Kill Bill and owns no ledger. Products choose their Kill Bill payment plugin.
- 0003: never fork upstream; extend through supported extension points; pinned versions in `components.yaml`.

## How the eight capabilities map onto the components

| Capability | Keycloak | Kill Bill | Core adds |
| --- | --- | --- | --- |
| Organization and tenant management | realm per brand, users, Organizations, groups, roles, invitations | tenant per brand, account per Core tenant | tenant and membership mapping, `resource:action` permissions, last-owner guard, activation decisions |
| Prepaid wallet and credits | | account credits, invoice credit items, overdue state | wallet view (Kill Bill credit minus holds), reservations before work with settle and release, promo and voucher codes, expiry sweeper, low-balance events |
| Usage metering and pricing | | usage API, consumable units, catalogue prices, rollup | usage ingestion with caller event IDs, dimension validation, summaries, deterministic forwarding into Kill Bill usage records |
| Service catalogue and entitlements | | catalogue XML, products, plans, phases, trials, change plan, entitlement API | catalogue upload from the manifest, quotas per plan and feature with overrides and counters, `CheckEntitlement` and `Consume` for admission |
| Payment and tax management | | payment plugins (Stripe verified, Adyen pinned, PayPal pending a Java plugin, others), invoice plugins for tax, multi-currency, one tenant per legal seller | plugin catalogue, per-brand plugin install and configuration, credential storage |
| Notifications and communication | email templates, events | email-notifications plugin, per-tenant templates, push notifications | EN and FR template sets per brand, signed outbound webhooks, outbox and retries |
| Audit and security | event SPI, authentication flows, MFA, service account clients | audit logs | event listener SPI forwarding to Core audit, API keys, staff grants, MFA policy per brand, sensitive-action approvals |
| Developer platform | Admin API | REST API, push notifications | unified OpenAPI, TypeScript SDK, Go client, brand-scoped passthrough credentials, Terraform and MCP later |

## Brand manifest

A brand is one file, `brands/<slug>.yaml`, validated against `api/schemas/brand-manifest.json`. It declares identity (realm settings, clients, brokered providers, authentication policy, theme, locales), billing (Kill Bill tenant, catalogue file, payment plugin and its configuration reference, invoice and tax plugin, templates, currencies, legal seller), notifications (sender identity, template set, webhook endpoints), and permissions (the `resource:action` list the product uses). `zs-core brand apply brands/<slug>.yaml` creates or converges the realm, the tenant, the plugins and the templates, and records the result in audit. Secrets are references to a secret store or environment variables, never values.

## Repository layout

| Path | Contents |
| --- | --- |
| `components.yaml` | pinned component versions and images for this Core release |
| `deploy/compose/` | reference stack for development and tests: Keycloak, Kill Bill, KAUI, Postgres |
| `deploy/ansible/` and `deploy/helm/` | environment deployment, Phase 2 onwards |
| `extensions/keycloak/` | themes and SPI extensions (event listener, required actions), built as Keycloak providers |
| `extensions/killbill/` | Core plugins (event relay, brand templates) and the approved third-party plugin catalogue with pinned versions |
| `cmd/zs-core` | the Core API and operator commands (`serve`, `migrate`, `brand apply`, `grant add`, `doctor`, `upgrade check`) |
| `pkg/` | importable packages: brand, identity, tenant, apikey, audit, events, outbox, errors, idempotency, idgen, scope |
| `internal/` | config, database, httpapi, Keycloak and Kill Bill clients |
| `api/openapi.yaml` | the `/v1` contract, source of truth for generated types and the SDK |
| `docs/` | architecture, decisions, modules, upgrades, development |

## Conventions

- Identity is `(issuer, subject)`. Email is an attribute and never a key.
- Errors are `{"error": {"code": "snake_case", "message": "...", "retryable": bool}}`. A dependency failure returns `503` with a `*_unavailable` code and never leaks an upstream message.
- Idempotency is a body field `idempotency_key`, 8 to 255 characters, scoped to brand, tenant and actor; mismatch returns `409 idempotency_key_conflict`.
- Pagination is cursor based: `limit` and `cursor` in, `next_cursor` out.
- IDs are UUIDv7 with a type prefix. Upstream IDs (Keycloak user ID, Kill Bill account ID) are stored alongside and never exposed as Core IDs.
- Money and quantities are integers in minor or micro units, with an explicit currency or unit.
- Every write appends an audit event with actor, brand, tenant, action, reason, outcome and correlation ID.
- Outbound webhooks are signed `X-ZS-Signature: t=<unix>,v1=<hmac-sha256 of "t.body">`, 5-minute tolerance, private addresses blocked, delivered from an outbox with retries.
- `Cache-Control: no-store` on every response. No secrets, keys or payment references in logs.
- OpenAPI 3.1 is the source of truth; Go types and the TypeScript SDK are generated from it and CI fails when stale.
- Stack: Go 1.26, standard library `net/http`, `pgx/v5`, `slog`, OpenTelemetry, environment-only configuration validated at start, embedded forward-only migrations, integration tests on isolated Postgres schemas and the reference compose stack.

## Phases

| Phase | Delivers | Gate |
| --- | --- | --- |
| 1 Foundation | Core API skeleton: config, migrations, Postgres test harness, error envelope, idempotency, IDs, audit, outbox, OpenAPI skeleton, `/healthz`, `/readyz`, `make check` | `make check` green; brand and tenant isolation proven on the audit route |
| 2 Distribution | `components.yaml`, `deploy/compose` reference stack with pinned images, `zs-core doctor` (versions, connectivity, migrations), `docs/upgrades/` runbooks for Keycloak and Kill Bill, release process | stack up from one command; doctor green; a Keycloak and a Kill Bill upgrade rehearsed on the stack and written down |
| 3 Brands and identity | brand manifest schema, `brand apply` converging a Keycloak realm (clients, brokers, theme, locales, MFA flow) and a Kill Bill tenant (API key and secret stored encrypted), token verification, tenants and memberships mapped to Keycloak Organizations and Kill Bill accounts, teams and permissions, invitations, activation, staff grants, API keys | two brands on one stack cannot see each other; ZCP account routes reproduced; runner API key rules reproduced |
| 4 Billing surface and extensions | catalogue upload per brand, subscriptions, invoices, payments through the brand's chosen plugin, overdue, plugin catalogue and per-brand plugin install; the ZSoftly extensions: wallet view, reservations, usage metering forwarded to Kill Bill, quotas and entitlements, promo codes (`docs/modules/billing-surface.md`, `docs/modules/wallet-and-metering.md`) | Stripe sandbox round trip for one brand and Adyen sandbox for another on the same stack, or the external payment plugin when no second sandbox is available; reservation, settle and reconciliation tests green |
| 5 Extensions | Keycloak event listener SPI to Core audit, Kill Bill push notifications to Core signed webhooks, EN and FR template sets through the email-notifications plugin, brand theme build, the extension guide (payment plugin, invoice or tax plugin, Keycloak authenticator) | a third-party plugin added by following the guide alone; signed webhook verified by the reference client |
| 6 Developer surface | OpenAPI complete, TypeScript SDK, Go client, brand-scoped passthrough credentials, module docs, adoption guides for the two first consumers | spec hash in CI; both products have a written adoption plan |

Each phase lands as one pull request with signed-off commits, reviewed before the next starts.

## What the first consumers hand over

ZCP Core API: Keycloak verification, personal and organization accounts, memberships, invitations, activation with audit, staff grants, and the Stripe customer, invoice, payment method and credit note code, which is replaced by Kill Bill with the Stripe plugin. It keeps CloudStack tenant reconciliation, native access revocation and domains, consuming Core activation and entitlement events for them.

CI runner control plane: tenants, memberships, system teams, permissions, invitations, API keys, orders and refunds, Stripe and Creem checkout, webhook signing and templates. It keeps scheduling, admission, the runner catalogue, source control integration and AI features. Its credit reservation before a job moves to Core's reservation extension, called in library mode inside its scheduler transaction. Creem has no Kill Bill plugin today. Writing one through the extension guide is the first test of the framework. Its production billing stays as it is until the Core path is fully tested in its staging.

Both repositories require an explicit owner decision before a split. Merging this document records that decision for Core. Each product records its own adoption decision in its own repository.
