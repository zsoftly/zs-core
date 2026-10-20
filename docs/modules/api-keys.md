# API keys

## TL;DR

An API key lets software act in one tenant without a person's token. The contract is in [identity-and-tenants.md](identity-and-tenants.md#api-keys). A key belongs to one tenant of one brand and acts as the membership that created it. Its secret is shown once and never stored. Core keeps a SHA-256 digest and a short prefix for display. A key expires after 90 days by default and after 365 days at most. Its permissions are fixed at creation and must be a subset of the creator's. At every use they are intersected with the creator's current permissions. A key shrinks when its creator is demoted. Removing or suspending its creator revokes it, and so does closing its tenant. A revoked key never works again, also when its creator is invited back. Revocation takes effect on the next request. The code is `pkg/apikey`.

## The secret

A secret looks like `zs_zs-reference_<43 characters>`. The middle part is the brand slug. The last part is 32 random bytes in unpadded base64url. A client sends it as `Authorization: Bearer zs_...`. Core tells a key from a Keycloak token by the `zs_` prefix, which a JWT never has. The slug names the brand, and the stored key must belong to that brand, so a key never acts in another brand.

Core stores the SHA-256 digest of the whole secret and compares digests in constant time. The prefix it keeps for display is the slug and the first 6 random characters, such as `zs_zs-reference_Q2x9aB`. The secret never appears in logs, errors, audit events or list responses.

## Creating a key

`POST /v1/tenants/{id}/api-keys` takes a name, a list of permissions and an optional `expires_at`. It needs:

- `apikey:create`, or `403 permission_denied`.
- Multi-factor authentication within the last 15 minutes, or `403 recent_mfa_required`. An API key never has it, so a key cannot create another key.
- Permissions registered for the brand, or `400 permission_unknown`.
- Permissions the caller holds now, or `403 permission_denied`.
- An `expires_at` in the future and at most 365 days away. Without one the key expires in 90 days.
- An active tenant.

Core checks the caller's membership and permissions again after it locks the tenant row, so a creator demoted a moment before cannot create a key with the permissions it lost.

The response holds the key and its secret. The secret is never shown again. Creation is audited as `tenant.api_key.created` with the key ID, prefix, permissions and expiry.

## Using a key

On each request Core:

1. Parses the secret and looks the key up by digest in the brand the slug names. Core answers `401 api_key_invalid` for a malformed, unknown, expired or revoked key. It answers the same for a key of a brand that is not active and applied.
2. Loads the creating membership. When it is not active, the answer is `401 api_key_owner_removed`. Removal and suspension revoke the creator's keys, so step 1 already refuses those. This step guards a membership changed any other way.
3. Computes the creator's effective permissions as for any member, see [tenants.md](tenants.md#permissions), and keeps the key's permissions that are in that set.
4. Writes `last_used_at` when it is empty or a minute old or older.

A database that cannot be reached is `503 authentication_unavailable`, never 401.

A key works on the routes of its own tenant only. Another tenant gets `404 tenant_not_found`. Routes that need a person's token, such as `GET /v1/me`, `POST /v1/tenants` and `POST /v1/invitations/accept`, answer `403 permission_denied`. Staff routes answer `404`. Routes that need recent MFA answer `403 recent_mfa_required`. The audit actor of a key's actions is `api_key` with the key ID.

Examples, for a key created by an admin with `member:read`, `team:manage` and `vm:create`:

- The admin is demoted to member. The key keeps `member:read`, which the `members` team grants, and loses `team:manage` and `vm:create` unless a team still grants them.
- The admin is promoted to owner. The key keeps exactly its three permissions. It never grows past what it was created with.
- The admin is removed or suspended. Core revokes the key in the same transaction. Every request with it is `401 api_key_invalid`.
- The admin is removed and later invited back. The admin gets a new membership with a new ID. The key still belongs to the old, removed membership and stays revoked: `401 api_key_invalid`.

Library callers use `apikey.Verify(ctx, tx, secret)` inside their own transaction. It returns the key, the brand slug, the tenant status, the creating membership and the effective permissions. Scope resolution refuses writes to a suspended tenant and everything on a closed one. A suspended tenant's key still settles, releases and extends its reservations and releases its quota leases.

A service key that carries `wallet:reserve` or `quota:consume` owns the holds and quota leases it makes. Neither the membership that created it nor any other caller of the tenant can settle, release or extend them. See [wallet.md](wallet.md#holders).

## Listing and revoking

`GET /v1/tenants/{id}/api-keys` needs `apikey:read`. It lists every key of the tenant, revoked and expired ones included, oldest first, with cursor pagination. `DELETE /v1/tenants/{id}/api-keys/{key}` needs `apikey:revoke` and takes an optional `reason` query parameter. The key stops working on the next request. Revocation is audited as `tenant.api_key.revoked`. A key of another tenant is `404`.

Core also revokes keys itself, in the transaction of the change that calls for it:

| Change | Keys revoked | `revoked_reason` |
| --- | --- | --- |
| A member is removed | Every key the member created | `creator_removed` |
| A member is suspended | Every key the member created | `creator_suspended` |
| The tenant is closed | Every key of the tenant | `tenant_closed` |

Each one is audited as `tenant.api_key.revoked`, with the reason, the key ID and its prefix. Reactivating a member or inviting it back restores no key. The creator makes new ones.

## Storage

Table `api_keys`, created by `migrations/000011_api_keys.up.sql`. A check constraint holds the expiry within 365 days of creation, and a unique index on the digest serves the lookup. The digest column holds 32 bytes and nothing else. `revoked_reason` holds the reason given to `DELETE`, or the reason Core recorded.
