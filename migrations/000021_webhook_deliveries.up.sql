-- Outbound webhook rows are independent durable deliveries. The endpoint URL
-- is a canonical identity and secret_ref is a reference only; secret values
-- are resolved immediately before each HTTP attempt.
ALTER TABLE outbox ADD CONSTRAINT outbox_brand_id_id_unique UNIQUE (brand_id, id);

CREATE TABLE webhook_fanouts (
    brand_id uuid NOT NULL REFERENCES brands(id),
    event_id uuid NOT NULL,
    endpoint_count integer NOT NULL CHECK (endpoint_count >= 0),
    fanned_out_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, event_id),
    FOREIGN KEY (brand_id, event_id) REFERENCES outbox (brand_id, id)
);

CREATE TABLE webhook_deliveries (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands(id),
    event_id uuid NOT NULL,
    event_type text NOT NULL,
    endpoint_url text NOT NULL,
    secret_ref text NOT NULL,
    envelope jsonb NOT NULL,
    attempts integer NOT NULL DEFAULT 0 CHECK (attempts >= 0),
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_token uuid,
    claimed_until timestamptz,
    delivered_at timestamptz,
    canceled_at timestamptz,
    quarantined_at timestamptz,
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    CHECK (jsonb_typeof(envelope) = 'object'),
    CHECK (length(endpoint_url) <= 2048),
    CHECK (length(secret_ref) <= 4096),
    UNIQUE (brand_id, event_id, endpoint_url),
    FOREIGN KEY (brand_id, event_id) REFERENCES outbox (brand_id, id)
);
CREATE INDEX webhook_deliveries_ready_idx ON webhook_deliveries (next_attempt_at, id)
    WHERE delivered_at IS NULL AND canceled_at IS NULL AND quarantined_at IS NULL;
