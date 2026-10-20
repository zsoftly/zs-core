# Brands

## TL;DR

A brand is one product on a Core deployment: one Keycloak realm, one Kill Bill tenant and one manifest, `brands/<slug>.yaml`. The contract is in [identity-and-tenants.md](identity-and-tenants.md). This page explains how apply, diff and secrets work. `zs-core brand diff` shows what would change and changes nothing. `zs-core brand apply` makes the change. It reads the state of Core, Keycloak and Kill Bill. It computes a diff. It applies the diff one change at a time, audits each change and prints the diff. Applying the same manifest twice gives an empty diff. Secrets are references, `env://NAME` or `file:///path`, resolved at apply time and never stored, logged or shown in a diff. Each brand reaches only its own secrets: `env://BRAND_<SLUG>__...` and files under `ZS_SECRETS_DIR/<slug>/`. Over HTTP, a change to MFA, clients, identity providers, `ssl_required` or the payment plugin needs recent MFA and a reason. Apply never deletes anything in Keycloak or Kill Bill.

## The manifest

The schema is [`api/schemas/brand-manifest.json`](../../api/schemas/brand-manifest.json) (JSON Schema 2020-12). Unknown keys, duplicate keys and more than one YAML document are rejected. Validation also checks these rules:

- The default locale is one of the locales.
- The slug is 2 to 40 lowercase letters and digits in words joined by single hyphens, such as `zs-reference`. A leading, trailing or double hyphen is refused.
- Client IDs and provider aliases are unique.
- File paths stay inside the manifest's directory.
- The realm matches `^[a-z0-9][a-z0-9-]{1,62}$` and is not `master`.
- Each `mfa_acr_values` entry is a non-empty string of at most 64 characters, and not `0` or `1`. List only values the realm issues after a second factor. Keycloak issues `0` and `1` without one.
- Each redirect URI uses `https`. Plain `http` is allowed only on `127.0.0.1`, `localhost` and `[::1]`, on any port. The host has no wildcard. The only wildcard is one trailing `/*`, and a URI with a query or fragment cannot end with it. Custom schemes and fragments are refused in v1.
- Each web origin is `+`, which means the origins of the redirect URIs, or an origin with the same scheme rules and no path. A bare `*` is refused.
- With `mfa: required`, neither the customer client nor the staff client may set `direct_access_grants`. With `mfa: required_for_staff`, the staff client may not. The password grant never runs a login flow, so it would issue tokens without the second factor.

Apply checks the secret references against the brand's namespace before it resolves anything. See [Secrets](#secrets). [`brands/zs-reference.yaml`](../../brands/zs-reference.yaml) matches the compose stack.

Validation uses a JSON Schema library listed in go.mod. It implements draft 2020-12 and was already in the module graph.

## What apply manages

| Manifest | Engine | How |
| --- | --- | --- |
| `identity` realm fields: display name, themes, locales, SSL, token lifetimes, `verify_email` | Keycloak realm | Created when missing, then the listed fields are updated. Fields the manifest omits are left alone. |
| `customer_client`, `staff_client`, `service_clients` | Keycloak clients | Public client with PKCE S256 for customers. Confidential clients for staff and services. Each client gets an audience mapper for its own client ID. The customer and staff clients also get an AMR mapper. Tokens then carry `aud` and `amr`. |
| `mfa` | Keycloak flows | Core creates two copies of the built-in browser flow. `zs-browser` asks for a second factor when the user has one. `zs-browser-mfa` requires OTP. Both set the AMR values `pwd` and `otp`. Core also creates `zs-post-broker-mfa`, which holds one required OTP form with the AMR value `otp`. `optional` binds `zs-browser`. `required` binds `zs-browser-mfa` and sets `zs-post-broker-mfa` as the post-broker-login flow of every identity provider, so a login through a provider also needs the second factor. `required_for_staff` binds `zs-browser` and sets `zs-browser-mfa` on the staff client. A copy of the browser flow cannot serve after a brokered login, because its password form would ask the user for a local password. |
| `required_actions` | Keycloak required actions | Listed actions are enabled and default for new users. Others are not default. `verify_email` uses the realm switch instead. |
| `identity_providers` | Keycloak identity providers | OIDC, SAML, Google, Microsoft, GitHub. Keycloak masks client secrets, so Core stores the secret's SHA-256 in the provider configuration to tell whether it changed. The post-broker-login flow follows `mfa`. Other policies remove it. |
| `killbill_tenant_key`, `killbill_api_secret_ref` | Kill Bill tenant | Created when missing. An existing tenant must accept the referenced secret, or apply stops. |
| `catalogue`, `overdue` | Kill Bill uploads | Uploaded when the file's SHA-256 differs from the one recorded in the tenant's `zs-core-catalogue-sha256` or `zs-core-overdue-sha256` user key. Kill Bill keeps every catalogue version and rejects a second version with the same effective date. |
| `payment_plugin` | Kill Bill per-tenant plugin configuration | Uploaded when it differs, compared in constant time. A plugin change needs a reason on every path. A plugin change is any of these: the plugin is added or removed, its `name`, `config_key` or `config_ref` changes, or its configuration in Kill Bill differs. Over HTTP it also needs MFA within the last 15 minutes. |
| brand row | Core `brands` table | Slug, name, realm, tenant key, secret reference, Kill Bill tenant ID, the applied manifest and its digest. The realm and the tenant key cannot change. |

