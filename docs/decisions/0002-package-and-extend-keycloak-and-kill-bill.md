# 0002. Core packages and extends Keycloak and Kill Bill; it owns no ledger of its own

## TL;DR

Core is the managed distribution of Keycloak and Kill Bill plus the extension framework around them. It pins one version of each and upgrades them on one schedule. It never forks either. Kill Bill is the financial authority, and Core keeps no customer balance or payment ledger. Core builds what Kill Bill lacks as extensions with their own transactional records. Each product chooses its payment plugin per brand. Existing product billing stays untouched until the Core replacement is tested.

Status: accepted, 2026-10-20. Supersedes the earlier draft, which proposed a Core-owned wallet with Stripe and Creem adapters.

## Context

Every ZSoftly product needs identity and billing, and each one has been running its own copy: its own Keycloak realm wiring, its own Stripe code, its own invoices and credit logic. Keeping those aligned on versions, security fixes, branding and upgrade timing is the real cost. Keycloak and Kill Bill already implement identity and billing well. What is missing is one place that packages them, configures them per brand, extends them where they stop, and upgrades them on one schedule.

## Decision

ZSoftly Core is the managed distribution of Keycloak and Kill Bill plus the extension framework around them.

- Core pins one version of each component per Core release, ships the deployment for them, and owns their upgrade and security patch cadence. See `components.yaml`.
- Core never forks either upstream. Everything Core adds goes through the supported extension points: Keycloak SPIs and themes, Kill Bill plugins and per-tenant configuration. See decision 0003.
- Kill Bill is the financial authority: accounts, catalogue, subscriptions, usage, invoices, credits, payments, overdue. Core keeps no customer balance and no payment ledger of its own.
- What Kill Bill does not provide, Core builds as ZSoftly extensions with their own transactional records. These are the prepaid wallet view, reservations before work, usage metering with caller event IDs, quotas and entitlements, branding automation and notifications. Every amount that changes what a customer owes or owns goes to Kill Bill. The extension records reference the Kill Bill object they produced. See `docs/modules/wallet-and-metering.md`.
- A product chooses how Kill Bill takes money. Core carries a catalogue of approved payment plugins (Stripe verified, Adyen pinned, PayPal pending a Java plugin written through the extension guide) and the framework to add others. The choice, credentials and configuration are per brand, declared in the brand manifest and applied by Core.
- Keycloak is the identity provider: users, organizations, roles, MFA, federation, themes. Core maps brands to realms, applies themes and authentication policies per brand, and verifies tokens.
- Core's own API is the unified surface: brands, tenants, memberships, API keys, staff grants, audit aggregation, signed webhooks, and brand-scoped access to the Keycloak and Kill Bill APIs. It is thin. Business rules live in the two engines and in their plugins.

## Consequences

- A Core release is a tested bundle: component versions, Core extensions, Core API, upgrade notes. Products upgrade by taking a Core release.
- Reservations, metering, quotas and the wallet view are Core extensions, shared by every product, not product-side code.
- Existing production billing in each product stays untouched until the Core replacement passes tests on the reference stack and in that product's staging. Cutover is a product decision recorded in the product's repository.
- Separate legal sellers are separate Kill Bill tenants under one Core deployment.
- This follows the platform rule not to add another billing engine. Kill Bill is the one engine.
