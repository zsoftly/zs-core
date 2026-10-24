# Brands, identity, tenants and API keys

## TL;DR

A brand is one product on a Core deployment: one Keycloak realm, one Kill Bill tenant, one manifest. A tenant is a personal or organization account inside a brand, backed by a Keycloak Organization and a Kill Bill account. A person is a Keycloak identity keyed on issuer plus subject and reaches a tenant only through an explicit membership that Core records. Permissions are `resource:action` strings the brand registers. A member holds the union of their teams' permissions. Three locked system teams follow the membership role. Every write checks the caller's authority again under the tenant row lock. API keys belong to one tenant, are stored as digests, expire, and never exceed their creator's current permissions. Removing or suspending a member revokes its keys for good. Staff grants are brand-scoped rows in Core. When a brand requires MFA for staff, every staff route needs a second factor in the token. Every authorization decision fails closed. This is the Phase 3 contract.

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
- With `required` and `required_for_staff`, the staff client's browser flow is `zs-browser-staff-mfa`. Its Cookie step is disabled, so a Keycloak SSO session from the customer client never signs a person in to the staff client alone. The staff client always asks for the password and the second factor.
- With `required` and `required_for_staff`, every staff route also needs evidence of a second factor in the staff token: a second-factor value such as `otp` in `amr`, or an `acr` listed in `mfa_acr_values`. Its age does not matter here. Without it the route answers `403 mfa_required`. This holds even if a login path in Keycloak skipped the second factor.
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

Table `tenants`: `id` (`tnt_`), `brand_id`, `kind` (`personal`, `organization`), `slug`, `name` (1 to 100 characters, because Kill Bill stores the account name in a varchar(100)), `status` (`active`, `suspended`, `closed`), `activation` (`pending`, `approved`, `rejected`), `keycloak_organization_id`, `killbill_account_id`, `billing_country`, `created_by_identity_id`. The slug comes from the tenant name plus the last 8 characters of the tenant ID. Users never set it.

Tables `personal_tenant_principals`, `memberships` (`mem_`, role `owner`, `admin`, `billing_admin`, `member`, status `active`, `suspended`, `removed`), `teams` (`tm_`, system and locked flags), `team_members`, `team_grants`, `invitations` (`inv_`, `token_digest`, 7-day expiry), `tenant_activations` (append-only), `tenant_provisioning` (creation record, idempotency record and sweeper queue).

An identity has at most one membership in a tenant that is not removed. A removed membership stays as history. Every row that names an identity carries its brand and references `identities (brand_id, id)`. Every tenant-owned row carries its brand and tenant and references `tenants (brand_id, id)`. An accepted invitation references the membership it created in the same tenant.

Mapping to the engines:

- Creating a tenant creates a Keycloak Organization in the brand realm named after the tenant, adds the creator as a member, and creates a Kill Bill account with the tenant ID as `externalKey`, the tenant name, and the brand's default currency. Both upstream IDs are stored. Core writes the tenant row only after both exist. When either side fails, no tenant row is written, the failure is audited and the caller gets a retryable 503. A sweeper completes the creation later. Keycloak requires unique Organization names, so a second tenant with the same name gets `<name> (<tenant ID>)`.
- Membership in Core is the source of truth for authorization. Keycloak Organization membership mirrors it for display and for Keycloak's own features (organization-scoped login, invitations by email through Keycloak when the brand enables it). Each tenant or membership change queues a `tenant.mirror` event in the outbox in its own transaction. Relays in `serve` apply it to the Organization, at most 4 at a time per instance. A mismatch found by the sweeper is corrected towards Core and audited.
- Keycloak groups and realm roles never create a Core membership.

Rules:

