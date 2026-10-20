# Brands, identity, tenants and API keys

## TL;DR

A brand is one product on a Core deployment: one Keycloak realm, one Kill Bill tenant, one manifest. A tenant is a personal or organization account inside a brand, backed by a Keycloak Organization and a Kill Bill account. A person is a Keycloak identity keyed on issuer plus subject and reaches a tenant only through an explicit membership that Core records. Permissions are `resource:action` strings the brand registers. A member holds the union of their teams' permissions. Three locked system teams follow the membership role. API keys belong to one tenant, are stored as digests, expire, and never exceed their creator's current permissions. Staff grants are brand-scoped rows in Core. Every authorization decision fails closed. This is the Phase 3 contract.

## Brands

Table `brands`: `id` (`brd_`), `slug` (unique, 2 to 40 chars, lowercase letters and digits in words joined by single hyphens), `name`, `status`, `keycloak_realm`, `killbill_tenant_id`, `killbill_api_key`, `killbill_api_secret_ref` (secret store reference, never the value), `manifest_digest`, `applied_at`, timestamps.

The manifest (`brands/<slug>.yaml`, schema `api/schemas/brand-manifest.json`) declares:

- `identity`: realm display name, login theme, email theme, locales (`en`, `fr`), customer client (OIDC public client with PKCE), staff client, service clients, brokered providers (Google, Microsoft, GitHub, enterprise OIDC or SAML) with secret references, authentication policy (`mfa: optional | required | required_for_staff`, TOTP and WebAuthn allowed, see below), token lifetimes, required actions (verify email, update profile).
- `billing`: Kill Bill tenant key, catalogue file path, currencies, legal seller name and address, payment plugin (`stripe`, `adyen`, `paypal-express` once a Java plugin exists, or a custom plugin name) with a configuration reference, invoice plugin (optional, for tax), email template set, overdue policy file.
- `notifications`: sender identity, reply-to, webhook endpoints with secret references.
- `permissions`: the `resource:action` strings the product uses beyond the built-ins.

