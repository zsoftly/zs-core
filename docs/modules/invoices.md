# Invoices and billing email

## TL;DR

Kill Bill renders every invoice. Core never builds one. A brand manifest names its invoice template, its manual pay template and a translation per locale, and brand apply uploads them to the brand's Kill Bill tenant when they differ from what Kill Bill holds. Apply adds the legal seller of the manifest, its name, address and tax registration numbers, to every translation it uploads. `GET /v1/tenants/{id}/invoices/{inv}/html` returns Kill Bill's rendering in the locale of the tenant's Kill Bill account. Core sets that locale when it creates the account, from the creator's locale or the brand's default. `GET .../pdf` sends that HTML through the optional PDF renderer, the pinned Gotenberg, and streams the PDF. Billing email goes through the email-notifications plugin, with each brand's sender, SMTP server and templates per locale. Kill Bill numbers invoices with one sequence for the whole server, so numbers are unique and increasing within a brand but not free of gaps.

## The manifest

```yaml
billing:
  seller:
    name: ZSoftly Technologies Inc.
    country: CA
    address_lines: ["100 Example Street, Suite 1", "Ottawa, Ontario"]
    tax_registrations: ["GST/HST 000000000 RT0001"]
  invoice_templates:
    template: zs-reference/invoices/invoice.html
    manual_pay_template: zs-reference/invoices/invoice-manual-pay.html
    translations:
      en: zs-reference/invoices/translation-en.properties
      fr: zs-reference/invoices/translation-fr.properties
    catalogue_translations:
      en: zs-reference/invoices/catalogue-en.properties
      fr: zs-reference/invoices/catalogue-fr.properties
  email:
    sender: billing@zs-reference.example.test
    events: [INVOICE_CREATION, INVOICE_PAYMENT_SUCCESS, INVOICE_PAYMENT_FAILED]
    smtp:
      host: mailpit
      port: 1025
    templates: zs-reference/email
```

- `seller.address` is the one-line form of `address_lines`. Set one of the two.
- Every locale of `translations` and `catalogue_translations` must be a locale of `identity.locales`.
- Files are UTF-8 text inside the manifest's directory. Over HTTP they must start with `<slug>/`.

## What apply uploads

| Manifest | Kill Bill API | Tenant key |
| --- | --- | --- |
| `template` | `POST /1.0/kb/invoices/template?deleteIfExists=true` | `INVOICE_TEMPLATE` |
| `manual_pay_template` | `POST /1.0/kb/invoices/manualPayTemplate?deleteIfExists=true` | `INVOICE_MP_TEMPLATE` |
| `translations` and `seller` | `POST /1.0/kb/invoices/translation/{locale}?deleteIfExists=true` | `INVOICE_TRANSLATION_<locale>` |
| `catalogue_translations` | `POST /1.0/kb/invoices/catalogTranslation/{locale}?deleteIfExists=true` | `CATALOG_TRANSLATION_<locale>` |

Apply reads each value back through `GET /1.0/kb/tenants/userKeyValue/{key}` and uploads when its SHA-256 differs from the text Core would upload. The diff shows the digest before and after, under targets `killbill.invoice_template`, `killbill.invoice_translation` and `killbill.catalogue_translation`, and audit records them. A second apply of the same manifest is empty. Kill Bill 0.24.22 answers `400` to a second upload without `deleteIfExists=true`, which is why Core always sends it.

Every locale of the brand gets a translation, even one the manifest gives no file for. Apply appends a block written from `billing.seller` to the file's text:

| Key | Value |
| --- | --- |
| `sellerName`, `companyName` | `seller.name` |
| `sellerAddress` | the address lines joined with `, ` |
| `sellerAddressLine1`, `...2` | each address line |
| `companyAddress`, `companyCityProvincePostalCode` | the first line, the others joined |
| `sellerCountry`, `companyCountry` | `seller.country` |
| `sellerTaxRegistrations` | the registrations joined with `; ` |
| `sellerTaxRegistration1`, `...2` | each registration |

The manifest wins over the same keys in a file. The `company` keys are the ones Kill Bill's own template and the email plugin's templates read.

Brand locales map to Kill Bill locales: `fr` stays `fr`, `fr-CA` becomes `fr_CA`. Kill Bill 0.24.22 falls back from `fr_FR` to the translation of `fr`.

## Writing a template

Kill Bill renders the template with jmustache. It passes three objects:

