CREATE TABLE staff_grants (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    issuer text NOT NULL,
    subject text NOT NULL,
    scope text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    granted_by text NOT NULL,
    reason text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT staff_grants_scope CHECK (scope IN ('operator', 'activation', 'billing', 'support')),
    CONSTRAINT staff_grants_status CHECK (status IN ('active', 'revoked')),
    CONSTRAINT staff_grants_issuer_length CHECK (char_length(issuer) BETWEEN 1 AND 1000),
    CONSTRAINT staff_grants_subject_length CHECK (char_length(subject) BETWEEN 1 AND 255),
    CONSTRAINT staff_grants_granted_by_length CHECK (char_length(granted_by) BETWEEN 1 AND 255),
    CONSTRAINT staff_grants_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000)
);

CREATE UNIQUE INDEX staff_grants_active_key
    ON staff_grants (brand_id, issuer, subject, scope)
    WHERE status = 'active';
