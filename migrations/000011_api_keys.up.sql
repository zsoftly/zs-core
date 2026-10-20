-- Tenant API keys. Only the SHA-256 digest of a secret is stored. The secret
-- itself is shown once, when the key is created. See docs/modules/api-keys.md.
CREATE TABLE api_keys (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    created_by_membership_id uuid NOT NULL,
    name text NOT NULL,
    digest bytea NOT NULL,
    prefix text NOT NULL,
    permissions text[] NOT NULL,
    expires_at timestamptz NOT NULL,
    last_used_at timestamptz,
    revoked_at timestamptz,
    revoked_reason text,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, tenant_id, created_by_membership_id) REFERENCES memberships (brand_id, tenant_id, id),
    CONSTRAINT api_keys_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT api_keys_digest_length CHECK (octet_length(digest) = 32),
    CONSTRAINT api_keys_prefix_length CHECK (char_length(prefix) BETWEEN 1 AND 80),
    CONSTRAINT api_keys_expiry CHECK (expires_at > created_at AND expires_at <= created_at + interval '365 days 1 hour'),
    CONSTRAINT api_keys_revoked_reason_length CHECK (revoked_reason IS NULL OR char_length(revoked_reason) <= 1000)
);

CREATE UNIQUE INDEX api_keys_digest_key ON api_keys (digest);
CREATE UNIQUE INDEX api_keys_scope_id_key ON api_keys (brand_id, tenant_id, id);
CREATE INDEX api_keys_creator_idx ON api_keys (brand_id, tenant_id, created_by_membership_id);