- `invoice`: `invoiceNumber`, `formattedInvoiceDate`, `currency`, `formattedChargedAmount`, `formattedPaidAmount`, `formattedBalance`, and `invoiceItems`, each with `invoiceItemType`, `description`, `prettyPlanName`, `formattedStartDate`, `formattedEndDate` and `formattedAmount`.
- `account`: `name`, `companyName`, `address1`, `city`, `stateOrProvince`, `postalCode` and `country`.
- `text`: the translation. Kill Bill exposes its own keys as fields, such as `{{text.invoiceTitle}}`, `{{text.invoiceDate}}`, `{{text.companyName}}` and `{{text.invoiceBalance}}`. Any other key is read through a section, `{{#text.properties}}sellerTaxRegistrations{{/text.properties}}`. A field Kill Bill does not know, such as `{{text.sellerName}}`, fails the rendering with HTTP 500.

Tax lines are invoice items of type `TAX`, added by an invoice plugin such as AvaTax or through `POST /1.0/kb/invoices/taxes/{accountId}`. The reference template lists every item with its type, description and amount, so each tax line shows with its own text. An empty `prettyPlanName` is an empty string, not a missing value, so a section on it always renders. The reference template prints the description and the plan name on two lines instead.

`brands/zs-reference/invoices/` holds the reference template in EN and FR: the brand name, the seller's name, address and registration number, the invoice number, date and currency, each item with its type, the totals and the balance.

## Rendering

`GET /v1/tenants/{id}/invoices/{inv}/html` needs `billing:read`. Core checks that the invoice belongs to the tenant's Kill Bill account, then returns `GET /1.0/kb/invoices/{id}/html` from Kill Bill. The answer has `Content-Type: text/html; charset=utf-8`, `Cache-Control: no-store` and a `Content-Security-Policy` that allows no script, no fetch, no form and no framing. Kill Bill escapes the account's and the invoice's values in the page.

