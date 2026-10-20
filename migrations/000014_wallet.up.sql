-- Wallet extension on Kill Bill. Kill Bill is the financial authority: the
-- wallet a product sees is a view, the Kill Bill account credit minus the
-- reservations Core holds. These tables hold holds, their history, the
-- rate limit of wallet events, a version per tenant wallet that tells Core
-- instances their cached Kill Bill credit is stale, and the findings of the
-- reconciliation sweep. They never hold a balance, a payment, an invoice or
-- a price. A settled reservation names the Kill Bill object its settlement
-- produced. See docs/modules/wallet.md.

-- A hold against the tenant's Kill Bill credit before work starts.
-- external_ref and attempt are the caller's keys. created_by_actor is the
-- membership or API key that made the hold: only it, or staff with a
-- reason, may settle, release, extend or grow it. amount_minor is the hold
-- in minor units of the account currency. A settlement is written here as
-- an intent, settlement_pending with the request, its fingerprint and the
-- chosen subscription, before Kill Bill is called. A pending hold never
-- expires: the sweeper completes it once Kill Bill answers. settle_claim_token
-- and settle_claimed_until mark the one worker that calls Kill Bill for it,
-- and settle_attempts and settle_next_at its backoff. sent_settlement is the
-- settlement last sent to Kill Bill whose outcome was not a refusal, so a
-- later settlement under the same key with another amount is refused.
-- killbill_ref is the usage record's tracking ID or the external charge's
-- invoice item ID.
CREATE TABLE reservations (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    currency text NOT NULL,
    external_ref text NOT NULL,
    attempt integer NOT NULL,
    label text NOT NULL DEFAULT '',
    amount_minor bigint NOT NULL,
    status text NOT NULL DEFAULT 'held',
    lease_expires_at timestamptz NOT NULL,
    settlement_pending boolean NOT NULL DEFAULT false,
    settlement jsonb,
    settlement_fingerprint text,
    settled_minor bigint,
    rated_by text,
    killbill_ref_type text,
    killbill_ref text,
    killbill_subscription_id uuid,
    estimate_minor bigint,
    settle_claim_token bytea,
    settle_claimed_until timestamptz,
    settle_attempts integer NOT NULL DEFAULT 0,
    settle_next_at timestamptz,
    settle_requested_at timestamptz,
    stale_reported_at timestamptz,
    sent_settlement jsonb,
    created_by_actor text NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    correlation_id text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    settled_at timestamptz,
    released_at timestamptz,
    expired_at timestamptz,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT reservations_ref CHECK (external_ref ~ '^[A-Za-z0-9][A-Za-z0-9._:-]{0,199}$'),
    CONSTRAINT reservations_attempt CHECK (attempt BETWEEN 0 AND 1000000),
    CONSTRAINT reservations_currency CHECK (currency ~ '^[A-Z]{3}$'),
    CONSTRAINT reservations_label_length CHECK (char_length(label) <= 200),
    CONSTRAINT reservations_amount CHECK (amount_minor > 0),
    CONSTRAINT reservations_status CHECK (status IN ('held', 'settled', 'released', 'expired')),
    CONSTRAINT reservations_pending_held CHECK (NOT settlement_pending OR (status = 'held' AND settlement IS NOT NULL)),
    CONSTRAINT reservations_settled_ref CHECK (status <> 'settled' OR (killbill_ref IS NOT NULL AND killbill_ref_type IS NOT NULL)),
    CONSTRAINT reservations_ref_type CHECK (killbill_ref_type IS NULL OR killbill_ref_type IN ('usage_record', 'external_charge')),
    CONSTRAINT reservations_rated_by CHECK (rated_by IS NULL OR rated_by IN ('external_charge', 'catalogue_price', 'unrated')),
    CONSTRAINT reservations_settled_minor CHECK (settled_minor IS NULL OR settled_minor >= 0),
    CONSTRAINT reservations_estimate CHECK (estimate_minor IS NULL OR estimate_minor >= 0),
    CONSTRAINT reservations_settled_at CHECK (status <> 'settled' OR settled_at IS NOT NULL),
    CONSTRAINT reservations_usage_subscription CHECK (killbill_ref_type IS DISTINCT FROM 'usage_record' OR killbill_subscription_id IS NOT NULL),
    CONSTRAINT reservations_pending_requested CHECK (NOT settlement_pending OR settle_requested_at IS NOT NULL),
    CONSTRAINT reservations_settle_attempts CHECK (settle_attempts >= 0),
    CONSTRAINT reservations_created_by_length CHECK (char_length(created_by_actor) BETWEEN 1 AND 200)
);

