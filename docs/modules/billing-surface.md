# Billing surface on Kill Bill

## TL;DR

Kill Bill is the billing engine and Core adds no ledger. Each brand is a Kill Bill tenant with its own catalogue, overdue policy, payment plugin, invoice plugin and templates, all declared in the brand manifest and applied by Core. Each Core tenant is a Kill Bill account keyed by the Core tenant ID. Core's `/v1` billing routes wrap the Kill Bill API and scope every call to one brand and one tenant. They cover subscriptions, usage, credits, promo codes, invoices, payments, payment methods and entitlement checks. Products that need more call Kill Bill directly with brand-scoped credentials from Core. This is the Phase 4 contract.

## Mapping

| Core concept | Kill Bill object | Notes |
| --- | --- | --- |
| Brand | Tenant (`apiKey`, `apiSecret`) | Secret held in the secret store, referenced by the brand row; Core signs every Kill Bill call with it |
| Tenant | Account (`externalKey` = Core tenant ID) | Currency from the brand default or the tenant's billing country; `billCycleDayLocal` from brand policy |
| Catalogue | Catalogue XML per tenant, versioned by `effectiveDate` | Products, plans, phases (trial, discount, evergreen), prices per currency, usage units, change plan policies |
| Entitlement and plan | Subscription (base, add-on, standalone) | `CheckEntitlement(tenant, feature)` reads active subscriptions and catalogue product features |
| Usage | Usage records on a consumable subscription unit | Core validates and forwards; caller `event_id` becomes the usage `trackingId` for idempotency |
| Credits | Account credits and invoice credit items | Promo and voucher codes are Core tables that resolve to a credit with reason and expiry; expiry is a Core sweeper that issues a negative credit |
| Invoices | Invoices | Read-only through Core; PDF through the invoice template plugin when the brand bundles one |
| Payments and methods | Payments, payment methods on the brand's payment plugin | Plugin chosen per brand: `killbill-stripe-plugin` (verified), `killbill-adyen-plugin` (pinned), `killbill-paypal-express-plugin` (pending a Java plugin), or a custom plugin built with the extension guide |
| Tax | Invoice plugin | Optional per brand; the plugin computes tax items on invoice generation |
| Dunning | Overdue configuration per tenant | Overdue states map to Core tenant billing standing and to notifications |
| Multiple legal sellers | One Kill Bill tenant per seller | Two brands can share a product name and never a tenant |

## Brand billing manifest

```yaml
billing:
  killbill_tenant_key: zcp-prod
  catalogue: billing/catalogue.xml
  overdue: billing/overdue.xml
  currencies: [CAD, USD]
  seller:
    name: ZSoftly Technologies Inc.
    country: CA
  payment_plugin:
    name: stripe
    version: 8.0.3
    config_ref: secret://zcp-prod/stripe-plugin
  invoice_plugin:
    name: null
  templates: billing/templates   # EN and FR invoice and email templates
```

`zs-core brand apply` installs the named plugin version through KPM if absent. It uploads the per-tenant plugin configuration from the reference. It uploads the catalogue when its digest changed (Kill Bill keeps the version history), then the overdue policy and the templates. It records every upload in audit. A catalogue that fails Kill Bill validation stops the apply before anything else changes.

## Core routes

All tenant routes require membership; reads need `billing:read`, writes need `billing:manage` unless noted.

| Method and path | Purpose |
| --- | --- |
| `GET /v1/brands/{id}/catalogue` | current catalogue summary: products, plans, phases, prices, usage units |
| `GET /v1/tenants/{id}/billing` | account summary: currency, balance, credits, overdue state, payment plugin name |
| `GET, POST /v1/tenants/{id}/subscriptions`; `POST .../subscriptions/{sub}/change`; `DELETE .../subscriptions/{sub}` | create with `plan`, `idempotency_key`; change plan with policy `immediate` or `end_of_term`; cancel |
| `GET /v1/tenants/{id}/entitlements/{feature}` | `{allowed, limit, used, remaining, reason, plan}`; `tenant:read` is enough |
| `POST /v1/tenants/{id}/usage` and `/usage/batch` | `{subscription, unit, quantity_micro, event_id, occurred_at, dimensions}`; `usage:write`; forwarded as usage records with `trackingId` |
| `GET /v1/tenants/{id}/usage/events`, `/usage/summary`, `/usage/billed` | Core's event list, Core's summary, and Kill Bill's billed usage for the period |
| Wallet, redeem and staff credit routes | `/wallet`, `/wallet/redeem` and `/staff/.../wallet/credits`, described in [wallet-and-metering.md](wallet-and-metering.md) |
| `GET /v1/tenants/{id}/invoices`; `GET .../invoices/{inv}`; `GET .../invoices/{inv}/pdf` | invoice list, detail, PDF when a template plugin is present |
| `GET /v1/tenants/{id}/payments`; `POST .../payments/{pay}/refund` | staff `billing` for refunds |
| `GET, POST, DELETE /v1/tenants/{id}/payment-methods`; `PUT .../payment-methods/{pm}/default` | shaped over the brand's plugin; `POST` returns the plugin's client-side token or redirect data, never raw card data |
| `POST /v1/billing/webhooks/killbill` | Kill Bill push notification endpoint for this deployment; verified by shared secret and source, deduplicated, turned into Core events |
| `GET /v1/tenants/{id}/billing/credentials` | `billing:manage`: a short-lived, brand-scoped Kill Bill credential for products that call Kill Bill directly |

## Rules

- Core never stores card data and never logs payment method references, plugin properties or Kill Bill secrets.
- Every Core write to Kill Bill carries `X-Killbill-CreatedBy` set to the Core principal, `X-Killbill-Reason` from the request, and `X-Killbill-Comment` with the correlation ID, so Kill Bill's own audit matches Core's.
- Kill Bill unreachable returns `503 billing_unavailable`; a Kill Bill validation error returns `422 billing_rejected` with the Kill Bill error code in `message`.
- Promo and voucher codes: table `credit_codes` (`brand_id`, code digest, amount, currency, max redemptions, per-tenant limit, expiry, reason). Redemption is idempotent per tenant and code, and writes one Kill Bill credit with the code as the reason.
- Credit expiry: a sweeper finds Core-issued credits past their expiry with unused balance and writes a negative credit for the remainder, audited; Kill Bill itself does not expire credits.
- `CheckEntitlement` fails closed: no active subscription, a blocked account, or Kill Bill unreachable all return `allowed: false` with a reason.
- Plans, prices, trials, upgrade and downgrade rules live in the catalogue XML, not in Core code. Core validates the file against the Kill Bill schema before upload.
- Holds before work (reserve, then settle) come from the reservation extension in [wallet-and-metering.md](wallet-and-metering.md). Reservations are holds against Kill Bill credit. They are not a customer balance.

## Tests that gate Phase 4

- Reference stack with two brands: brand A on the Stripe plugin (sandbox), brand B on the Adyen plugin (sandbox), or the external payment plugin when no second sandbox is available; a subscription, a usage batch, an invoice and a payment round trip on each, and brand A's credentials cannot read brand B's account.
- Catalogue apply with a digest change creates a new catalogue version; an invalid catalogue stops the apply with no side effects.
- Usage replay with the same `event_id` is accepted once; a different payload returns `409`.
- Promo code redemption twice by the same tenant returns the first result; a second tenant can redeem until the cap.
- Credit expiry sweeper reverses only the unused part.
- Kill Bill down: every billing route returns `503 billing_unavailable`; `CheckEntitlement` returns `allowed: false`.
- Suspending a tenant blocks the Kill Bill account; reactivation unblocks it; both audited on both sides.