Apply does not handle these yet: plugin installation through KPM, the invoice plugin and templates (Phase 4), notifications and webhooks (Phase 5), and permissions (Phase 3b). They are validated and stored with the manifest.

## Diff and audit

A diff is a list of changes. Each change names a target, such as `keycloak.realm` or `killbill.catalogue`. It names the object, the kind (`create` or `update`) and the fields with their values before and after. A changed secret is a field with `redacted: true` and no values. Each change is audited as a brand event, for example `brand.keycloak.realm.updated`, with the change in `details`. A failed change is audited with outcome `failure` and the error code. The manifest is recorded last, as `brand.manifest.recorded`. Apply stops at the first failure. It is not atomic across the two engines. Run it again to continue from where it stopped.

Two applies of one brand never interleave: apply holds a database lock per slug and a second one gets `409 brand_apply_in_progress`. Over HTTP the lock is taken only after the slug is found to belong to the brand of the route. A manifest for another brand is refused with `400 brand_manifest_invalid` and never holds that brand's lock.

## Sensitive changes over HTTP

Some fields decide how people sign in or where money goes. An HTTP apply whose diff or manifest changes any of them needs two things. The operator token must show MFA within the last 15 minutes, or the route answers 403 `recent_mfa_required`. The request must carry a non-empty `reason`, or the route answers 400 `reason_required`. The fields are:

- `identity.mfa` and `identity.mfa_acr_values`, and the authentication flows.
- Any client: redirect URIs, web origins, secrets and flows.
- Any identity provider.
- `identity.ssl_required`.
- Any `billing.payment_plugin` field, and the plugin configuration in Kill Bill.

The command line is exempt. It runs as a service actor named by `--operator`, on a host that already holds the admin credentials. A payment plugin change still needs `--reason` there.

Over HTTP, `identity.ssl_required` must be `external` or `all`. The command line may apply `none` for the loopback reference stack and prints a warning when it does.

## Commands and routes

```bash
zs-core brand diff brands/zs-reference.yaml
zs-core brand apply --operator <you> --reason '<why>' brands/zs-reference.yaml
```

Both need the database with migrations applied and the Keycloak and Kill Bill admin settings. They resolve file paths against the manifest's directory.

`POST /v1/brands/{id}/apply` takes the manifest as JSON or YAML from a staff `operator` of that brand, with an optional `reason` query parameter. It returns the diff. File paths in the manifest must start with `<slug>/` and resolve inside `ZS_BRANDS_DIR/<slug>/`, so one brand cannot read another brand's files. The route is long-running. It runs the apply for up to 10 minutes and extends its own response write deadline past `ZS_HTTP_WRITE_TIMEOUT`. A client that disconnects does not stop an apply that has started. The audit trail is then the record of what changed.

Without the Keycloak or Kill Bill admin settings the route answers 503 `not_ready`. When an engine is down it answers 503 `identity_provider_unavailable` or 503 `billing_unavailable`. Kill Bill answers some invalid uploads with a server error, and Core reports every Kill Bill 5xx as 503 `billing_unavailable`. When an engine refuses a change with a 4xx it answers 422 `brand_apply_rejected`. A secret reference outside the brand's namespace gives 422 `secret_ref_invalid`. A secret that does not resolve gives 422 `secret_unresolved`. A bad manifest gives 400 `brand_manifest_invalid`. The first apply of a brand runs from the command line, because no staff grant can exist before the brand.

## Secrets

