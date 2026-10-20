-- Tenants, memberships, teams, invitations and activation decisions. Every
-- tenant-owned row carries brand_id and tenant_id, and a composite foreign key
-- to tenants (brand_id, id), so a row can never point at a tenant of another
-- brand. A column that names an identity references identities (brand_id,
-- id) for the same reason. identities is created by 000008, so those foreign
-- keys and the unique index they need are added by 000012. See
-- docs/modules/identity-and-tenants.md and docs/modules/tenants.md.

CREATE TABLE tenants (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    kind text NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    activation text NOT NULL DEFAULT 'pending',
    keycloak_organization_id text NOT NULL,
    killbill_account_id uuid NOT NULL,
    billing_country text,
    created_by_identity_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    suspended_at timestamptz,
    closed_at timestamptz,
    CONSTRAINT tenants_kind CHECK (kind IN ('personal', 'organization')),
    CONSTRAINT tenants_slug_format CHECK (slug ~ '^[a-z0-9]+(-[a-z0-9]+)*$' AND char_length(slug) BETWEEN 2 AND 63),
    -- Kill Bill stores the account name in a varchar(100).
    CONSTRAINT tenants_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT tenants_status CHECK (status IN ('active', 'suspended', 'closed')),
    CONSTRAINT tenants_activation CHECK (activation IN ('pending', 'approved', 'rejected')),
    CONSTRAINT tenants_keycloak_organization_length CHECK (char_length(keycloak_organization_id) BETWEEN 1 AND 255),
    CONSTRAINT tenants_billing_country_format CHECK (billing_country IS NULL OR billing_country ~ '^[A-Z]{2}$')
);

CREATE UNIQUE INDEX tenants_brand_id_key ON tenants (brand_id, id);
CREATE UNIQUE INDEX tenants_brand_slug_key ON tenants (brand_id, slug);
CREATE UNIQUE INDEX tenants_brand_killbill_account_key ON tenants (brand_id, killbill_account_id);
CREATE UNIQUE INDEX tenants_brand_keycloak_organization_key ON tenants (brand_id, keycloak_organization_id);

-- One personal tenant per identity and brand.
CREATE TABLE personal_tenant_principals (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    identity_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, identity_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id)
);

CREATE UNIQUE INDEX personal_tenant_principals_tenant_key ON personal_tenant_principals (brand_id, tenant_id);

CREATE TABLE memberships (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    identity_id uuid NOT NULL,
    role text NOT NULL,
    status text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    removed_at timestamptz,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT memberships_role CHECK (role IN ('owner', 'admin', 'billing_admin', 'member')),
    CONSTRAINT memberships_status CHECK (status IN ('active', 'suspended', 'removed'))
);

CREATE UNIQUE INDEX memberships_scope_id_key ON memberships (brand_id, tenant_id, id);
-- An identity has at most one membership in a tenant that is not removed. A
-- removed membership stays as history: accepting a new invitation inserts a
-- new membership with a new ID.
CREATE UNIQUE INDEX memberships_scope_identity_key ON memberships (brand_id, tenant_id, identity_id) WHERE status <> 'removed';
CREATE INDEX memberships_scope_identity_idx ON memberships (brand_id, tenant_id, identity_id, created_at DESC);
CREATE INDEX memberships_identity_idx ON memberships (brand_id, identity_id) WHERE status = 'active';

CREATE TABLE teams (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    name text NOT NULL,
    system boolean NOT NULL DEFAULT false,
    locked boolean NOT NULL DEFAULT false,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT teams_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    -- The three system teams are always locked.
    CONSTRAINT teams_system_locked CHECK (NOT system OR locked)
);

CREATE UNIQUE INDEX teams_scope_id_key ON teams (brand_id, tenant_id, id);
CREATE UNIQUE INDEX teams_scope_name_key ON teams (brand_id, tenant_id, lower(name));

CREATE TABLE team_members (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    team_id uuid NOT NULL,
    membership_id uuid NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, team_id, membership_id),
    FOREIGN KEY (brand_id, tenant_id, team_id) REFERENCES teams (brand_id, tenant_id, id) ON DELETE CASCADE,
    FOREIGN KEY (brand_id, tenant_id, membership_id) REFERENCES memberships (brand_id, tenant_id, id)
);

CREATE INDEX team_members_membership_idx ON team_members (brand_id, tenant_id, membership_id);

CREATE TABLE team_grants (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    team_id uuid NOT NULL,
    permission text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, team_id, permission),
    FOREIGN KEY (brand_id, tenant_id, team_id) REFERENCES teams (brand_id, tenant_id, id) ON DELETE CASCADE,
    CONSTRAINT team_grants_permission_format CHECK (permission ~ '^[a-z][a-z0-9_]*:[a-z][a-z0-9_]*$' AND char_length(permission) <= 100)
);

