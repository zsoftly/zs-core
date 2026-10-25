-- A tenant is closing only with a closure settlement to settle it, checked at
-- commit so that the status and the settlement can be written in one
-- transaction.
CREATE FUNCTION tenant_closing_needs_settlement() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'closing'
       AND NOT EXISTS (SELECT 1 FROM tenant_closure_settlements WHERE brand_id = NEW.brand_id AND tenant_id = NEW.id) THEN
        RAISE EXCEPTION 'a closing tenant needs a closure settlement' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER tenant_closing_needs_settlement AFTER INSERT OR UPDATE OF status ON tenants
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (NEW.status = 'closing')
EXECUTE FUNCTION tenant_closing_needs_settlement();
ALTER TABLE tenants ENABLE ALWAYS TRIGGER tenant_closing_needs_settlement;

-- Closed is terminal: a closed tenant never leaves that status.
CREATE OR REPLACE FUNCTION tenant_closed_needs_completed_closure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF OLD.status = 'closed' AND NEW.status <> 'closed' THEN
        RAISE EXCEPTION 'a closed tenant stays closed' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.status = 'closing' AND NEW.status NOT IN ('closing', 'closed') THEN
        RAISE EXCEPTION 'a closing tenant leaves that status only by closing' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.status = 'closed' AND OLD.status <> 'closed' THEN
        IF NOT EXISTS (SELECT 1 FROM tenant_closure_settlements WHERE brand_id = NEW.brand_id AND tenant_id = NEW.id AND status = 'completed')
           AND (OLD.status = 'closing' OR EXISTS (SELECT 1 FROM tenant_closure_settlements WHERE brand_id = NEW.brand_id AND tenant_id = NEW.id)) THEN
            RAISE EXCEPTION 'a tenant with a closure is closed only by a completed settlement' USING ERRCODE = 'check_violation';
        END IF;
    END IF;
    RETURN NULL;
END;
$$;