`env://NAME` reads the environment variable of the process that applies. `file:///path` reads a file, without its trailing newline, up to 64 KiB. Any other form is refused, so a value pasted where a reference belongs is rejected.

References are namespaced per brand. The rule applies to every secret reference, on the command line and over HTTP, including `payment_plugin.config_ref` and `killbill_api_secret_ref`.

- An `env://` reference must start with `BRAND_<SLUG>__`. `<SLUG>` is the slug in upper case with `-` replaced by `_`, and two underscores end the prefix. For `zs-reference` the prefix is `BRAND_ZS_REFERENCE__`. For `zs` it is `BRAND_ZS__`. A slug never holds a double hyphen and never starts or ends with one, so `__` cannot occur inside `<SLUG>`. Two brands therefore never share a prefix, and no prefix starts another. No variable is in the namespace of two brands, whichever brand was created first. Apply also refuses a reference inside the prefix of any other brand in the `brands` table, as a second check.
- A `file://` reference must resolve inside `ZS_SECRETS_DIR/<slug>/`. When `ZS_SECRETS_DIR` is unset, file references are refused. Core opens the brand's directory as a root, so a symbolic link cannot lead outside it.

Validation refuses any other reference with 422 `secret_ref_invalid`. The resolver checks the rule again before it reads anything. A valid reference that does not resolve gives 422 `secret_unresolved` with one message, whether the variable is missing, the file is missing or the file is unreadable.

The reference manifest references two secrets. `BRAND_ZS_REFERENCE__KEYCLOAK_STAFF_CLIENT_SECRET` holds the value of `ZS_STAFF_CLIENT_SECRET` from `deploy/compose/.env`. `BRAND_ZS_REFERENCE__KILLBILL_TENANT_API_SECRET` holds the value of `KILLBILL_TENANT_API_SECRET`.

## Token verification

`serve` verifies tokens against the applied brands. The realm in `iss` selects the brand. Core checks these in every token:

- `iss` equals the realm's discovery issuer.
- The signature is RS256 and verifies against the realm JWKS.
- `typ` is `Bearer`.
- `exp` and `nbf` hold with 60 seconds of leeway.
- `aud` contains a brand audience.
- `azp` is always required. It names one of the brand's clients and appears in `aud`. The client decides the principal kind: the customer client yields a `user`, the staff client a `staff` and a service client a `service` principal.
- `email_verified` is true for actions that need it.
- MFA is read from `amr` and `acr`. Recent MFA means within 15 minutes.

Keycloak emits `otp` in `amr` for the SSO session. A re-authentication inside that session without a second factor can refresh `auth_time`. Core treats `auth_time` as the MFA time. Brands that need stronger proof set a step-up `acr` value.

The realm in `iss` is looked up in a snapshot of the applied brands. A lookup never waits on the database while a snapshot is usable. One refresh at a time loads a new snapshot in the background, when the snapshot is half its TTL old, after an apply over HTTP, or when a token names an unknown realm, at most once every 5 seconds. A snapshot is served until it is one TTL old, 30 seconds in `serve`. A failed refresh keeps the previous snapshot until then. Only a request with no usable snapshot waits, for the running refresh or for its own deadline, whichever comes first. If neither gives a snapshot, the request gets 503 `authentication_unavailable`. A token of a brand applied after the last refresh gets 401 `token_audience_unknown` until the refresh it starts completes.

Keys come from `ZS_KEYCLOAK_URL` and are cached for 10 minutes. They keep working for an hour while Keycloak is unreachable. Any answer other than 200 from discovery or the key endpoint, 404 included, counts as unreachable. Cached keys are served at once. When they are due for a refresh, one refresh per realm runs in the background. A request waits only when it has no usable key: no keys yet, keys older than an hour, or an unknown key ID. It waits for the running refresh or for its own deadline, whichever comes first. Refreshes start at most once every 30 seconds, whatever triggers them.

An unknown realm or audience gives 401 `token_audience_unknown`. Any other defect gives 401 `token_invalid`. A token without a verified email gives 403 `email_not_verified` where the action needs one. Unreachable keys give 503 `authentication_unavailable`.

Known gap: with `required_for_staff`, a staff member who signs in through a brokered identity provider is not asked for a second factor by Keycloak, because the post-broker flow binding is per provider, not per client. Core's recent-MFA checks still refuse sensitive actions for such a session. Phase 3b binds the staff client to the MFA post-broker flow.
