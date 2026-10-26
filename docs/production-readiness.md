# Production readiness

Status: draft. Recorded at commit ecbc65a (the head of pull request 27). Update the Status and Evidence columns in the pull request that changes them.

## TL;DR

This page lists what Core must prove before a product bills real customers through it. Each row has an acceptance test that anyone can rerun, the current status and the evidence for that status. Status is Done, Partial or Open, and a row is Done only when its test has passed on a stack that matches the release. Today the billing engine, the wallet, metering and the audit trail are well covered by integration tests against a fake Kill Bill and a real Kill Bill for the happy path. No real payment has gone through the Stripe plugin, no tax plugin is installed, and there is no monitoring, on-call or lost-answer runbook yet. A release that bills real money needs every row Done, or an explicit written risk acceptance with an owner and a date. Rows are numbered so a release note can cite them.

## How to read a row

| Column | Meaning |
| --- | --- |
| Acceptance test | A procedure with a pass condition. It names the inputs and the expected result. |
| Status | Done: the test passed on the release stack. Partial: some of it passed, or it passed only against a fake. Open: not run or no code yet. |
| Evidence | A file, a test name or a document that shows the status. For Open rows it names the gap. |

A fake Kill Bill proves Core's own logic. It does not prove Kill Bill, a payment plugin or a tax service. Rows that depend on those need a run on the reference stack.

## 1. Payments

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| P1 | The pinned Stripe plugin installs and starts on a test brand from Core's verified mirror. | Done | `components.yaml` marks Stripe 8.0.3 `verified_install`. |
| P2 | In Stripe test mode, a tenant adds a card through the hosted payment page. An invoice is paid from it. Kill Bill shows a `SUCCESS` payment, and Core emits one `billing.payment.succeeded`. | Open | `components.yaml` notes "no live payment exercised yet". The hosted form route and the event mapping exist (`pkg/billing/payments.go`, `pkg/billing/webhook.go`) and are tested against the fake. |
| P3 | A declined card (test card for decline) leaves the invoice unpaid, Kill Bill records `PAYMENT_FAILED`, Core emits one `billing.payment.failed`, and the overdue state advances on the schedule in the brand's `overdue.xml`. | Open | Event mapping covered by `TestNotifications` and `TestNotificationDedupFollowsKillBillState` against the fake only. |
| P4 | A card that needs 3-D Secure (test card for authentication) ends in a documented state: the customer can complete authentication and the invoice is paid, or the payment is marked failed with a reason Core exposes. No payment is recorded twice. | Open | No code or document handles the authentication step. The plugin behaviour has not been read or run. |
| P5 | A staff refund of a paid invoice, partial and full, reaches Stripe and Kill Bill. A refund above the payment is refused. A retry with the same `idempotency_key` creates no second refund. | Partial | `TestPaymentsRefundsAndPaymentMethods`, `TestRefundCannotExceedThePayment` against the fake. Not run against Stripe. |
| P6 | A dispute opened in Stripe test mode appears in Kill Bill as a chargeback or a failed transaction, the tenant's standing changes as the brand's policy says, and an audit event and a Core event record it. | Open | No code or document mentions disputes. The plugin's behaviour is unknown. |
| P7 | A payment method can be added, made default and removed. The last method cannot be removed while a subscription needs it. | Partial | Payment method routes exist and are tested against the fake. Not run against Stripe. |
| P8 | Stopping Core or Kill Bill in the middle of a payment never produces a duplicate charge. After recovery the payment count equals the invoice count that was due. | Open | Not exercised. Needs a fault drill on the reference stack. |

## 2. Canadian taxes

