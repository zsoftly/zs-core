-- The review rule of migration 26 and the order rule of migration 27 as
-- functions the application can ask before it writes, so a refusal is an
-- answer and not a statement error, which would end a lifecycle capability.
-- The triggers stay as the database's own check.
CREATE FUNCTION tenant_closure_review_blocker(p_brand uuid, p_tenant uuid) RETURNS text LANGUAGE plpgsql STABLE AS $$
DECLARE
    held bigint;
BEGIN
    SELECT count(*) INTO held FROM usage_events e WHERE e.brand_id = p_brand AND e.tenant_id = p_tenant
        AND e.acceptance_protocol = 0 AND e.reservation_id IS NULL AND e.rollup_id IS NULL;
    IF held > 0 AND NOT EXISTS (SELECT 1 FROM tenant_closure_review_items i JOIN tenant_closure_review_resolutions r ON r.item_id = i.id
                                WHERE i.brand_id = p_brand AND i.tenant_id = p_tenant AND i.kind = 'legacy_usage_events' AND i.item_count >= held) THEN
        RETURN 'closure has legacy usage events that staff have not reviewed';
    END IF;
    SELECT count(*) INTO held FROM usage_rollups u WHERE u.brand_id = p_brand AND u.tenant_id = p_tenant
        AND u.send_protocol = 0 AND u.status IN ('pending', 'rejected');
    IF held > 0 AND NOT EXISTS (SELECT 1 FROM tenant_closure_review_items i JOIN tenant_closure_review_resolutions r ON r.item_id = i.id
                                WHERE i.brand_id = p_brand AND i.tenant_id = p_tenant AND i.kind = 'legacy_usage_rollups' AND i.item_count >= held) THEN
        RETURN 'closure has legacy usage rollups that staff have not reviewed';
    END IF;
    RETURN NULL;
END;
$$;

CREATE OR REPLACE FUNCTION tenant_closure_complete_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    blocker text;
BEGIN
    IF NEW.status = 'completed' AND (TG_OP = 'INSERT' OR OLD.status IS DISTINCT FROM 'completed') THEN
        blocker := tenant_closure_review_blocker(NEW.brand_id, NEW.tenant_id);
        IF blocker IS NOT NULL THEN
            RAISE EXCEPTION '%', blocker USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

-- A step that was recorded is never changed or cleared, and a completed
-- settlement is frozen whole.
CREATE OR REPLACE FUNCTION tenant_closure_settlement_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'completed' THEN
        IF to_jsonb(NEW) IS DISTINCT FROM to_jsonb(OLD) THEN
            RAISE EXCEPTION 'a completed closure settlement is frozen' USING ERRCODE = 'check_violation';
        END IF;
        RETURN NEW;
    END IF;
    IF (OLD.cancellation_completed_at IS NOT NULL AND NEW.cancellation_completed_at IS DISTINCT FROM OLD.cancellation_completed_at)
       OR (OLD.drain_completed_at IS NOT NULL AND NEW.drain_completed_at IS DISTINCT FROM OLD.drain_completed_at)
       OR (OLD.finance_completed_at IS NOT NULL AND NEW.finance_completed_at IS DISTINCT FROM OLD.finance_completed_at)
       OR (OLD.blocked_at IS NOT NULL AND NEW.blocked_at IS DISTINCT FROM OLD.blocked_at)
       OR (OLD.finance_invoice_id IS NOT NULL AND NEW.finance_invoice_id IS DISTINCT FROM OLD.finance_invoice_id) THEN
        RAISE EXCEPTION 'a recorded closure step cannot change' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status IS NOT DISTINCT FROM OLD.status THEN
        RETURN NEW;
    END IF;
    IF NOT ((OLD.status = 'cancelling' AND NEW.status = 'finance_pending')
         OR (OLD.status = 'finance_pending' AND NEW.status = 'blocking')
         OR (OLD.status = 'blocking' AND NEW.status = 'completed')) THEN
        RAISE EXCEPTION 'closure settlement cannot move from % to %', OLD.status, NEW.status USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'finance_pending' AND (NEW.cancellation_completed_at IS NULL OR NEW.drain_completed_at IS NULL OR NEW.finance_target_date IS NULL) THEN
        RAISE EXCEPTION 'closure settlement needs its cancellation, its usage drain and a target date before finance' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status IN ('blocking', 'completed') AND NOT EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o JOIN tenant_closure_invoice_attempts a ON a.id = o.attempt_id
        WHERE a.brand_id = NEW.brand_id AND a.tenant_id = NEW.tenant_id AND o.kind IN ('evidenced', 'no_invoice')
    ) THEN
        RAISE EXCEPTION 'closure settlement needs a settled invoice before %', NEW.status USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'blocking' AND NEW.finance_completed_at IS NULL THEN
        RAISE EXCEPTION 'closure settlement needs its finance step recorded before blocking' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'completed' AND (NEW.finance_completed_at IS NULL OR NEW.blocked_at IS NULL) THEN
        RAISE EXCEPTION 'closure settlement needs the account blocked before it completes' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

-- A closure settlement starts at cancelling. Nothing a session can set changes
-- that: a test database that needs a fixture at a later state disables this
-- trigger, which takes ownership of the table.
CREATE FUNCTION tenant_closure_settlement_start() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'cancelling' THEN
        RAISE EXCEPTION 'a closure settlement starts at cancelling' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_settlement_start BEFORE INSERT ON tenant_closure_settlements
FOR EACH ROW EXECUTE FUNCTION tenant_closure_settlement_start();
