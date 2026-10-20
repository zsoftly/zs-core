CREATE TABLE brands (
    id uuid PRIMARY KEY,
    slug text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    keycloak_realm text NOT NULL,
    killbill_tenant_id uuid,
    killbill_api_key text NOT NULL,
    killbill_api_secret_ref text NOT NULL,
    manifest jsonb NOT NULL,
    manifest_digest text NOT NULL,
    applied_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT brands_slug_format CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) BETWEEN 2 AND 40),
    CONSTRAINT brands_name_length CHECK (char_length(name) BETWEEN 1 AND 200),
    CONSTRAINT brands_status CHECK (status IN ('active', 'disabled')),
    CONSTRAINT brands_realm_length CHECK (char_length(keycloak_realm) BETWEEN 1 AND 255),
    CONSTRAINT brands_killbill_api_key_length CHECK (char_length(killbill_api_key) BETWEEN 1 AND 255),
    -- Only a reference is ever stored, never the secret itself.
    CONSTRAINT brands_secret_is_reference CHECK (killbill_api_secret_ref ~ '^(env://[A-Za-z_][A-Za-z0-9_]*|file:///.+)$'),
    CONSTRAINT brands_manifest_object CHECK (jsonb_typeof(manifest) = 'object'),
    CONSTRAINT brands_manifest_digest_format CHECK (manifest_digest ~ '^sha256:[0-9a-f]{64}$')
);

CREATE UNIQUE INDEX brands_slug_key ON brands (slug);
CREATE UNIQUE INDEX brands_keycloak_realm_key ON brands (keycloak_realm);
CREATE UNIQUE INDEX brands_killbill_api_key_key ON brands (killbill_api_key);
CREATE UNIQUE INDEX brands_killbill_tenant_id_key ON brands (killbill_tenant_id) WHERE killbill_tenant_id IS NOT NULL;