Secret references in v1 are only `env://NAME` and `file:///abs/path`. They are namespaced per brand. An `env://` reference of brand `<slug>` starts with `BRAND_<SLUG>__`, such as `BRAND_ZS_REFERENCE__` for `zs-reference` and `BRAND_ZS__` for `zs`. The double underscore ends the prefix and cannot occur inside a slug, so the namespaces of two brands never overlap. A `file://` reference resolves inside `ZS_SECRETS_DIR/<slug>/`. Any other reference is `422 secret_ref_invalid`. See [brands.md](brands.md#secrets).

`zs-core brand apply <file>` converges Keycloak (realm, clients, brokers, theme, locales, flows, required actions) through the Admin API, converges Kill Bill (tenant, plugin install through KPM, per-tenant plugin configuration, catalogue upload, overdue upload, templates) through its API, stores the result, and audits every change with a diff. Apply is idempotent and refuses unknown manifest keys.

Rules:

- A token acts inside exactly one brand, chosen by its issuer (the realm) and audience. Unknown realm or audience: `401 token_audience_unknown`.
- Nothing crosses brands: not tokens, not API keys, not staff grants, not webhooks.
- `mfa: required` holds for every login path. The realm's browser flow requires OTP, and every identity provider runs a post-broker-login flow that requires OTP. Neither the customer client nor the staff client may allow the password grant. `required_for_staff` binds the OTP flow to the staff client and refuses the password grant on it.
- Changing a brand's payment plugin is an apply with a required `reason`. Over HTTP it also needs recent staff MFA. It never migrates stored payment methods automatically.
- Over HTTP, an apply that changes MFA settings, a client, an identity provider, `ssl_required` or the payment plugin needs recent staff MFA (`403 recent_mfa_required`) and a `reason` (`400 reason_required`). The command line is exempt because it runs as a service actor named by `--operator`.

## Identities

Table `identities`: `id` (`idn_`), `brand_id`, `issuer`, `subject`, `keycloak_user_id`, `email`, `email_verified`, `display_name`, `last_seen_at`. Unique on `(brand_id, issuer, subject)`.

Rules:

- Created on first authenticated contact with a verified email claim. Without `email_verified: true` the caller is authenticated but cannot create a tenant or accept an invitation. The API returns `403 email_not_verified`.
- Email is refreshed from the token. It is never used to find or merge identities.
- Core reads the identity first. It writes the row only when the email, the verified flag or the display name changed, or `last_seen_at` is 5 minutes old or older.
- Verification: RS256 against the realm JWKS, `iss` equals the realm issuer, `aud` contains a brand audience, `typ` is `Bearer`, `exp` and `nbf` with 60 seconds leeway. `azp` is always required, must name one of the brand's clients and must appear in `aud`. The client decides the principal kind. MFA is read from `amr` and `acr`. `RequireRecentMFA(15 minutes)` guards API key creation, owner transfer, member removal and sensitive brand applies over HTTP. It treats `auth_time` as the time of the second factor. See [brands.md](brands.md#token-verification) for the limit this implies.
- A token stays valid until it expires. Core re-checks membership and tenant status on every request, so a removed member loses access at once.

## Tenants

Table `tenants`: `id` (`tnt_`), `brand_id`, `kind` (`personal`, `organization`), `slug`, `name`, `status` (`active`, `suspended`, `closed`), `activation` (`pending`, `approved`, `rejected`), `keycloak_organization_id`, `killbill_account_id`, `billing_country`, `created_by_identity_id`.

Tables `personal_tenant_principals`, `memberships` (`mem_`, role `owner`, `admin`, `billing_admin`, `member`, status `active`, `suspended`, `removed`), `teams` (`tm_`, system and locked flags), `team_members`, `team_grants`, `invitations` (`inv_`, `token_digest`, 7-day expiry), `tenant_activations` (append-only).

Mapping to the engines:

- Creating a tenant creates a Keycloak Organization in the brand realm named after the tenant, adds the creator as a member, and creates a Kill Bill account with the tenant ID as `externalKey`, the tenant name, and the brand's default currency. Both upstream IDs are stored. Failure of either side rolls back the Core row and audits the failure; a sweeper retries orphans.
- Membership in Core is the source of truth for authorization. Keycloak Organization membership mirrors it for display and for Keycloak's own features (organization-scoped login, invitations by email through Keycloak when the brand enables it). A mismatch found by the sweeper is corrected towards Core and audited.
- Keycloak groups and realm roles never create a Core membership.

Rules:

- `POST /v1/tenants/personal` creates or returns the caller's personal tenant for the brand. One per identity. Creates nothing in a payment plugin.
- `POST /v1/tenants` creates an organization tenant with the caller as owner; requires `idempotency_key`.
- Roles: `owner` does everything; `admin` manages members and teams but not `billing:*`; `billing_admin` has `billing:*` only; `member` has what its teams grant. System teams `owners`, `admins`, `members` are created with the tenant, locked, and kept in step with role changes in the same transaction.
- The last active owner cannot be removed, suspended or demoted: `409 last_owner`. Ownership transfers to an active admin with recent MFA.
- Invitations: owner or admin invites an email with a non-owner role. The clear token is returned once. Acceptance requires a verified email equal to the invited address, case-insensitive, and recent authentication. Expired or revoked: `404 invitation_not_found`.
- Suspending a tenant blocks every write in Core and sets the Kill Bill account to blocked through the entitlement blocking API; reads continue. Closing is terminal and closes the Kill Bill account.
- Activation: `GET /v1/tenants/{id}/activation`; `POST /v1/staff/tenants/{id}/activation` with the `activation` grant, `{state, reason, request_id}`, idempotent on `request_id`.

## API keys

Table `api_keys`: `id` (`key_`), `brand_id`, `tenant_id`, `created_by_membership_id`, `name`, `digest`, `prefix`, `permissions`, `expires_at` (at most 365 days, default 90), `last_used_at`, `revoked_at`, `revoked_reason`.

Secret format `zs_<brand slug>_<32 bytes base64url>`, shown once.

Rules: creation needs `apikey:create` and recent MFA; requested permissions must be a subset of the creator's; effective permissions at use are the intersection with the creator's current permissions; creator not active returns `401 api_key_owner_removed`; a key against another tenant returns `404`; revocation is immediate and audited. Library entry: `apikey.Verify(ctx, tx, secret)`.

## Staff grants

Table `staff_grants`: `id` (`grt_`), `brand_id`, `issuer`, `subject`, `scope` (`operator`, `activation`, `billing`, `support`), `status`, `granted_by`, `reason`.

Staff tokens are verified against the brand's staff client. Authority comes only from an active grant with the needed scope in the same brand. A missing grant returns `404`. No HTTP route creates or revokes grants in v1. `zs-core grant add` creates one and `zs-core grant revoke --brand --issuer --subject --scope --reason --operator` sets it to `revoked`. Both are audited, as `staff.grant.added` and `staff.grant.revoked`. A revoked grant stops working at once, because verification requires an `active` grant. Migrations create no grants. Keycloak realm roles never grant staff authority.

## Scope resolution

Middleware resolves `{brand, tenant?, principal, permissions}` before any handler, in this order:

1. Brand from issuer and audience or key prefix (`401` if unknown).
2. Credential verified (`401 token_invalid`, `401 api_key_invalid`, `503 authentication_unavailable` when JWKS or the database is down).
3. Tenant route membership loaded (`404 tenant_not_found` if missing, inactive, or wrong tenant for a key).
4. Handlers call `scope.Require(ctx, "resource:action")` (`403 permission_denied`).

The Phase 1 trusted-header mode is removed here.

## Routes

| Method and path | Auth | Permission |
| --- | --- | --- |
| `GET /v1/me` | token | none |
| `POST /v1/tenants/personal` | token, verified email | none |
| `POST /v1/tenants` | token, verified email, `idempotency_key` | none |
| `GET, PATCH /v1/tenants/{id}` | member | `tenant:read`, `tenant:update` |
| `GET /v1/tenants/{id}/members`; `PATCH, DELETE .../members/{mem}` | member, recent MFA on changes | `member:read`, `member:update`, `member:remove` |
| `POST /v1/tenants/{id}/ownership/transfer` | owner, recent MFA | `tenant:transfer` |
| `GET, POST /v1/tenants/{id}/teams`; `PATCH, DELETE .../teams/{tm}`; `PUT .../teams/{tm}/members`, `.../grants` | member | `team:read`, `team:manage` |
| `GET, POST /v1/tenants/{id}/invitations`; `DELETE .../invitations/{inv}` | member | `member:invite` |
| `POST /v1/invitations/accept` | token, verified email, recent auth | none |
| `GET /v1/tenants/{id}/activation`; `POST /v1/staff/tenants/{id}/activation` | member; staff `activation` | `tenant:read`; n/a |
| `GET, POST /v1/tenants/{id}/api-keys`; `DELETE .../api-keys/{key}` | member, recent MFA on create | `apikey:*` |
| `GET /v1/brands/{id}`; `POST /v1/brands/{id}/apply` | staff `operator` | n/a |
| `GET /v1/audit/events` | staff `operator`, optional `tenant_id`; `404` without the grant | n/a |

Built-in permissions: `tenant:read`, `tenant:update`, `tenant:transfer`, `member:read`, `member:invite`, `member:update`, `member:remove`, `team:read`, `team:manage`, `apikey:create`, `apikey:read`, `apikey:revoke`, `billing:read`, `billing:manage`, `audit:read`. `audit:read` is the Phase 3b permission for tenant members.

## Tests that gate Phase 3

- Two brands on one stack, same Keycloak server, two realms: a token for brand A cannot read, create or accept anything in brand B; a Kill Bill call for brand A uses only brand A's tenant credentials.
- `brand apply` twice is a no-op the second time and reports no diff; a changed theme or MFA policy produces exactly that diff in audit.
- Tenant creation rolls back when Kill Bill is unreachable and the sweeper completes it later; the Keycloak Organization and the Kill Bill account carry the Core tenant ID.
- A removed member's live token and live API key both fail on the next request, and the Keycloak Organization no longer lists them after the sweeper runs.
- Last-owner guard across removal, suspension, demotion and transfer.
- Invitation acceptance with a different verified email fails; same email in different case succeeds.
- API key permissions shrink when the creator is demoted and fail when the creator is removed.
- Staff grant in brand A grants nothing in brand B.
- Idempotent organization creation: same key same payload returns the same tenant; same key different payload returns `409`.
- JWKS unreachable returns `503 authentication_unavailable`, never `401`.
