# Billing

## TL;DR

Kill Bill is the billing engine and the financial authority. Core holds no balance, no invoice and no payment. Each brand is a Kill Bill tenant. Each Core tenant is a Kill Bill account keyed by the tenant ID. Billing routes read and write Kill Bill with the brand's credentials and Core's audit headers. Core keeps promo codes, credit expiry, a cache of billing standing, push notification receipts, plugin installs and the plugin DDLs it applied. Payment and invoice plugins run in test mode only, and brand apply refuses real-money mode. A brand can run several payment plugins with one default. Products get a 15-minute Core credential and call Kill Bill through a proxy for the tenant's own account. Kill Bill push notifications become Core events in the outbox. Outbound webhook delivery is deferred for this candidate. The code is `pkg/billing` and `internal/httpapi/billing.go`.

## Mapping

| Core | Kill Bill | Notes |
| --- | --- | --- |
| Brand | Tenant | Every call carries `X-Killbill-ApiKey` and `X-Killbill-ApiSecret` of the brand. Core resolves the secret from the brand's secret reference at each call and never stores it. |
| Tenant | Account | External key is the `tnt_` ID. Currency is the brand's first currency. |
| Catalogue summary | `GET /1.0/kb/catalog` | The last version is in force. Prices become minor units. |
| Billing summary | `GET /1.0/kb/accounts/{id}?accountWithBalanceAndCBA=true`, `/overdue`, `/block` | Balance and credit come from Kill Bill on every read. |
| Subscription | Subscription | Kill Bill subscription ID. Created with an external key derived from the idempotency key. |
| Plan change | `PUT /1.0/kb/subscriptions/{id}?billingPolicy=` | `immediate` is `IMMEDIATE`, `end_of_term` is `END_OF_TERM`. |
| Cancellation | `DELETE /1.0/kb/subscriptions/{id}` | The policy applies to entitlement and billing. |
| Invoice | Invoice | Read only. Lines on a single invoice. HTML and PDF are Kill Bill's rendering. See [invoices.md](invoices.md). |
| Payment, refund | Payment, `POST /1.0/kb/payments/{id}/refunds` | Refunds by staff with a `billing` grant. The refund's transaction external key is derived from the idempotency key. |
| Payment method | Payment method of the brand's default plugin | `__EXTERNAL_PAYMENT__` when the brand names no plugin. |
| Hosted payment page | `POST /1.0/kb/paymentGateways/hosted/form` | Kill Bill adds a payment method of the plugin and asks it for its form. |
| Credit | `POST /1.0/kb/credits?autoCommit=true` | A `CREDIT_ADJ` item. Kill Bill refuses zero and negative amounts with error code 4009. |
| Credit expiry | `POST /1.0/kb/invoices/charges/{accountId}?autoCommit=true` | An external charge, which Kill Bill pays from the account credit. |
| Standing | Overdue state and blocking states | Cached in `tenant_billing_standing`. Kill Bill stays the authority. |
| Push notification | `POST /1.0/kb/tenants/registerNotificationCallback?cb=` | One callback per Kill Bill tenant. Registering replaces the previous one. |
| Plugin install | `POST /1.0/kb/nodesInfo` with `INSTALL_PLUGIN` and `START_PLUGIN` | KPM, from Core's verified plugin mirror. See [plugins.md](plugins.md). |

Objects that live in Kill Bill only, such as subscriptions, invoices, payments and payment methods, are named by their Kill Bill ID. Core never maps them to Core IDs.

## What Core stores