CREATE UNIQUE INDEX reservations_ref_key ON reservations (brand_id, tenant_id, external_ref, attempt);
CREATE UNIQUE INDEX reservations_scope_id_key ON reservations (brand_id, tenant_id, id);
CREATE INDEX reservations_held_idx ON reservations (brand_id, tenant_id) WHERE status = 'held';
CREATE INDEX reservations_lease_idx ON reservations (lease_expires_at) WHERE status = 'held' AND NOT settlement_pending;
CREATE INDEX reservations_pending_idx ON reservations (settle_next_at) WHERE settlement_pending;
CREATE INDEX reservations_list_idx ON reservations (brand_id, tenant_id, created_at DESC, id DESC);

-- The history of each reservation: reserve, grow, extend, settle, a queued,
-- failed or stale settlement, release and expiry. amount_minor is the hold after
-- the event. Append-only.
CREATE TABLE reservation_events (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    reservation_id uuid NOT NULL,
    kind text NOT NULL,
    amount_minor bigint NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    correlation_id text NOT NULL,
    details jsonb,
    occurred_at timestamptz NOT NULL DEFAULT now(),
    FOREIGN KEY (brand_id, tenant_id, reservation_id) REFERENCES reservations (brand_id, tenant_id, id),
    CONSTRAINT reservation_events_kind CHECK (kind IN ('reserve', 'grow', 'extend', 'settle', 'settlement_queued',
        'settlement_failed', 'settlement_stale', 'release', 'expire'))
);

CREATE INDEX reservation_events_reservation_idx ON reservation_events (brand_id, tenant_id, reservation_id, occurred_at);

CREATE FUNCTION reservation_events_reject_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'reservation_events is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER reservation_events_no_update_delete
    BEFORE UPDATE OR DELETE ON reservation_events
    FOR EACH ROW EXECUTE FUNCTION reservation_events_reject_change();

CREATE TRIGGER reservation_events_no_truncate
    BEFORE TRUNCATE ON reservation_events
    FOR EACH STATEMENT EXECUTE FUNCTION reservation_events_reject_change();

-- When Core last emitted a rate-limited event of a kind for a tenant, such
-- as wallet.low_balance or metering.forward_backlog.
CREATE TABLE tenant_alerts (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    last_sent_at timestamptz NOT NULL,
    PRIMARY KEY (brand_id, tenant_id, kind),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT tenant_alerts_kind_length CHECK (char_length(kind) BETWEEN 1 AND 100)
);

-- What the reconciliation sweep found in Kill Bill that Core's records do
-- not explain, once per finding. killbill_ref names the Kill Bill object.
-- The sweep reports a finding when it first sees it and never changes Kill
-- Bill.
CREATE TABLE wallet_reconciliation_findings (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    kind text NOT NULL,
    killbill_ref text NOT NULL,
    details jsonb NOT NULL,
    first_seen_at timestamptz NOT NULL,
    last_seen_at timestamptz NOT NULL,
    PRIMARY KEY (brand_id, tenant_id, kind, killbill_ref),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT wallet_reconciliation_findings_kind CHECK (kind IN ('unknown_credit', 'missing_credit', 'unknown_charge',
        'missing_charge', 'unknown_usage', 'missing_usage', 'held_mismatch', 'over_reserved', 'pending_settlement_stale',
        'rejected_usage')),
    CONSTRAINT wallet_reconciliation_findings_ref_length CHECK (char_length(killbill_ref) BETWEEN 1 AND 255)
);

-- When the reconciliation sweep last compared a tenant with Kill Bill.
CREATE TABLE wallet_reconciliations (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    last_run_at timestamptz NOT NULL,
    PRIMARY KEY (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id)
);

-- The version of a tenant's wallet. Core bumps it in the same transaction as
-- every change to the tenant's Kill Bill credit or unbilled amount that Core
-- makes or detects: a settlement, a credit redemption, a staff credit, a
-- credit expiry, forwarded usage and a credit change the reconciliation
-- finds. A Core instance relies on the Kill Bill credit it cached only while
-- the version it read under the tenant lock is the one it cached with.
CREATE TABLE wallet_versions (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    version bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT wallet_versions_version CHECK (version > 0)
);
