-- A closure records who asked for it, why, and under which request ID, so a
-- repeated request is told from a different one. The three columns are set
-- when the settlement starts and never change.
ALTER TABLE tenant_closure_settlements
    ADD COLUMN started_by text,
    ADD COLUMN start_reason text,
    ADD COLUMN start_request_id text,
    ADD CONSTRAINT tenant_closure_settlements_started_by_length CHECK (started_by IS NULL OR char_length(started_by) BETWEEN 1 AND 300),
    ADD CONSTRAINT tenant_closure_settlements_start_reason_length CHECK (start_reason IS NULL OR char_length(start_reason) BETWEEN 1 AND 1000),
    ADD CONSTRAINT tenant_closure_settlements_start_request_length CHECK (start_request_id IS NULL OR char_length(start_request_id) BETWEEN 8 AND 255);

CREATE FUNCTION tenant_closure_settlements_keep_start_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.started_by IS DISTINCT FROM OLD.started_by
       OR NEW.start_reason IS DISTINCT FROM OLD.start_reason
       OR NEW.start_request_id IS DISTINCT FROM OLD.start_request_id THEN
        RAISE EXCEPTION 'the start record of a closure is immutable' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_settlements_start_record_immutable BEFORE UPDATE ON tenant_closure_settlements
FOR EACH ROW EXECUTE FUNCTION tenant_closure_settlements_keep_start_record();
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_settlements_start_record_immutable;

-- Closing a tenant is destructive, so it is its own grant and does not ride on
-- another one.
ALTER TABLE staff_grants DROP CONSTRAINT staff_grants_scope;
ALTER TABLE staff_grants ADD CONSTRAINT staff_grants_scope CHECK (scope IN ('operator', 'activation', 'billing', 'support', 'closure'));