Core does not compute tax. Kill Bill invoice plugins add tax items when an invoice is generated. A brand names one in `billing.invoice_plugin`.

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| T1 | An approved invoice plugin installs and starts on a test brand, with its tables created by `zs-core plugin ddl apply` and checked by `zs-core doctor`. | Open | `components.yaml` lists `avatax` 0.9.3 as `pinned_not_installed`. The manifest comment says a simple-tax plugin is unimplemented. |
| T2 | Invoices for tenants whose billing address is in each of Ontario, Quebec, British Columbia, Alberta and an Atlantic province show the tax items the authorities publish on the test date: GST, HST or GST plus QST as that province requires. Each tax amount equals the rate times the taxable base, rounded as the plugin documents. | Open | No tax plugin installed. |
| T3 | An invoice to a buyer outside Canada carries no Canadian tax. A tax-exempt tenant carries none. | Open | Needs the tax plugin and a way to mark a tenant exempt. |
| T4 | A credit or refund of a taxed invoice reverses the tax in proportion. | Open | Credit adjustments exist, tax reversal is untested. |
| T5 | The seller's tax registration number prints on every invoice and in the PDF. | Done | `billing.seller.tax_registrations` in the manifest, `docs/modules/invoices.md`, the reference templates under `brands/zs-reference/invoices/`. |
| T6 | A tax service outage never blocks invoice generation silently. The invoice is generated or held, an alert fires and the run can be repeated. | Open | Not exercised. |

## 3. Invoicing

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| I1 | Invoice numbers of one seller are unique and increase. The jurisdiction's rule on gaps is decided and recorded. If gap-free numbers are required, a test creates invoices in two brands in turn and shows no gap in either. | Partial | Numbers are unique and increasing within a brand, but Kill Bill uses one sequence for the whole server, so a brand sees gaps (`docs/modules/invoices.md`, "Invoice numbers"). |
| I2 | The PDF of an invoice renders in English and French. Its seller block, buyer block, lines, tax lines and totals equal the Kill Bill invoice to the minor unit. | Partial | The optional renderer route and the templates exist (`internal/pdfrender`, `components.yaml` Gotenberg). No test compares PDF text with the invoice amounts. |
| I3 | A credit given to a customer produces a document the customer can keep: numbered, dated, naming the invoice it relates to, with tax reversed. Or the decision that a credit adjustment line on the invoice is enough is recorded for each jurisdiction served. | Open | Kill Bill records a credit as a `CREDIT_ADJ` item. Core renders no separate credit note. |
| I4 | A new invoice, a successful payment and a failed payment each send one email in the tenant's locale, using the brand's templates. | Partial | The email plugin is verified on the reference stack. Delivery with a real SMTP relay is untested. |
| I5 | The total of all invoices of a period equals the usage and recurring charges the catalogue and the recorded usage imply, for a sample of tenants chosen by the release owner. | Open | Wallet reconciliation compares usage per tracking ID (`docs/modules/wallet.md`). No billing-run acceptance on live-like data. |

## 4. Reliability

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| R1 | The merge gate (`make check`) and the full integration suite pass on the exact commit being released, in a clean checkout, with the commit ID and the results recorded in the pull request. No hosted CI is assumed. | Partial | `make check` is the gate (`Makefile`, `docs/development.md`). Results on the head of pull request 27 are not recorded on this page yet. |
| R2 | A backup of every database (Core, Kill Bill and Keycloak) restores into an empty stack. After the restore, `zs-core doctor` is green, wallet reconciliation shows no new finding, and the invoice count and each tenant's balance equal the values before the backup. The backup and restore times are written down as the recovery point and recovery time. | Partial | `deploy/compose/rehearsal/backup.sh` dumps the Keycloak and Kill Bill databases and proves only the Keycloak restore. The Core database is not in the script and no restore of Kill Bill is proven. |
| R3 | Each component upgrade has a rehearsal row for the exact step, and a rollback test restores the previous version from backup. | Partial | Rows exist for Kill Bill 0.24.21 to 0.24.22 and Keycloak 26.8.0 to 26.8.1 (`docs/upgrades/`). The Kill Bill page says the current revisions need a new run. Both runbooks describe a rollback (`docs/upgrades/killbill.md`, "Roll back"; `docs/upgrades/keycloak.md`), but no rehearsal row records a rollback run. |
| R4 | Lost-answer drill. Drop the connection after Kill Bill accepted a usage request. Staff follow the runbook. At the end Kill Bill and Core agree, no `unknown_usage` finding remains, and the usage was billed once. | Open | The behaviour is documented (`docs/modules/metering.md`, "Forwarding"; `docs/modules/wallet.md`, "Known limits"). There is no step-by-step runbook, and the staff routes only reassign or discard rejected rollups. |
| R5 | Two Core instances run for a day with a steady usage load. No tenant gets two sends of the same event set and no rollup stays unconfirmed without an alert. | Partial | `TestTwoForwardersNeverSendTwice` and the claim and sweep tests in `pkg/metering`. No soak run on the reference stack. |
| R6 | Migrations apply forward on a copy of a database from the previous release, and a damaged migration history is refused before any SQL runs. | Done | Forward-only migrations with history validation; see `migrations/` and the migration runner tests. |
| R7 | Kill Bill unavailable for an hour: reads fail closed with `503`, writes are refused or queued with a visible state, and when it returns the queues drain without duplicates. | Partial | Fail-closed paths and queued settlements are tested against the fake. No hour-long outage drill. |

