-- The locale of a tenant's Kill Bill account. Kill Bill renders invoices and
-- the email-notifications plugin writes email in the account's locale. Core
-- sets it when it creates the account: the creator's locale when the brand
-- offers it, otherwise the brand's default locale. It is kept with the
-- creation so the sweeper creates the account with the same locale. See
-- docs/modules/invoices.md.
ALTER TABLE tenant_provisioning ADD COLUMN billing_locale text;
ALTER TABLE tenant_provisioning ADD CONSTRAINT tenant_provisioning_billing_locale
    CHECK (billing_locale IS NULL OR billing_locale ~ '^[a-z]{2}(_[A-Z]{2})?$');
