-- A closure settlement moves one state at a time, in the order cancelling,
-- finance_pending, blocking, completed, and each state needs what the earlier
-- ones recorded. The review guard of migration 26 still decides completion.
CREATE FUNCTION tenant_closure_settlement_order() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
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
CREATE TRIGGER tenant_closure_settlement_order BEFORE UPDATE ON tenant_closure_settlements
FOR EACH ROW EXECUTE FUNCTION tenant_closure_settlement_order();