- `POST /v1/tenants/personal` creates or returns the caller's personal tenant for the brand. One per identity. Creates nothing in a payment plugin. Its name is the display name cut to 100 characters, or `Personal`. A second request while the first still creates it waits up to 5 seconds and returns the same tenant. Otherwise it answers a retryable `503 not_ready`, never `409`. A personal tenant takes no invitations: `409 tenant_kind_unsupported`.
- `POST /v1/tenants` creates an organization tenant with the caller as owner; requires `idempotency_key`.
- Roles: `owner` does everything. `admin` manages members and teams but has neither `billing:*` nor `tenant:transfer`, nor the workload permissions `wallet:reserve`, `usage:write` and `quota:consume`. `billing_admin` has `billing:*` only. `member` has what its teams grant. System teams `owners`, `admins`, `members` are created with the tenant, locked, and kept in step with role changes in the same transaction. The `members` system team starts with `tenant:read`, `member:read` and `team:read`. Locked means Core manages the team's name and members. Its grants still change.
- Nobody grants beyond their own permissions. This covers role changes, invitations, team grants and API keys.
- Scope resolution reads the caller's membership before the write's transaction starts. Every write locks the tenant row, reads the caller's membership, role and effective permissions again, and runs its checks on what it read. A caller that is no longer active gets `404 tenant_not_found`. A caller that lost the permission gets `403 permission_denied`. An owner demoted while promoting someone else therefore cannot make an owner.
- The last active owner cannot be removed, suspended or demoted: `409 last_owner`. Ownership transfers to an active admin with recent MFA.
- Invitations: owner or admin invites an email with a non-owner role. The clear token is returned once. Acceptance requires a verified email equal to the invited address, case-insensitive, and recent authentication. Expired or revoked: `404 invitation_not_found`.
- An invitation needs its inviter's authority when it is accepted. The inviter must be an active member with `member:invite` and every permission the invited role implies. Otherwise Core revokes the invitation, audits `tenant.invitation.refused_stale_inviter` and answers `404 invitation_not_found`. Removing a member revokes every pending invitation it sent. Demoting or suspending a member revokes those it can no longer send.
- Accepting an invitation always creates a new membership with a new `mem_` ID. A removed member who comes back never gets the old membership, or its API keys, back.
- Suspending a tenant blocks every write in Core and sets the Kill Bill account to blocked through the entitlement blocking API. Reads continue. The writes that only give back what the tenant holds still go through: settling, releasing and extending a reservation, and releasing a quota lease. Closing is terminal and closes the Kill Bill account. It revokes every API key of the tenant. Suspending, reactivating or closing a closed tenant is `409 tenant_closed`. The check runs again under the tenant lock, so a reactivation that races a close is refused. No HTTP route suspends, reactivates or closes a tenant in v1. Use `zs-core tenant suspend|reactivate|close`.
- Activation: `GET /v1/tenants/{id}/activation`; `POST /v1/staff/tenants/{id}/activation` with the `activation` grant, `{state, reason, request_id}`, idempotent on `request_id`.

## API keys

Table `api_keys`: `id` (`key_`), `brand_id`, `tenant_id`, `created_by_membership_id`, `name`, `digest`, `prefix`, `permissions`, `expires_at` (at most 365 days, default 90), `last_used_at`, `revoked_at`, `revoked_reason`.

Secret format `zs_<brand slug>_<32 bytes base64url>`, shown once.

Rules:

- Creation needs `apikey:create` and recent MFA.
- Requested permissions must be a subset of the creator's.
- Effective permissions at use are the intersection with the creator's current permissions.
- Removing the creator revokes every key it created, with `revoked_reason` `creator_removed`. Suspending the creator revokes them with `creator_suspended`. Closing the tenant revokes every key with `tenant_closed`. Each revocation is audited as `tenant.api_key.revoked`. A revoked key answers `401 api_key_invalid` and never works again, also after the person is invited back.
- A key whose creator is not active and that is not revoked answers `401 api_key_owner_removed`.
- A key against another tenant returns `404`.
- Revocation is immediate and audited.
- An API key gets `403 permission_denied` on token-only routes (`GET /v1/me`, `POST /v1/tenants`, `POST /v1/invitations/accept`) and `404` on staff routes.
- Library entry: `apikey.Verify(ctx, tx, secret)`.

## Staff grants

Table `staff_grants`: `id` (`grt_`), `brand_id`, `issuer`, `subject`, `scope` (`operator`, `activation`, `billing`, `support`, `closure`), `status`, `granted_by`, `reason`.

Staff tokens are verified against the brand's staff client. Authority comes only from an active grant with the needed scope in the same brand. A missing grant returns `404`. With a grant, a brand on `mfa: required` or `required_for_staff` also needs a second factor in the token, or the route answers `403 mfa_required`. No HTTP route creates or revokes grants in v1. `zs-core grant add` creates one and `zs-core grant revoke --brand --issuer --subject --scope --reason --operator` sets it to `revoked`. Both are audited, as `staff.grant.added` and `staff.grant.revoked`. A revoked grant stops working at once, because verification requires an `active` grant. Migrations create no grants. Keycloak realm roles never grant staff authority.

## Scope resolution

Middleware resolves `{brand, tenant?, principal, permissions}` before any handler, in this order:

