CREATE TABLE idempotency_keys (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    actor_id text NOT NULL,
    idempotency_key text NOT NULL,
    fingerprint text NOT NULL,
    status text NOT NULL,
    result_status integer,
    result_body bytea,
    created_at timestamptz NOT NULL,
    completed_at timestamptz,
    lease_expires_at timestamptz,
    expires_at timestamptz NOT NULL,
    PRIMARY KEY (brand_id, tenant_id, actor_id, idempotency_key),
    CONSTRAINT idempotency_keys_key_length CHECK (char_length(idempotency_key) BETWEEN 8 AND 255),
    CONSTRAINT idempotency_keys_status CHECK (status IN ('pending', 'completed')),
    CONSTRAINT idempotency_keys_completed_has_result CHECK (
        status = 'pending' OR (result_status IS NOT NULL AND result_body IS NOT NULL AND completed_at IS NOT NULL)
    )
);

CREATE INDEX idempotency_keys_expires_at_idx ON idempotency_keys (expires_at);
