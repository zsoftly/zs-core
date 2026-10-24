-- The start record of a closure is all there or all absent, and a change to it
-- is a check violation like the other rules of the settlement.
ALTER TABLE tenant_closure_settlements ADD CONSTRAINT tenant_closure_settlements_start_record_complete CHECK (
    (started_by IS NULL AND start_reason IS NULL AND start_request_id IS NULL)
    OR (started_by IS NOT NULL AND start_reason IS NOT NULL AND start_request_id IS NOT NULL));

CREATE OR REPLACE FUNCTION tenant_closure_settlements_keep_start_record() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.started_by IS DISTINCT FROM OLD.started_by
       OR NEW.start_reason IS DISTINCT FROM OLD.start_reason
       OR NEW.start_request_id IS DISTINCT FROM OLD.start_request_id THEN
        RAISE EXCEPTION 'the start record of a closure is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
