-- A usage rollup whose send Kill Bill never confirmed is frozen: it is never
-- sent again and its record never changes. Staff can confirm that Kill Bill
-- holds it, with the invoice that bills it as the evidence, and that is kept
-- here without touching the rollup. A tenant whose rollups are all confirmed
-- or sent is drained.
CREATE TABLE usage_rollup_resolutions (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    rollup_id uuid NOT NULL PRIMARY KEY,
    decision text NOT NULL CHECK (decision IN ('forwarded')),
    invoice_id uuid NOT NULL,
    resolved_by text NOT NULL CHECK (char_length(resolved_by) BETWEEN 1 AND 300),
    note text NOT NULL CHECK (char_length(note) BETWEEN 1 AND 1000),
    resolved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    FOREIGN KEY (brand_id, tenant_id, rollup_id) REFERENCES usage_rollups (brand_id, tenant_id, id)
);

-- Only a rollup whose send started and whose outcome is unconfirmed can be
-- confirmed as forwarded: one that never started sent nothing.
CREATE FUNCTION usage_rollup_resolution_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM usage_rollups r
        WHERE r.brand_id = NEW.brand_id AND r.tenant_id = NEW.tenant_id AND r.id = NEW.rollup_id
          AND r.send_protocol = 1 AND r.status = 'pending' AND r.send_started_at IS NOT NULL
          AND r.closure_unresolved_reason IN ('usage_send_unconfirmed', 'usage_duplicate_unconfirmed', 'usage_transport_unconfirmed')) THEN
        RAISE EXCEPTION 'only an unconfirmed send can be confirmed as forwarded' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER usage_rollup_resolution_guard BEFORE INSERT ON usage_rollup_resolutions
FOR EACH ROW EXECUTE FUNCTION usage_rollup_resolution_guard();

CREATE FUNCTION usage_rollup_resolution_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a usage rollup resolution is never changed or removed' USING ERRCODE = 'check_violation';
END;
$$;
CREATE TRIGGER usage_rollup_resolution_no_change BEFORE UPDATE OR DELETE ON usage_rollup_resolutions
FOR EACH ROW EXECUTE FUNCTION usage_rollup_resolution_immutable();
CREATE TRIGGER usage_rollup_resolution_no_truncate BEFORE TRUNCATE ON usage_rollup_resolutions
FOR EACH STATEMENT EXECUTE FUNCTION usage_rollup_resolution_immutable();
ALTER TABLE usage_rollup_resolutions ENABLE ALWAYS TRIGGER usage_rollup_resolution_guard;
ALTER TABLE usage_rollup_resolutions ENABLE ALWAYS TRIGGER usage_rollup_resolution_no_change;
ALTER TABLE usage_rollup_resolutions ENABLE ALWAYS TRIGGER usage_rollup_resolution_no_truncate;
