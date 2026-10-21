-- Billing email is an eventually consistent projection of Core membership.
-- Store the owner binding and retry outcome, never a second copy of an email.
CREATE TABLE tenant_billing_recipients (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    membership_id uuid,
    status text NOT NULL DEFAULT 'pending',
    checked_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error_code text,
    PRIMARY KEY (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, tenant_id, membership_id) REFERENCES memberships (brand_id, tenant_id, id),
    CONSTRAINT tenant_billing_recipients_status CHECK (status IN ('pending', 'ready', 'no_eligible_owner')),
    CONSTRAINT tenant_billing_recipients_attempts CHECK (attempts >= 0),
    CONSTRAINT tenant_billing_recipients_error_length CHECK (last_error_code IS NULL OR char_length(last_error_code) <= 100)
);

CREATE INDEX tenant_billing_recipients_due_idx ON tenant_billing_recipients (next_attempt_at);

-- Existing accounts need a real upstream comparison, not an assumed success.
INSERT INTO tenant_billing_recipients (brand_id, tenant_id)
SELECT brand_id, id FROM tenants;
