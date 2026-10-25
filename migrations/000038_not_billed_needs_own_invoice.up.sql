-- Recording usage as not billed says Kill Bill has not billed it and will not.
-- Kill Bill makes the invoice of a cancel a little after the cancel, so the
-- cancellation being done proves nothing: the settlement must be past the
-- finance step, whose own invoice trigger ran after the cancel. The application
-- also reads the invoice of the cancel, which the database cannot.
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
                       WHERE s.brand_id = NEW.brand_id AND s.tenant_id = NEW.tenant_id
                         AND s.status IN ('blocking', 'completed') AND s.finance_completed_at IS NOT NULL) THEN
            RAISE EXCEPTION 'usage is recorded as not billed only once the finance step of the closure is done' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NEW;
END;
$$;