| Table | Holds | Never holds |
| --- | --- | --- |
| `credit_codes` | Brand, SHA-256 digest of the code keyed by the brand, the first four characters, amount in minor units, currency, cap, per-tenant limit, expiry, credit validity, reason, status | The code itself |
| `credit_code_redemptions` | Tenant, a code of the same brand, Kill Bill credit and invoice IDs, amount, expiry of the credit, the part reversed, the sweeper's failed attempts | A balance |
| `staff_credits` | Tenant, idempotency key, Kill Bill credit and invoice IDs, amount, reason, expiry, the part reversed, the sweeper's failed attempts | A balance |
| `tenant_billing_standing` | Overdue state name, whether it blocks changes or entitlement, last Kill Bill event | Amounts |
| `killbill_notifications` | Digest of each push notification and of the state Core read for it, its type, tenant, object and that state, for seven days | The notification body |
| `killbill_plugin_installs` | Plugin, version, Kill Bill node, the brand whose apply installed it, times | Configuration |
| `killbill_plugin_ddl` | Plugin, version, SHA-256 of the DDL, its tables, the tables the last apply created, the operator | The DDL text |

Core never stores a balance, an invoice, a payment, card data or a payment method reference. Core's redaction controls do not cover upstream Kill Bill or plugin logging. The selected [reference Compose controls](../../deploy/compose/README.md) suppress the upstream email-handler, KPM and Beatrix event-dispatch categories, including errors, at the cost of diagnostic detail and event-dispatch diagnostics. Other deployments must configure equivalent controls. Treat suppression as unproven until the effective JVM configuration, selected configuration and every enabled sink have been checked with synthetic values. Every amount in the tables above names the Kill Bill object it produced.

## Routes

| Method and path | Needs |
| --- | --- |
| `GET /v1/brands/{id}/catalogue` | Any token of the brand |
| `GET /v1/tenants/{id}/billing` | `billing:read` |
| `GET, POST /v1/tenants/{id}/subscriptions` | `billing:read`, `billing:manage` |
| `POST /v1/tenants/{id}/subscriptions/{sub}/change`, `DELETE /v1/tenants/{id}/subscriptions/{sub}` | `billing:manage` |
| `GET /v1/tenants/{id}/entitlements/{feature}` | `tenant:read` |
| `GET /v1/tenants/{id}/invoices`, `.../invoices/{inv}`, `.../invoices/{inv}/html`, `.../invoices/{inv}/pdf` | `billing:read` |
| `GET /v1/tenants/{id}/payments` | `billing:read` |
| `POST /v1/tenants/{id}/payments/{pay}/refund` | Staff `billing` grant |
| `GET, POST /v1/tenants/{id}/payment-methods`, `PUT .../payment-methods/{pm}/default`, `DELETE .../payment-methods/{pm}` | `billing:read`, `billing:manage` |
| `POST /v1/tenants/{id}/credits/redeem` | `billing:manage` |
| `POST /v1/staff/tenants/{id}/credits` | Staff `billing` grant |
| `GET /v1/tenants/{id}/billing/credentials` | `billing:manage` |
| `GET, POST /v1/tenants/{id}/billing/proxy/{path}` | A billing credential |
| `POST /v1/billing/webhooks/killbill/{brand}/{token}` | The brand's callback token |

