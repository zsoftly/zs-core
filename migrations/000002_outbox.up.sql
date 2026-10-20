CREATE TABLE outbox (
    id uuid PRIMARY KEY,
    event_type text NOT NULL,
    brand_id uuid NOT NULL,
    tenant_id uuid,
    occurred_at timestamptz NOT NULL,
    envelope jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    published_at timestamptz,
    dispatch_attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_dispatch_error text,
    quarantined_at timestamptz
);

CREATE INDEX outbox_dispatch_ready_idx
    ON outbox (next_attempt_at, occurred_at, id)
    WHERE published_at IS NULL AND quarantined_at IS NULL;

CREATE INDEX outbox_brand_tenant_idx ON outbox (brand_id, tenant_id, occurred_at);