## 5. Operations

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| O1 | A runbook covers the Kill Bill stack: sizing, JVM settings, plugin install through the mirror, the plugin tables step, catalogue versioning, one Kill Bill tenant per legal seller, the credential rotation steps and the upgrade steps. A new operator follows it on a clean host and reaches a stack that passes `zs-core doctor`. | Partial | Parts are in `docs/modules/plugins.md`, `docs/upgrades/killbill.md` and `deploy/compose/README.md`. There is no single runbook and no sizing guidance. |
| O2 | Metrics and alerts exist for: Kill Bill and Keycloak reachability, outbox and webhook backlog, metering backlog and unconfirmed rollups, wallet reconciliation findings, overdue tenants, and error rates. Each alert is fired once in a drill and reaches a person. | Open | Tracing exists (`internal/httpapi/middleware.go`), plus `/healthz`, `/readyz` and `zs-core doctor`. There is no metrics endpoint and there are no alert rules. |
| O3 | Named on-call roles, severities, response times and escalation exist, with a link from every alert to a runbook. A drill from alert to resolution is timed. | Open | Not started. |
| O4 | A Kill Bill tenant API secret can be rotated with no lost request and no downtime, and the old secret stops working. | Open | Not assessed. |
| O5 | Staff and API key actions on billing are all in the audit log with actor, tenant and outcome, and the log can be exported for an auditor. | Partial | `TestEveryBillingWriteIsAudited`, `docs/modules/audit.md`. No export procedure. |

## 6. API and SDK versioning

| ID | Acceptance test | Status | Evidence |
| --- | --- | --- | --- |
| A1 | `api/openapi.yaml` is the contract and the generated Go types match it. `make check` fails when they differ. | Done | `generate-check` in `Makefile`. |
| A2 | A breaking change to the contract fails a check unless the major version changes. | Open | `docs/releases.md` defines release types, but no tool compares two contract versions. |
| A3 | A published deprecation policy states how long a deprecated route or field lives and how clients learn about it. A deprecated route answers with a header that says so. | Open | Not written. |
| A4 | A TypeScript SDK and a Go client are generated from the contract and tested against the reference stack, and they are versioned with the release. | Open | `docs/architecture/v1-scope.md` lists them in Phase 6. `api/` holds only the contract and schemas. |
| A5 | Outbound webhooks are signed, retried from an outbox, and a sample consumer verifies the signature and tolerates a replay. | Done | `docs/architecture/v1-scope.md` conventions, `examples/webhook-client`, the webhook delivery tests. |
| A6 | Every write route accepts an `idempotency_key`, and a repeat with the same key and body returns the first answer. A repeat with a different body returns `409 idempotency_key_conflict`. | Done | `pkg/idempotency` tests and the route tests. |

## Release rule

A release that charges real customers needs every row above Done, or a written acceptance of the open row signed off by the release owner with a date to close it. The rows most likely to hurt a customer are P2 to P4, P6, T2, I1, R2, R4 and O2. Close those first.

## Order of work

1. Run P2 to P5 on a test brand with Stripe test mode. Their results decide the shape of P4, P6 and P8.
2. Install the tax plugin on the reference stack and run T1 to T4. Decide I1 and I3 with the jurisdiction's rules in hand.
3. Extend the backup script to all three databases and run R2, then write the lost-answer runbook and run R4.
4. Add the metrics endpoint, the alert rules and the on-call roles (O2, O3).
5. Add the contract compatibility check and the deprecation policy (A2, A3), then generate the clients (A4).