1. Brand from issuer and audience or key prefix (`401` if unknown).
2. Credential verified (`401 token_invalid`, `401 api_key_invalid`, `503 authentication_unavailable` when JWKS or the database is down).
3. Tenant route membership loaded (`404 tenant_not_found` if missing, inactive, or wrong tenant for a key). An API key gets `403 permission_denied` on token-only routes (`GET /v1/me`, `POST /v1/tenants`, `POST /v1/invitations/accept`) and `404` on staff routes.
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
| `GET /v1/tenants/{id}/audit/events` | member or API key | `audit:read` |
| `GET /v1/brands/{id}`; `POST /v1/brands/{id}/apply` | staff `operator` | n/a |
| `GET /v1/audit/events` | staff `operator`, optional `tenant_id`; `404` without the grant | n/a |

Built-in permissions: `tenant:read`, `tenant:update`, `tenant:transfer`, `member:read`, `member:invite`, `member:update`, `member:remove`, `team:read`, `team:manage`, `apikey:create`, `apikey:read`, `apikey:revoke`, `billing:read`, `billing:manage`, `audit:read`, `wallet:reserve`, `usage:write`, `usage:read`, `quota:consume`. `audit:read` is the Phase 3b permission for tenant members. The last four are the Phase 4b permissions of the wallet, metering and quotas. An admin holds `usage:read` through the role. `wallet:reserve`, `usage:write` and `quota:consume` are workload permissions: only the owner holds them through the role, and otherwise a team grants them or a service API key carries them. A billing admin holds none of the four. A hold or a quota lease belongs to the membership or API key that made it. Another caller, the owner included, gets `404 reservation_not_found`, or `404 not_found` for a lease. Staff with the `support` grant and a reason may settle, release or extend another holder's reservation and release another holder's lease, audited as `wallet.reservation.staff_override` and `quota.lease.staff_override`.

## Error codes

| Code | Name | Status |
| --- | --- | --- |
| ZS-1407 | `mfa_required` | 403 |
| ZS-1600 | `tenant_not_found` | 404 |
| ZS-1601 | `permission_denied` | 403 |
| ZS-1602 | `last_owner` | 409 |
| ZS-1603 | `invitation_not_found` | 404 |
| ZS-1604 | `invitation_email_mismatch` | 403 |
| ZS-1605 | `permission_unknown` | 400 |
| ZS-1606 | `team_locked` | 409 |
| ZS-1607 | `already_member` | 409 |
| ZS-1608 | `tenant_kind_unsupported` | 409 |
| ZS-1609 | `recent_auth_required` | 403 |
| ZS-1610 | `tenant_closed` | 409 |
| ZS-1700 | `api_key_invalid` | 401 |
| ZS-1701 | `api_key_owner_removed` | 401 |

## Configuration

`ZS_TENANT_SWEEP_INTERVAL` sets how often the sweeper runs. The default is 1m.

## Tests that gate Phase 3

- Two brands on one stack, same Keycloak server, two realms: a token for brand A cannot read, create or accept anything in brand B; a Kill Bill call for brand A uses only brand A's tenant credentials.
- `brand apply` twice is a no-op the second time and reports no diff; a changed theme or MFA policy produces exactly that diff in audit.
- Tenant creation rolls back when Kill Bill is unreachable and the sweeper completes it later; the Keycloak Organization and the Kill Bill account carry the Core tenant ID.
- A removed member's live token and live API key both fail on the next request, and the Keycloak Organization no longer lists them after the sweeper runs.
- A removed member's API key stays `401 api_key_invalid` after the person is invited again and accepts. The new membership has a new ID.
- Two owners demoting each other at once leave one owner. An owner demoted while promoting a member cannot make that member an owner.
- Two acceptances of one invitation add one membership.
- An invitation whose inviter lost the authority is refused and revoked.
- A reactivation that races a close is refused with `tenant_closed`.
- A tenant name of 101 characters is refused, and a personal tenant takes a long display name cut to 100 characters. The test Kill Bill refuses longer names, as Kill Bill does.
- 100 tenant writes queue 100 `tenant.mirror` events and start no goroutine each.
- With `mfa: required_for_staff`, a person signed in to the customer client without a second factor is asked for it by the staff client in the same browser, and Core refuses a staff token without a second factor.
- Last-owner guard across removal, suspension, demotion and transfer.
- Invitation acceptance with a different verified email fails; same email in different case succeeds.
- API key permissions shrink when the creator is demoted and fail when the creator is removed.
- Staff grant in brand A grants nothing in brand B.
- Idempotent organization creation: same key same payload returns the same tenant; same key different payload returns `409`.
- JWKS unreachable returns `503 authentication_unavailable`, never `401`.
