CREATE TABLE audit_events (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    action text NOT NULL,
    reason text,
    outcome text NOT NULL,
    correlation_id text NOT NULL,
    occurred_at timestamptz NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT audit_events_actor_type CHECK (
        actor_type IN ('user', 'api_key', 'staff', 'service', 'provider', 'system')
    ),
    CONSTRAINT audit_events_actor_id CHECK (actor_type = 'system' OR actor_id IS NOT NULL),
    CONSTRAINT audit_events_outcome CHECK (outcome IN ('success', 'failure', 'denied')),
    CONSTRAINT audit_events_reason_length CHECK (reason IS NULL OR char_length(reason) <= 1000)
);

CREATE INDEX audit_events_scope_time_idx
    ON audit_events (brand_id, tenant_id, occurred_at DESC, id DESC);
CREATE INDEX audit_events_scope_actor_idx
    ON audit_events (brand_id, tenant_id, actor_id, occurred_at DESC, id DESC);
CREATE INDEX audit_events_scope_action_idx
    ON audit_events (brand_id, tenant_id, action, occurred_at DESC, id DESC);

CREATE FUNCTION audit_events_reject_change() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'audit_events is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;

CREATE TRIGGER audit_events_no_update_delete
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_reject_change();

CREATE TRIGGER audit_events_no_truncate
    BEFORE TRUNCATE ON audit_events
    FOR EACH STATEMENT EXECUTE FUNCTION audit_events_reject_change();
