-- A closure settlement starts empty: at cancelling, with no step recorded, no
-- finance data, no error and no attempt. Nothing can start a settlement that
-- already claims to be partly done.
CREATE OR REPLACE FUNCTION tenant_closure_settlement_start() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status <> 'cancelling'
       OR NEW.cancellation_completed_at IS NOT NULL OR NEW.drain_completed_at IS NOT NULL
       OR NEW.finance_requested_at IS NOT NULL OR NEW.finance_attempt_id IS NOT NULL
       OR NEW.finance_target_date IS NOT NULL OR NEW.finance_invoice_id IS NOT NULL
       OR NEW.finance_completed_at IS NOT NULL OR NEW.blocked_at IS NOT NULL
       OR NEW.completed_at IS NOT NULL OR NEW.last_error IS NOT NULL OR NEW.attempts <> 0 THEN
        RAISE EXCEPTION 'a closure settlement starts at cancelling with nothing recorded' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;

-- The settlement rules fire in every session replication role, so a session
-- that sets session_replication_role to replica does not skip them. Disabling
-- one needs ownership of the table, which an application role lacks.
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_settlement_start;
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_settlement_order;
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_complete_guard;
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_settlements_cutoff_immutable;
