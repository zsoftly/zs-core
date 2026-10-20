-- Billing surface on Kill Bill. Kill Bill is the financial authority: it
-- holds every balance, credit, invoice and payment. These tables hold only
-- what Kill Bill does not: promo and voucher codes and their redemptions, the
-- expiry of credits Core issued, a cache of each tenant's billing standing,
-- the receipts of Kill Bill push notifications, and the plugins Core
-- installed. Every amount here names the Kill Bill object it produced. See
-- docs/modules/billing.md.

-- Promo and voucher codes. Only a digest of the code is stored, keyed by the
-- brand, so the same code in two brands has two digests. hint is the first
-- characters of the code, for operators.
CREATE TABLE credit_codes (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    code_digest bytea NOT NULL,
    hint text NOT NULL,
    amount_minor bigint NOT NULL,
    currency text NOT NULL,
    max_redemptions integer,
    per_tenant_limit integer NOT NULL DEFAULT 1,
    redemptions integer NOT NULL DEFAULT 0,
    expires_at timestamptz,
    credit_valid_days integer,
    reason text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    created_by text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    disabled_at timestamptz,
    CONSTRAINT credit_codes_digest_length CHECK (octet_length(code_digest) = 32),
    CONSTRAINT credit_codes_hint_length CHECK (char_length(hint) BETWEEN 1 AND 8),
    CONSTRAINT credit_codes_amount CHECK (amount_minor > 0),
    CONSTRAINT credit_codes_currency CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT credit_codes_max_redemptions CHECK (max_redemptions IS NULL OR max_redemptions > 0),
    CONSTRAINT credit_codes_per_tenant_limit CHECK (per_tenant_limit > 0),
    CONSTRAINT credit_codes_redemptions CHECK (redemptions >= 0 AND (max_redemptions IS NULL OR redemptions <= max_redemptions)),
    CONSTRAINT credit_codes_credit_valid_days CHECK (credit_valid_days IS NULL OR credit_valid_days BETWEEN 1 AND 3660),
    CONSTRAINT credit_codes_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000),
    CONSTRAINT credit_codes_status CHECK (status IN ('active', 'disabled')),
    CONSTRAINT credit_codes_created_by_length CHECK (char_length(created_by) BETWEEN 1 AND 255)
);

CREATE UNIQUE INDEX credit_codes_brand_digest_key ON credit_codes (brand_id, code_digest);
CREATE UNIQUE INDEX credit_codes_brand_id_key ON credit_codes (brand_id, id);
CREATE INDEX credit_codes_brand_created_idx ON credit_codes (brand_id, created_at DESC, id DESC);

-- One redemption of a code by a tenant. The row is written as pending before
-- Core calls Kill Bill, so a retry after a lost answer finds the credit Kill
-- Bill made instead of writing a second one. claimed_until is the lease of
-- the request that calls Kill Bill: a retry resumes a pending row only after
-- it. killbill_credit_id is the invoice item of the credit. expires_at is
-- set when the row is written, so a resumed redemption keeps it. attempts and
-- next_attempt_at are the sweeper's retries of the expiry after a failure.
CREATE TABLE credit_code_redemptions (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    credit_code_id uuid NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    claimed_until timestamptz,
    amount_minor bigint NOT NULL,
    currency text NOT NULL,
    killbill_credit_id uuid,
    killbill_invoice_id uuid,
    expires_at timestamptz,
    expired_at timestamptz,
    reversed_minor bigint NOT NULL DEFAULT 0,
    reversal_killbill_id uuid,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    actor_type text NOT NULL,
    actor_id text,
    correlation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, credit_code_id) REFERENCES credit_codes (brand_id, id),
    CONSTRAINT credit_code_redemptions_attempts CHECK (attempts >= 0),
    CONSTRAINT credit_code_redemptions_status CHECK (status IN ('pending', 'completed')),
    CONSTRAINT credit_code_redemptions_completed CHECK (status = 'pending' OR killbill_credit_id IS NOT NULL),
    CONSTRAINT credit_code_redemptions_amount CHECK (amount_minor > 0),
    CONSTRAINT credit_code_redemptions_currency CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT credit_code_redemptions_reversed CHECK (reversed_minor >= 0 AND reversed_minor <= amount_minor)
);

CREATE UNIQUE INDEX credit_code_redemptions_scope_id_key ON credit_code_redemptions (brand_id, tenant_id, id);
CREATE INDEX credit_code_redemptions_code_tenant_idx ON credit_code_redemptions (credit_code_id, brand_id, tenant_id, created_at);
CREATE INDEX credit_code_redemptions_expiry_idx ON credit_code_redemptions (expires_at)
    WHERE status = 'completed' AND expires_at IS NOT NULL AND expired_at IS NULL;

