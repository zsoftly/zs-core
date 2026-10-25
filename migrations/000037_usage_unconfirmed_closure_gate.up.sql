-- A send that started and was never confirmed holds nothing back on its own: no
-- invoice shows its tracking ID until Kill Bill bills the usage, which is at the
-- period end or at the cancel, so a closure that waited for it before it cancels
-- would wait for ever. The closure cancels, and the repair invoice bills what
-- Kill Bill holds. What staff must still do is say what became of the usage, and
-- that now gates completion, as legacy usage does.

-- A staff decision is either that Kill Bill bills the record (forwarded, with
-- the invoice that carries it) or that it does not (not_billed, with the quantity
-- of the frozen rollup that was lost).
ALTER TABLE usage_rollup_resolutions ADD COLUMN unbilled_quantity_micro bigint;
ALTER TABLE usage_rollup_resolutions ALTER COLUMN invoice_id DROP NOT NULL;
ALTER TABLE usage_rollup_resolutions DROP CONSTRAINT usage_rollup_resolutions_decision_check;
ALTER TABLE usage_rollup_resolutions ADD CONSTRAINT usage_rollup_resolutions_decision_check CHECK (decision IN ('forwarded', 'not_billed'));
ALTER TABLE usage_rollup_resolutions ADD CONSTRAINT usage_rollup_resolutions_shape CHECK (
    (decision = 'forwarded' AND invoice_id IS NOT NULL AND unbilled_quantity_micro IS NULL)
    OR (decision = 'not_billed' AND invoice_id IS NULL AND unbilled_quantity_micro > 0));

CREATE OR REPLACE FUNCTION usage_rollup_resolution_guard() RETURNS trigger LANGUAGE plpgsql AS $$
DECLARE
    quantity bigint;
BEGIN
    SELECT r.quantity_micro INTO quantity FROM usage_rollups r
        WHERE r.brand_id = NEW.brand_id AND r.tenant_id = NEW.tenant_id AND r.id = NEW.rollup_id
          AND r.send_protocol = 1 AND r.status = 'pending' AND r.send_started_at IS NOT NULL
          AND r.closure_unresolved_reason IN ('usage_send_unconfirmed', 'usage_duplicate_unconfirmed', 'usage_transport_unconfirmed');
    IF NOT FOUND THEN
        RAISE EXCEPTION 'only an unconfirmed send can be resolved' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.decision = 'not_billed' THEN
        IF NEW.unbilled_quantity_micro IS DISTINCT FROM quantity THEN
            RAISE EXCEPTION 'the unbilled quantity is the quantity of the rollup' USING ERRCODE = 'check_violation';
        END IF;
        IF NOT EXISTS (SELECT 1 FROM tenant_closure_settlements s
                       WHERE s.brand_id = NEW.brand_id AND s.tenant_id = NEW.tenant_id AND s.cancellation_completed_at IS NOT NULL) THEN
            RAISE EXCEPTION 'usage is recorded as not billed only after the cancellation' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;

-- The review gate of completion also holds for usage whose send was never
-- confirmed, until staff resolve it.
CREATE OR REPLACE FUNCTION tenant_closure_review_blocker(p_brand uuid, p_tenant uuid) RETURNS text LANGUAGE plpgsql STABLE AS $$
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
    IF EXISTS (SELECT 1 FROM usage_rollups u WHERE u.brand_id = p_brand AND u.tenant_id = p_tenant
               AND u.send_protocol = 1 AND u.status = 'pending' AND u.send_started_at IS NOT NULL
               AND u.closure_unresolved_reason IN ('usage_send_unconfirmed', 'usage_duplicate_unconfirmed', 'usage_transport_unconfirmed')
               AND NOT EXISTS (SELECT 1 FROM usage_rollup_resolutions x WHERE x.rollup_id = u.id)) THEN
        RETURN 'closure has usage whose send Kill Bill never confirmed and staff have not resolved';
    END IF;
    RETURN NULL;
END;
$$;