Tenant routes follow [tenants.md](tenants.md#scope-resolution). A closed tenant answers `404 tenant_not_found` on every billing route. A suspended tenant keeps its reads and loses its writes. Staff routes answer `404` without the grant. `api/openapi.yaml` is the contract.

## Errors

A Kill Bill 5xx, a timeout, a 408 or refused credentials is `503 billing_unavailable`, retryable. A Kill Bill 4xx is `422 billing_rejected`, and its message carries Kill Bill's numeric error code only, never Kill Bill's text. An object of another account, or one Kill Bill does not know, is `404 not_found`, and the two are never told apart. Core refuses a path value that is not a UUID before it calls Kill Bill.

Kill Bill 0.24.22 answers some validation failures with a 500, such as a second subscription with an existing external key. Core reports those as `503 billing_unavailable`, like every 5xx.

## Holds

`GET /v1/tenants/{id}/billing` lists why billing changes are on hold:

- `tenant_suspended`: the Core tenant is suspended.
- `account_blocked`: a Kill Bill blocking state in force blocks changes, entitlement or billing. Suspension adds one.
- `overdue_blocks_changes`: the overdue state blocks changes.
- `overdue_disables_entitlement`: the overdue state disables entitlement.

While any reason holds, creating a subscription or changing a plan answers `409 wallet_on_hold`. Every reason except `overdue_blocks_changes` also makes `CheckEntitlement` answer `allowed: false` with the reason `wallet_on_hold`.

## Subscriptions and idempotency

Creating a subscription, adding a payment method and refunding a payment take an `idempotency_key`. Core runs the command once per key, actor and tenant and replays the result for 24 hours. Core also derives the Kill Bill external key of the object from the tenant, the actor and the key. A retry after a lost answer therefore finds the subscription, payment method or refund Kill Bill already made instead of making a second one.

## Refunds

A refund may not exceed what the payment captured or purchased, less what it has already refunded. Core reads the payment from Kill Bill and checks this before it calls Kill Bill. A larger amount is `422 billing_rejected`, and the message carries `refund_exceeds_payment`. A retry of a refund Kill Bill already made is not checked again, because that refund already counts as refunded.

## Entitlements

`CheckEntitlement(ctx, scope, feature)` is in `pkg/billing`. A feature is an active product's name or a usage unit in that product's plans in the catalogue in force. The answer is `{allowed, limit, used, remaining, reason, plan}`. Since Phase 4b the route `GET /v1/tenants/{id}/entitlements/{feature}` answers with `pkg/quota`. It starts from this check and adds the brand's quotas. When a quota applies, `limit`, `used` and `remaining` are set. See [quotas.md](quotas.md). The check fails closed. Every failure is `allowed: false` with a reason, never an error:

| Reason | When |
| --- | --- |
| `included_in_plan` | Allowed. `plan` names the plan. |
| `no_active_subscription` | No subscription is `ACTIVE`. |
| `feature_not_in_plan` | No active subscription's product has the feature. |
| `wallet_on_hold` | A hold that stops entitlement. |
| `billing_unavailable` | Kill Bill, the database or the billing settings are not reachable. |
| `tenant_not_found` | The tenant is closed or unknown. |
| `feature_invalid` | The feature name is malformed. The route answers 400 instead. |
| `quota_exceeded` | A quota applies and the tenant has used its limit. Set by `pkg/quota`. |

## Promo and voucher codes

`zs-core credit-code add` adds a code to a brand. Core stores its digest and its first four characters, never the code. The code is 4 to 64 letters, digits and dashes and is not case sensitive. It has an amount in minor units, a currency the brand supports, an optional cap across tenants, a per-tenant limit, 1 by default, an optional expiry and an optional credit validity in days.

```bash
zs-core credit-code add --brand zs-reference --code WELCOME10 --amount-minor 1000 --currency CAD \
  --max-redemptions 500 --credit-valid-days 90 --reason 'launch offer' --operator <you>
zs-core credit-code list --brand zs-reference
zs-core credit-code disable --brand zs-reference --id ccd_... --reason '<why>' --operator <you>
```

`POST /v1/tenants/{id}/credits/redeem` writes one Kill Bill account credit with the code, in upper case, as `X-Killbill-Reason`. It answers `201` with the credit. Redemption is idempotent per tenant and code. Once the tenant has used its per-tenant limit, a redemption answers `200` with the latest credit and writes nothing. Another tenant redeems until the cap, after which the answer is `409 credit_code_exhausted`. An unknown, disabled or expired code, a code of another brand and a code in another currency than the account are all `404 credit_code_invalid`.

Core writes the redemption row as pending before it calls Kill Bill, and holds it for two minutes. Concurrent redemptions of one code by one tenant wait on the code row, so Kill Bill gets one credit. A retry after Kill Bill was unreachable resumes the pending row. The row keeps the expiry its first attempt set, so a retry never extends a credit. The retry first looks for a credit whose description names the row, `zs-core credit crd_...`, so a credit Kill Bill made before its answer was lost is not written twice. A credit Kill Bill refused frees the row and the redemption.

## Staff credits and expiry

`POST /v1/staff/tenants/{id}/credits` writes a Kill Bill credit with the staff member's reason. With `expires_at`, the credit expires. A code with a credit validity gives its credits an expiry too. Kill Bill does not expire credit, so a sweeper does. `serve` runs it every `ZS_CREDIT_SWEEP_INTERVAL`, one hour by default, and `zs-core credit sweep` runs one pass. Only one pass runs at a time across all Core instances.

For each credit past its expiry, the sweeper first looks for a charge that names the credit, `zs-core credit expiry crd_...`. An earlier pass that failed half way may have written it. When it exists, the sweeper records that charge with its own amount and ID. It never charges twice and never recomputes the amount.

Otherwise it reads the account credit from Kill Bill just before the charge. Kill Bill reports one balance per account, so the sweeper assumes Core credits are used oldest first and credit from other sources is used last. Credit that did not come from Core, such as an overpayment, is never reversed. The sweeper reverses the least of these amounts:

- the credit's own amount
- the account credit
- the account credit, less the unexpired Core credits issued after it, less the credit that did not come from Core

The credit that did not come from Core is the larger of two estimates. One is the account credit beyond every unexpired Core credit of the tenant. The other is the credit Kill Bill added from other sources, capped at the account credit. Kill Bill shows each addition to the account credit as a positive `CBA_ADJ` item, and Core subtracts the ones its own credits made. Pending Core credits count as unexpired.

When the amount is zero or less, the sweeper marks the credit expired with nothing reversed and audits `billing.credit.expired_nothing_to_reverse`. Otherwise it writes an external charge for the amount, which Kill Bill pays from the account credit. It records the charge and audits `billing.credit.expired`. It audits only when its own update marked the credit expired.

No reservation covers the expiry charge, so it lowers the credit a wallet relies on. The sweeper writes it under the tenant's wallet lock and moves the tenant's wallet version on before it releases the lock. No `Reserve` of any Core instance reads in between, and none relies on the credit it cached before. A redeemed code and a granted staff credit move the version on too, in the transaction that records them. See [wallet.md](wallet.md#the-credit-cache).

A tenant with a pending credit waits for a later pass, because that credit may already sit in the account credit. A credit whose brand is not active waits too. Neither counts as a failure. A credit whose expiry fails is retried 10 seconds later, then after twice the previous wait, up to once a day. Each failure adds one to `attempts`. A pass reads the due credits in pages, oldest first, and moves past the ones that wait, so they never hold up the others.

A pass holds a database advisory lock on its own connection. It keeps no transaction open while it calls Kill Bill, apart from the short one that holds the wallet lock across the expiry charge, and records each credit in its own short transaction. Each pass also deletes push notification receipts older than seven days.

## Payment methods

`POST /v1/tenants/{id}/payment-methods` adds a payment method through the brand's plugin. It never takes card data. Two flows exist:

- `hosted_page: true`: Kill Bill adds a payment method of the plugin and asks the plugin for the form of its hosted payment page. The answer carries the form's URL, method and fields. The fields include the provider's client-side token when the plugin sets one. The customer completes the page with the provider. The external payment plugin returns an empty form.
- `external_payment_method_id`: the token the provider's own client library made in the browser, such as a Stripe payment method ID. The Stripe plugin 8.0.3 refuses a payment method without it or a `sessionId` property.

`plugin_properties` and `hosted_page_fields` pass keys and values to the plugin. Core redaction does not establish upstream plugin log safety. The external payment plugin takes one payment method per account, and Kill Bill refuses to delete the default payment method. Both refusals are `422 billing_rejected`.

`GET .../invoices/{inv}/html` returns Kill Bill's rendering of the invoice with the brand's template, in the locale of the tenant's account. `GET .../invoices/{inv}/pdf` renders that HTML through the PDF renderer of `ZS_PDF_RENDERER_URL`. Without a renderer it answers `404 invoice_pdf_unavailable` for an invoice of the tenant. See [invoices.md](invoices.md).

## Billing credentials and the proxy

Products need a short-lived credential. Kill Bill has none, and its tenant secret opens every account of the tenant. So Core issues its own credential and exchanges it on a proxy route.

1. A member with `billing:manage` calls `GET /v1/tenants/{id}/billing/credentials`. Core returns a token `zsbc_...` that expires after 15 minutes, the proxy path, and the tenant's Kill Bill account ID and external key. Core audits `billing.credential.issued`.
2. The product sends `Authorization: Bearer zsbc_...` to `GET` or `POST /v1/tenants/{id}/billing/proxy/<Kill Bill path>`, such as `/v1/tenants/tnt_.../billing/proxy/1.0/kb/accounts/<account ID>/invoices`.
3. Core checks the token's signature and expiry, and confirms it was issued for the tenant in the path. It reads the holder's membership, or API key, again and requires it to be active with `billing:manage`. The tenant must not be closed, and must be active for a `POST`.
4. Core applies the account-scoping rule, then calls Kill Bill with the brand's credentials and returns Kill Bill's answer. A `Location` header points at the proxy route.

The token is an HMAC-SHA256 over its claims, the brand, the tenant, the membership, the API key and the expiry, with a key derived from `ZS_KILLBILL_WEBHOOK_SECRET`. Core stores nothing for it. Removing the member, revoking the key or removing `billing:manage` ends it on the next request.

The account-scoping rule accepts these requests and refuses every other with `404 not_found`:

| Request | Condition |
| --- | --- |
| `GET 1.0/kb/accounts?externalKey=<tenant ID>` | The external key is the tenant's own. Only `externalKey`, `accountWithBalance`, `accountWithBalanceAndCBA` and `audit` are accepted in the query. |
| `GET 1.0/kb/accounts/<account ID>` and any path below it | The account is the tenant's own. |
| `POST 1.0/kb/accounts/<account ID>/paymentMethods` | The account is the tenant's own, the body names the brand's plugin, and has only `pluginName`, `externalKey`, `pluginInfo` and `isDefault`. Only `isDefault` is accepted in the query. |
| `POST 1.0/kb/subscriptions` | The body is one JSON object for the tenant's own account with only `accountId`, `planName`, `externalKey`, `bundleExternalKey`, `productName`, `productCategory`, `billingPeriod` and `priceList`, without a query. A `bundleExternalKey` is absent, null or the external key of one of the tenant's own bundles, as Kill Bill reports them. Kill Bill adds the subscription to the bundle the key names, so any other key could reach another account. |

So the proxy never writes blocking states, payments, refunds, credits, usage or price overrides, never backdates a subscription, and never reaches another account. Each write through the proxy is audited as `billing.proxy.write` with the method, the Kill Bill path, the status and the ID of the object Kill Bill made. The body is never recorded. A path with an empty, `.` or `..` segment, or a character other than letters, digits and `._~/-`, is refused. A repeated JSON key is refused, so the field Core checks is the field Kill Bill reads.

## Test mode only

No payment plugin takes real money in this release.

- `mode` is required on each plugin of `billing.payment_plugins`, on `billing.payment_plugin`, its one-plugin form, and on `billing.invoice_plugin`. `test` is the only accepted value. `live` is refused with `400 brand_manifest_invalid`: "must be test, because v1 refuses live and no payment plugin may take real money".
- `billing.payment_plugins` has exactly one plugin with `default: true`. It is the plugin of the brand's payment methods and of the billing proxy. A plugin listed twice, or both forms at once, is `400 brand_manifest_invalid`.
- A payment plugin must be in `approved_payment_plugins` of `components.yaml`, and an invoice plugin in `approved_invoice_plugins`. A plugin with status `no_java_artifact`, such as `paypal-express`, or any other plugin, is `422 plugin_unavailable`. A plugin with no test-mode rule is also `422 plugin_unavailable`. A manifest `version` other than the pin is refused.
- `config_key` is empty or `killbill-<name>`. Kill Bill hands a plugin the configuration stored under its own service name. Any other key would upload a configuration that another plugin reads, so it is `400 brand_manifest_invalid`.
- Core reads the configuration as Kill Bill does, as a Java properties text read by Java 11, Kill Bill's runtime. `pkg/brand/testdata/java_properties.json` records how Kill Bill's own JVM reads a set of texts, and the parser must match it.
- Core also refuses parts of the format that a reader other than Java's could misread. A configuration with a `\u` escape, a carriage return that does not end a line, a line that ends with a continuation backslash, or a byte that is not ASCII is `422 brand_apply_rejected`. The message names the rule.
- Every known plugin's rule runs against every configuration, whatever the plugin. A Stripe key that does not start with `sk_test_` is refused under any plugin, and so is an Adyen environment other than `TEST` or an AvaTax endpoint other than the sandbox.
- Stripe: the configuration must set `org.killbill.billing.plugin.stripe.apiKey`, and every value it gets must start with `sk_test_`. A configuration without the key is refused, because Core cannot see a key the Kill Bill node takes from its environment.
- Adyen: the configuration must set `org.killbill.billing.plugin.adyen.enviroment`, as the plugin 0.10.0 spells it, to `TEST`. Unset is refused. `org.killbill.billing.plugin.adyen.environment` must be `TEST` too when it is set.
- AvaTax: the configuration must set `org.killbill.billing.plugin.avatax.url` to an `https` URL on `sandbox-rest.avatax.com`, the sandbox the plugin's documentation names. Every value of it and of `org.killbill.billing.plugin.avatax.taxratesapi.url` must be on that host.
- A refused Stripe key, Adyen environment or AvaTax endpoint is `422 brand_apply_rejected`.

Apply checks the configuration before anything changes, and its error never holds a configuration value. A change of a plugin, its version, mode, default, configuration key or reference, or of any plugin configuration in Kill Bill, is a plugin change and needs a reason. Over HTTP it also needs recent MFA. The Adyen plugin also reads an `ADYEN_ENVIROMENT` environment variable on the Kill Bill node. Keep it unset.

## Plugin install

When the manifest names an approved plugin, apply makes sure the pinned version runs on every Kill Bill node, and uploads the plugin's per-tenant configuration when it differs. Every payment plugin, the invoice plugin and, for `billing.email`, the email-notifications plugin go through the same path. Another installed version is refused with `422 brand_apply_rejected`, and the upgrade runbook applies.

New KPM installs require Core's verified plugin mirror, configured with `ZS_PLUGIN_MIRROR_URL` and `ZS_PLUGIN_MIRROR_SECRET`. Core checks each mirror response against the SHA-256 in `components.yaml`, then the mirror serves KPM a fresh verified snapshot. A plugin's own tables are a deployment step, `zs-core plugin ddl apply`. Brand apply on the command line with `ZS_KILLBILL_DATABASE_URL` refuses a plugin whose tables are missing with `422 plugin_tables_missing`. See [plugins.md](plugins.md).

## Push notifications

With `ZS_KILLBILL_WEBHOOK_SECRET` and `ZS_KILLBILL_CALLBACK_URL` set, apply registers each brand's callback on its Kill Bill tenant: `<ZS_KILLBILL_CALLBACK_URL>/v1/billing/webhooks/killbill/<brand ID>/<token>`. The token is the HMAC-SHA256 of the brand ID under the secret, in hex. A brand's Kill Bill tenant only ever sees its own token, so it cannot post as another brand. The diff shows the callback as a redacted field. Kill Bill keeps one callback per tenant, so registering replaces any other. Kill Bill cannot add headers to its callbacks, which is why the token is in the path. Core logs routes by their pattern, never the path itself.

Kill Bill 0.24.22 posts `{"eventType", "accountId", "objectType", "objectId", "metaData"}` and nothing else. The notification carries no event ID, and a retry sends the same body. The metadata of a payment event names its transaction, so two transactions of one payment have different bodies. Core handles a notification like this:

1. A wrong brand or token is `404`.
2. For a tenant's account, Core reads the overdue state from Kill Bill. For a subscription event it reads the subscription's state, plan, phase and cancellation date instead. That is the observed state.
3. The receipt key is the SHA-256 of the body and the observed state, per brand, in `killbill_notifications`. A receipt with the same key is a retry. It is acknowledged and does nothing.
4. A notification whose observed state differs from the last state recorded for its object is never dropped, even when an earlier receipt has its key. That happens when a state changes and changes back.
5. A notification without an account, or for an account outside the brand, is recorded, acknowledged and ignored.
6. For a tenant's account, Core refreshes `tenant_billing_standing` with the overdue state and the event type.
7. Core writes the Core event to the outbox in the same transaction as the receipt.

Receipts are kept seven days. Each credit sweeper pass deletes older ones.

| Kill Bill event | Core event |
| --- | --- |
| `INVOICE_CREATION` | `billing.invoice.created` |
| `PAYMENT_SUCCESS` | `billing.payment.succeeded`, with the transaction type and ID |
| `PAYMENT_FAILED` | `billing.payment.failed`, with the transaction type and ID |
| `OVERDUE_CHANGE` | `billing.overdue.changed`, with the overdue state |
| `SUBSCRIPTION_CREATION`, `_PHASE`, `_CHANGE`, `_CANCEL`, `_UNCANCEL`, `_BCD_CHANGE` | `billing.subscription.changed` |

Kill Bill sends a `PAYMENT_*` event for every payment transaction, a purchase and a refund alike, and its metadata names the transaction. It also sends `INVOICE_PAYMENT_*` when a payment pays an invoice. Those refresh the standing and make no second Core event. Each event names the brand, the tenant and the Kill Bill object, with the provider actor `killbill`. When Core cannot reach Kill Bill or its database, it answers `503` and records nothing, so Kill Bill retries. The route answers `204` otherwise. Delivering these events to products as signed webhooks is Phase 5.

## Audit

| Action | When |
| --- | --- |
| `billing.credit_code.created`, `billing.credit_code.disabled` | Brand events. The details hold the code ID and its first four characters. |
| `billing.credit.redeemed` | A redemption, with the code ID, the amount and the Kill Bill credit ID. |
| `billing.credit.granted` | A staff credit, with its reason and expiry. |
| `billing.credit.expired` | The sweeper, with the part reversed and the Kill Bill charge ID. |
| `billing.credit.expired_nothing_to_reverse` | The sweeper, for an expired credit with nothing left to reverse. |
| `billing.subscription.created`, `billing.subscription.plan_changed`, `billing.subscription.cancelled` | A subscription write, with the Kill Bill account, subscription and bundle IDs, the plan and the policy. |
| `billing.payment.refunded` | A refund, with the Kill Bill payment and transaction IDs and the amount. |
| `billing.payment_method.added`, `billing.payment_method.default_set`, `billing.payment_method.deleted` | A payment method write, with the Kill Bill account and payment method IDs. |
| `billing.credential.issued` | A billing credential, with the Kill Bill account ID. |
| `billing.proxy.write` | A write through the proxy, with the method, the Kill Bill path, the status and the Kill Bill object ID. |
| `brand.killbill.plugin.*`, `brand.killbill.notification_callback.*` | Brand apply. |

Every write Core makes in Kill Bill is audited once Kill Bill has accepted it. An idempotent replay writes no second event. The details hold Kill Bill IDs, plan names and amounts only. They never hold plugin properties, payment method references or card data.

Every Kill Bill write also carries `X-Killbill-CreatedBy` (the Core principal, such as `user:idn_...`, `staff:sub:...` or `zs-core` for the sweeper), `X-Killbill-Reason` and `X-Killbill-Comment` (the request ID), so Kill Bill's audit log matches Core's.

## Known limits

- Credit expiry assumes Core credits are used oldest first and other credit last. Kill Bill does not report usage per credit.
- A tenant's expired credits wait while it has a pending credit. A pending credit completes only when its request is retried.
- New plugin installs require Core's verified mirror URL and secret. Without them, apply refuses the install before making changes. See [plugins.md](plugins.md#the-mirror).
- The proxy accepts the small set of requests above. A product that needs more calls Core's routes or asks for a wider rule.
- Invoice numbers come from one Kill Bill sequence for the whole server. They have gaps within a brand. See [invoices.md](invoices.md#invoice-numbers).
- Plugin tables are a deployment step, `zs-core plugin ddl apply`. `serve` never checks them, doctor does.
- `billing.*` events remain in the existing outbox; outbound delivery is deferred for this candidate.