-- The clear invitation token is shown once and never stored: only its
-- SHA-256 digest is.
CREATE TABLE invitations (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    email text NOT NULL,
    role text NOT NULL,
    token_digest bytea NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    invited_by_membership_id uuid NOT NULL,
    expires_at timestamptz NOT NULL,
    accepted_at timestamptz,
    accepted_membership_id uuid,
    revoked_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, tenant_id, invited_by_membership_id) REFERENCES memberships (brand_id, tenant_id, id),
    FOREIGN KEY (brand_id, tenant_id, accepted_membership_id) REFERENCES memberships (brand_id, tenant_id, id),
    CONSTRAINT invitations_accepted_membership CHECK ((status = 'accepted') = (accepted_membership_id IS NOT NULL)),
    CONSTRAINT invitations_email_length CHECK (char_length(email) BETWEEN 3 AND 320),
    CONSTRAINT invitations_role CHECK (role IN ('admin', 'billing_admin', 'member')),
    CONSTRAINT invitations_token_digest_length CHECK (octet_length(token_digest) = 32),
    CONSTRAINT invitations_status CHECK (status IN ('pending', 'accepted', 'revoked'))
);

CREATE UNIQUE INDEX invitations_scope_id_key ON invitations (brand_id, tenant_id, id);
CREATE UNIQUE INDEX invitations_token_digest_key ON invitations (token_digest);
CREATE INDEX invitations_inviter_pending_idx ON invitations (brand_id, tenant_id, invited_by_membership_id) WHERE status = 'pending';

-- Activation decisions are append-only. The current state is copied to
-- tenants.activation in the same transaction.
CREATE TABLE tenant_activations (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    state text NOT NULL,
    reason text NOT NULL,
    request_id text NOT NULL,
    fingerprint text NOT NULL,
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    decided_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT tenant_activations_state CHECK (state IN ('pending', 'approved', 'rejected')),
    CONSTRAINT tenant_activations_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000),
    CONSTRAINT tenant_activations_request_id_length CHECK (char_length(request_id) BETWEEN 8 AND 255)
);

CREATE UNIQUE INDEX tenant_activations_request_key ON tenant_activations (brand_id, tenant_id, request_id);
CREATE INDEX tenant_activations_scope_time_idx ON tenant_activations (brand_id, tenant_id, decided_at DESC, id DESC);

CREATE FUNCTION tenant_activations_reject_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'tenant_activations is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER tenant_activations_no_update_delete
    BEFORE UPDATE OR DELETE ON tenant_activations
    FOR EACH ROW EXECUTE FUNCTION tenant_activations_reject_change();

CREATE TRIGGER tenant_activations_no_truncate
    BEFORE TRUNCATE ON tenant_activations
    FOR EACH STATEMENT EXECUTE FUNCTION tenant_activations_reject_change();

-- A tenant creation in flight. The row is written before Core calls Keycloak
-- and Kill Bill and records the upstream IDs as they are created. The tenants
-- row is inserted only when both exist. A creation that fails stays pending,
-- and the sweeper, or a retry with the same idempotency_key, completes it. It
-- is also the idempotency record of POST /v1/tenants: the key is scoped to
-- the brand and the creating identity.
CREATE TABLE tenant_provisioning (
    tenant_id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    kind text NOT NULL,
    slug text NOT NULL,
    name text NOT NULL,
    billing_country text,
    created_by_identity_id uuid NOT NULL,
    idempotency_key text,
    fingerprint text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    keycloak_organization_id text,
    killbill_account_id uuid,
    attempts integer NOT NULL DEFAULT 0,
    last_error text,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_token bytea,
    claimed_until timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    completed_at timestamptz,
    CONSTRAINT tenant_provisioning_kind CHECK (kind IN ('personal', 'organization')),
    CONSTRAINT tenant_provisioning_name_length CHECK (char_length(name) BETWEEN 1 AND 100),
    CONSTRAINT tenant_provisioning_status CHECK (status IN ('pending', 'completed')),
    CONSTRAINT tenant_provisioning_key_length CHECK (idempotency_key IS NULL OR char_length(idempotency_key) BETWEEN 8 AND 255),
    CONSTRAINT tenant_provisioning_organization_has_key CHECK (kind = 'personal' OR idempotency_key IS NOT NULL),
    CONSTRAINT tenant_provisioning_last_error_length CHECK (last_error IS NULL OR char_length(last_error) <= 200)
);

CREATE UNIQUE INDEX tenant_provisioning_idempotency_key
    ON tenant_provisioning (brand_id, created_by_identity_id, idempotency_key)
    WHERE idempotency_key IS NOT NULL;
CREATE UNIQUE INDEX tenant_provisioning_personal_key
    ON tenant_provisioning (brand_id, created_by_identity_id)
    WHERE kind = 'personal';
CREATE INDEX tenant_provisioning_due_idx
    ON tenant_provisioning (next_attempt_at)
    WHERE status = 'pending';
