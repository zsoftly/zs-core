CREATE TABLE tenant_closure_review_items (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    id uuid NOT NULL,
    kind text NOT NULL CHECK (kind IN ('legacy_usage_events', 'legacy_usage_rollups')),
    reason text NOT NULL CHECK (reason IN ('legacy_usage_unbilled', 'legacy_rollup_unsent')),
    item_count bigint NOT NULL CHECK (item_count > 0),
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'object'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    correlation_id text NOT NULL,
    PRIMARY KEY (brand_id, tenant_id, id),
    UNIQUE (id),
    UNIQUE (brand_id, tenant_id, kind, item_count),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenant_closure_settlements (brand_id, tenant_id),
    CHECK ((kind = 'legacy_usage_events' AND reason = 'legacy_usage_unbilled') OR (kind = 'legacy_usage_rollups' AND reason = 'legacy_rollup_unsent'))
);
CREATE FUNCTION tenant_closure_review_item_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'closure review item is immutable' USING ERRCODE = 'check_violation'; END;
$$;
CREATE FUNCTION tenant_closure_review_resolution_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'closure review resolution is immutable' USING ERRCODE = 'check_violation'; END;
$$;
CREATE TRIGGER tenant_closure_review_items_no_change BEFORE UPDATE OR DELETE ON tenant_closure_review_items
FOR EACH ROW EXECUTE FUNCTION tenant_closure_review_item_immutable();
CREATE TRIGGER tenant_closure_review_items_no_truncate BEFORE TRUNCATE ON tenant_closure_review_items
FOR EACH STATEMENT EXECUTE FUNCTION tenant_closure_review_item_immutable();

CREATE TABLE tenant_closure_review_resolutions (
    item_id uuid PRIMARY KEY REFERENCES tenant_closure_review_items (id),
    resolved_by text NOT NULL CHECK (char_length(resolved_by) BETWEEN 1 AND 255),
    note text NOT NULL CHECK (char_length(note) BETWEEN 1 AND 500),
    resolved_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TRIGGER tenant_closure_review_resolutions_no_change BEFORE UPDATE OR DELETE ON tenant_closure_review_resolutions
FOR EACH ROW EXECUTE FUNCTION tenant_closure_review_resolution_immutable();
CREATE TRIGGER tenant_closure_review_resolutions_no_truncate BEFORE TRUNCATE ON tenant_closure_review_resolutions
FOR EACH STATEMENT EXECUTE FUNCTION tenant_closure_review_resolution_immutable();

-- The legacy usage a closing tenant can hold is found by tenant, so these
-- indexes cover only the rows the closure guard and the review look for.
CREATE INDEX usage_events_legacy_unrolled_idx ON usage_events (brand_id, tenant_id)
WHERE acceptance_protocol = 0 AND reservation_id IS NULL AND rollup_id IS NULL;
CREATE INDEX usage_rollups_legacy_unbilled_idx ON usage_rollups (brand_id, tenant_id)
WHERE send_protocol = 0 AND status IN ('pending', 'rejected');

-- Closure cannot complete while the tenant holds usage that was never
-- forwarded, pending or rejected, until staff have resolved a review item of
-- that kind that covers at least the usage held now. The check reads the usage
-- itself, so it holds even when no item was ever recorded, and usage that
-- appears after a review needs another one.
CREATE FUNCTION tenant_closure_complete_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    held bigint;
BEGIN
    IF NEW.status = 'completed' AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM 'completed') THEN
        SELECT count(*) INTO held FROM usage_events e WHERE e.brand_id = NEW.brand_id AND e.tenant_id = NEW.tenant_id
            AND e.acceptance_protocol = 0 AND e.reservation_id IS NULL AND e.rollup_id IS NULL;
        IF held > 0 AND NOT EXISTS (SELECT 1 FROM tenant_closure_review_items i JOIN tenant_closure_review_resolutions r ON r.item_id = i.id
                                    WHERE i.brand_id = NEW.brand_id AND i.tenant_id = NEW.tenant_id AND i.kind = 'legacy_usage_events' AND i.item_count >= held) THEN
            RAISE EXCEPTION 'closure has legacy usage events that staff have not reviewed' USING ERRCODE = 'check_violation';
        END IF;
        SELECT count(*) INTO held FROM usage_rollups u WHERE u.brand_id = NEW.brand_id AND u.tenant_id = NEW.tenant_id
            AND u.send_protocol = 0 AND u.status IN ('pending', 'rejected');
        IF held > 0 AND NOT EXISTS (SELECT 1 FROM tenant_closure_review_items i JOIN tenant_closure_review_resolutions r ON r.item_id = i.id
                                    WHERE i.brand_id = NEW.brand_id AND i.tenant_id = NEW.tenant_id AND i.kind = 'legacy_usage_rollups' AND i.item_count >= held) THEN
            RAISE EXCEPTION 'closure has legacy usage rollups that staff have not reviewed' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_complete_guard BEFORE INSERT OR UPDATE ON tenant_closure_settlements
FOR EACH ROW EXECUTE FUNCTION tenant_closure_complete_guard();
