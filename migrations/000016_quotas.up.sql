-- Quotas and entitlements on Kill Bill. Kill Bill decides which plan a
-- tenant is on. Core holds the limit of each feature per plan, the
-- overrides staff grant a tenant, and counters. See docs/modules/quotas.md.

-- The quotas of a brand: the limit of a feature for a plan, or for every
-- plan with *. window_kind is none, day, month or concurrent. meter_slug
-- names the meter whose usage counts against a day or month limit.
CREATE TABLE quotas (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    feature text NOT NULL,
    plan text NOT NULL,
    limit_value bigint NOT NULL,
    window_kind text NOT NULL,
    meter_slug text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT quotas_feature CHECK (feature ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,99}$'),
    CONSTRAINT quotas_plan_length CHECK (char_length(plan) BETWEEN 1 AND 255),
    CONSTRAINT quotas_limit CHECK (limit_value BETWEEN 0 AND 1000000000000000),
    CONSTRAINT quotas_window CHECK (window_kind IN ('none', 'day', 'month', 'concurrent')),
    CONSTRAINT quotas_meter CHECK ((window_kind IN ('day', 'month') AND meter_slug IS NOT NULL)
        OR (window_kind = 'concurrent' AND meter_slug IS NULL) OR window_kind = 'none')
);

CREATE UNIQUE INDEX quotas_brand_feature_plan_key ON quotas (brand_id, feature, plan);

-- Limits staff set for one tenant. The latest override of a feature is in
-- force until it expires. An override with no limit_value clears the ones
-- before it for good, even once it expires itself. window_kind and
-- meter_slug are set when the override names its window, which it must
-- when no quota of the brand covers the feature. An override that names a
-- window applies only while the plan's quota has that window, or while no
-- quota covers the feature. Append-only.
CREATE TABLE quota_overrides (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    feature text NOT NULL,
    limit_value bigint,
    window_kind text,
    meter_slug text,
    reason text NOT NULL,
    expires_at timestamptz,
    actor_type text NOT NULL,
    actor_id text NOT NULL,
    correlation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT quota_overrides_feature CHECK (feature ~ '^[A-Za-z0-9][A-Za-z0-9_.:-]{0,99}$'),
    CONSTRAINT quota_overrides_limit CHECK (limit_value IS NULL OR limit_value BETWEEN 0 AND 1000000000000000),
    CONSTRAINT quota_overrides_window CHECK (window_kind IS NULL OR window_kind IN ('none', 'day', 'month', 'concurrent')),
    CONSTRAINT quota_overrides_meter CHECK ((window_kind IS NULL AND meter_slug IS NULL)
        OR (window_kind IN ('day', 'month') AND meter_slug IS NOT NULL)
        OR (window_kind = 'concurrent' AND meter_slug IS NULL) OR window_kind = 'none'),
    CONSTRAINT quota_overrides_reason_length CHECK (char_length(reason) BETWEEN 1 AND 1000)
);

CREATE INDEX quota_overrides_lookup_idx ON quota_overrides (brand_id, tenant_id, feature, created_at DESC, id DESC);

CREATE FUNCTION quota_overrides_reject_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'quota_overrides is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER quota_overrides_no_update_delete
    BEFORE UPDATE OR DELETE ON quota_overrides
    FOR EACH ROW EXECUTE FUNCTION quota_overrides_reject_change();

CREATE TRIGGER quota_overrides_no_truncate
    BEFORE TRUNCATE ON quota_overrides
    FOR EACH STATEMENT EXECUTE FUNCTION quota_overrides_reject_change();

-- Counters per tenant, feature and window. A concurrent counter has the
-- window start 1970-01-01 and is the sum of the tenant's leases. A day or
-- month counter is what Consume admitted in the window, raised to the
-- usage summaries when they count more. It is authoritative for admission
-- and the summaries only detect drift.
CREATE TABLE quota_counters (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    feature text NOT NULL,
    window_start timestamptz NOT NULL,
    value bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, feature, window_start),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT quota_counters_value CHECK (value >= 0)
);

-- What each caller reference holds of a quota, such as a running job.
-- Consume adds a lease, Release removes it, so a retry of either changes
-- nothing. created_by_actor is the membership or API key that consumed:
-- only it, or staff with a reason, may release or extend the lease. A
-- concurrent lease expires at expires_at unless it is extended, and the
-- quota sweeper then releases it. A day or month lease records a
-- consumption of the window that starts at window_start, so a retry is
-- counted once. It ends with its window and is never given back.
CREATE TABLE quota_leases (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    feature text NOT NULL,
    ref text NOT NULL,
    amount bigint NOT NULL,
    window_kind text NOT NULL,
    window_start timestamptz NOT NULL,
    expires_at timestamptz NOT NULL,
    created_by_actor text NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, feature, ref),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT quota_leases_ref CHECK (ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$'),
    CONSTRAINT quota_leases_amount CHECK (amount > 0),
    CONSTRAINT quota_leases_window CHECK (window_kind IN ('concurrent', 'day', 'month')),
    CONSTRAINT quota_leases_created_by_length CHECK (char_length(created_by_actor) BETWEEN 1 AND 200)
);

CREATE INDEX quota_leases_expiry_idx ON quota_leases (expires_at);
