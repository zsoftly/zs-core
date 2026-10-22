-- Provisional next-free migration from main at Unit 3 implementation time.
-- Recheck and renumber it immediately before integrating another schema branch.
-- A receipt is inserted in the same Core transaction as its brand audit row.
CREATE TABLE keycloak_audit_receipts (
    brand_id uuid NOT NULL REFERENCES brands(id),
    realm_name text NOT NULL,
    kind text NOT NULL,
    event_id text NOT NULL,
    body_digest bytea NOT NULL,
    received_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, realm_name, kind, event_id),
    CONSTRAINT keycloak_audit_receipts_realm_name_length CHECK (char_length(realm_name) BETWEEN 1 AND 63),
    CONSTRAINT keycloak_audit_receipts_kind CHECK (kind IN ('user', 'admin')),
    CONSTRAINT keycloak_audit_receipts_event_id_length CHECK (char_length(event_id) BETWEEN 1 AND 128),
    CONSTRAINT keycloak_audit_receipts_digest_length CHECK (octet_length(body_digest) = 32)
);