-- Credits staff granted. Like a redemption, the row is written as pending
-- first and names the Kill Bill credit once it exists. attempts and
-- next_attempt_at are the sweeper's retries of the expiry after a failure.
CREATE TABLE staff_credits (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    idempotency_key text NOT NULL,
    fingerprint text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    claimed_until timestamptz,
    amount_minor bigint NOT NULL,
    currency text NOT NULL,
    reason text NOT NULL,
    killbill_credit_id uuid,
    killbill_invoice_id uuid,
    expires_at timestamptz,
    expired_at timestamptz,
    reversed_minor bigint NOT NULL DEFAULT 0,
    reversal_killbill_id uuid,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz,
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    correlation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT staff_credits_attempts CHECK (attempts >= 0),
    CONSTRAINT staff_credits_key_length CHECK (char_length(idempotency_key) BETWEEN 8 AND 255),
    CONSTRAINT staff_credits_status CHECK (status IN ('pending', 'completed')),
    CONSTRAINT staff_credits_completed CHECK (status = 'pending' OR killbill_credit_id IS NOT NULL),
    CONSTRAINT staff_credits_amount CHECK (amount_minor > 0),
    CONSTRAINT staff_credits_currency CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT staff_credits_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000),
    CONSTRAINT staff_credits_reversed CHECK (reversed_minor >= 0 AND reversed_minor <= amount_minor)
);

CREATE UNIQUE INDEX staff_credits_scope_id_key ON staff_credits (brand_id, tenant_id, id);
CREATE UNIQUE INDEX staff_credits_idempotency_key ON staff_credits (brand_id, tenant_id, actor_id, idempotency_key);
CREATE INDEX staff_credits_expiry_idx ON staff_credits (expires_at)
    WHERE status = 'completed' AND expires_at IS NOT NULL AND expired_at IS NULL;

-- The billing standing of a tenant as Kill Bill last reported it: the
-- overdue state and whether it blocks changes or entitlement. It is a cache.
-- Kill Bill stays the authority, and a push notification or a read refreshes
-- it.
CREATE TABLE tenant_billing_standing (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    overdue_state text NOT NULL,
    overdue_blocks_changes boolean NOT NULL DEFAULT false,
    overdue_disables_entitlement boolean NOT NULL DEFAULT false,
    last_event_type text,
    last_event_at timestamptz,
    refreshed_at timestamptz NOT NULL,
    PRIMARY KEY (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT tenant_billing_standing_state_length CHECK (char_length(overdue_state) BETWEEN 1 AND 255),
    CONSTRAINT tenant_billing_standing_event_length CHECK (last_event_type IS NULL OR char_length(last_event_type) <= 100)
);

-- Receipts of Kill Bill push notifications, so a notification Kill Bill
-- sends again is turned into Core events once. dedupe_key is a digest of the
-- notification and of the state Core read from Kill Bill when it arrived,
-- observed_state. object_key names the Kill Bill object, and seq orders the
-- receipts of an object. A notification whose state differs from the last
-- one recorded for its object is never dropped. The credit sweeper deletes
-- receipts older than seven days.
CREATE TABLE killbill_notifications (
    brand_id uuid NOT NULL REFERENCES brands (id),
    dedupe_key text NOT NULL,
    event_type text NOT NULL,
    tenant_id uuid,
    object_key text NOT NULL,
    observed_state text NOT NULL DEFAULT '',
    seq bigserial NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, dedupe_key),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT killbill_notifications_key_length CHECK (char_length(dedupe_key) BETWEEN 1 AND 128),
    CONSTRAINT killbill_notifications_event_length CHECK (char_length(event_type) BETWEEN 1 AND 100),
    CONSTRAINT killbill_notifications_object_length CHECK (char_length(object_key) BETWEEN 1 AND 300),
    CONSTRAINT killbill_notifications_state_length CHECK (char_length(observed_state) <= 600)
);

CREATE INDEX killbill_notifications_received_idx ON killbill_notifications (received_at);
CREATE INDEX killbill_notifications_object_idx ON killbill_notifications (brand_id, object_key, seq DESC);

-- Kill Bill plugins that brand apply installed or started through KPM, one
-- row per plugin version and Kill Bill node.
CREATE TABLE killbill_plugin_installs (
    id uuid PRIMARY KEY,
    plugin text NOT NULL,
    version text NOT NULL,
    node text NOT NULL,
    brand_id uuid REFERENCES brands (id),
    installed_at timestamptz NOT NULL,
    started_at timestamptz,
    CONSTRAINT killbill_plugin_installs_plugin_length CHECK (char_length(plugin) BETWEEN 1 AND 64),
    CONSTRAINT killbill_plugin_installs_version_length CHECK (char_length(version) BETWEEN 1 AND 50),
    CONSTRAINT killbill_plugin_installs_node_length CHECK (char_length(node) BETWEEN 1 AND 255)
);

CREATE UNIQUE INDEX killbill_plugin_installs_key ON killbill_plugin_installs (plugin, version, node);