The locale is the locale of the Kill Bill account. When Core creates a tenant, it sets the account's locale to the creator's locale from the token's `locale` claim when the brand offers it, otherwise to the brand's default locale. It seeds the account's primary email from the creator's verified email. After provisioning, the [recipient reconciler](tenants.md#billing-recipient-projection) maintains that primary email from current eligible ownership rather than immutable creator provenance. The locale is kept with the creation, so the sweeper creates the account with the same locale. An account created before this release keeps whatever locale Kill Bill holds.

`GET .../pdf` renders that HTML through the PDF renderer of `ZS_PDF_RENDERER_URL`. Core posts it as one `index.html` file to Gotenberg's `/forms/chromium/convert/html` route, reads the complete answer within 64 MiB, and checks the `%PDF-` header before returning it. The response uses `Content-Type: application/pdf` and `Cache-Control: no-store`. An oversized or interrupted renderer response answers `503 pdf_renderer_unavailable` before any PDF bytes are sent. Without a renderer an invoice of the tenant answers `404 invoice_pdf_unavailable`. A renderer that fails answers `503 pdf_renderer_unavailable`. An invoice of another tenant answers `404 not_found` on both routes.

## The PDF renderer

`components.yaml` pins Gotenberg 8.38.1 by digest as an optional component. Its licence is MIT. The reference stack configures it on loopback port 3000 with these controls:

- JavaScript is off.
- Chromium's HTTP, HTTPS, WS and WSS URL checks reject destinations resolving to public or non-public IP addresses.
- Local file access is restricted to the current request's temporary directory.
- The LibreOffice routes, webhooks and remote downloads are off.

These Chromium URL controls do not establish container-wide egress isolation. A template that links an image or a font over HTTP or HTTPS renders without it. Embed images as `data:` URLs. Doctor checks the renderer's `/version` against the pin when `ZS_PDF_RENDERER_URL` is set.

## Invoice numbers

Kill Bill 0.24.22 gives each invoice the next number of one sequence for the whole server, across every Kill Bill tenant. Within a brand, numbers are unique and increase, and invoices of other brands leave gaps. The 2026-10-20 Phase 5a rehearsal recorded `14, 16, 18` for one brand and `15, 17, 19` for another. These historical results do not establish stack acceptance of the current takeover revisions, which require a new run. A brand that must number its invoices without gaps needs its own Kill Bill server. Core does not renumber invoices, because Kill Bill is the authority for them.

## Email

`billing.email` makes apply install the pinned email-notifications plugin, through the same path as the payment plugins, and upload its per-tenant configuration under `killbill-email-notifications`:

| Property, under `org.killbill.billing.plugin.email-notifications.` | From |
| --- | --- |
| `defaultEvents` | `email.events`, `INVOICE_CREATION` when absent |
| `smtp.host`, `smtp.port`, `smtp.useSSL` | `email.smtp` |
| `smtp.useAuthentication`, `smtp.userName`, `smtp.password` | `email.smtp.username_ref` and `password_ref`, secret references |
| `smtp.defaultSender` | `email.sender` |
| `smtp.sendHTMLEmail` | `true` |

These are the keys the plugin 0.8.1 reads. Core escapes every value for a Java properties text. The diff shows the configuration redacted. The prior rehearsal observed a stale configuration after upload, but the pinned plugin source includes configuration-change reload handling, so the cause is unknown. After a configuration change, Core conservatively sends `STOP_PLUGIN`, waits for every original target to report the pinned plugin `STOPPED`, sends `START_PLUGIN`, then waits for that same target set to report the pinned plugin `RUNNING`. A missing target or either failed observation leaves the marker pending and makes apply fail for retry. Delivery during restart is unverified; do not rely on replay. The approved stack acceptance must verify this stop/start sequence before release. See [../upgrades/killbill.md](../upgrades/killbill.md#email-notifications-configuration-restart). The events the plugin sends email for are `INVOICE_CREATION`, `INVOICE_NOTIFICATION`, `INVOICE_PAYMENT_SUCCESS`, `INVOICE_PAYMENT_FAILED` and `SUBSCRIPTION_CANCEL`. The plugin reads the account's primary email for each handled event. Core initially seeds that email at creation and then reconciles it to current eligible ownership. A successful pass compares actual scoped account data, writes only a difference and verifies readback. No eligible owner means an empty primary email. The pinned SMTP sender rejects blank To before sending; this differs from the listener's null-primary skip branch. Its SES path bypasses the SMTP validation, so clearing primary email is not proof of universal no-send behavior. Additional account emails are separate unmanaged CC recipients. Doctor warns when the scoped collection is nonempty, naming only the account ID. A failed or invalid collection read is an inspection failure. Core never writes or deletes those additional addresses. Existing queued, materialized or sent messages are not recalled, and a delayed stale write can persist until a later pass repairs it. Delivery status is a projection observation, not a delivery receipt. Closure behavior is unchanged and a final invoice on closure remains undecided.

Apply records a tenant-scoped configuration digest only after the restart succeeds. A failed restart remains an apply failure, and a later apply retries it even when the uploaded configuration already matches. If a command was accepted but times out or its completion is uncertain, an operator must reconcile the engine and plugin state before that retry. Serialization of Core applies sharing the Core database does not fence external writers, writers using another Core database, or late accepted commands. The first apply of an existing email configuration without this marker also restarts the plugin to establish it.

Core captures the target node set before `STOP_PLUGIN`. It must observe every original target's pinned plugin `STOPPED` before `START_PLUGIN`, then observe that same target set `RUNNING` before recording the digest. A changed node set or an unobserved state fails the apply and leaves the marker pending for retry.

The templates directory holds one directory per locale with the plugin's file names: `InvoiceCreation.mustache`, `UpcomingInvoice.mustache`, `SuccessfulPayment.mustache`, `FailedPayment.mustache`, `PaymentRefund.mustache`, `SubscriptionCancellationRequested.mustache`, `SubscriptionCancellationEffective.mustache` and `Translation.properties`. Each file is optional. Apply uploads each one it finds as a tenant user key:

| File | Tenant key for locale `en` | For `fr-CA` |
| --- | --- | --- |
| `InvoiceCreation.mustache` | `killbill-email-notifications:INVOICE_CREATION_en_` | `..._fr_CA` |
| `Translation.properties` | `killbill-email-notifications:TEMPLATE_TRANSLATION_en_` | `..._fr_CA` |

The plugin 0.8.1 builds the suffix from Java's locale as `_<language>_<country>`, with an empty country for `en`. The trailing underscore is part of the key. A translation replaces all of the plugin's own text for that locale, so it must hold every key the plugin's templates read. Apply adds the seller block to a translation the brand provides, and never uploads a translation the brand does not provide. The email plugin's `text` is a map, so a template reads any key as `{{text.key}}`. The subject is the translation's `<type>Subject` key, such as `invoiceCreationSubject`.

Changing the SMTP settings is a sensitive change over HTTP, like a payment plugin change, and every plugin configuration change needs a reason.

## Tests

The 2026-10-20 Phase 5a rehearsal passed the invoice-rendering and brand-isolation stack tests below. Current takeover revisions require a new stack acceptance run.

- `pkg/brand`: manifest rules, the seller block, email keys and the email configuration, which the Java properties parser reads back exactly.
- `internal/pdfrender`: the request Core sends and the PDF check.
- `TestInvoicesOnTheStack`: the template and translations in Kill Bill, an EN and an FR tenant, an invoice with a TAX item rendered as HTML and as PDF in each locale, and the invoice numbers.
- `TestBrandIsolationOnTheStack`: two brands with their own templates, sellers and senders, and Mailpit receiving each brand's invoice email from its own sender with its own template.
