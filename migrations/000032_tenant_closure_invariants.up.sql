-- A completed closure settlement and a closed tenant go together. Both rules are
-- deferred to commit, so the update of the tenant and the update of the
-- settlement made in one transaction satisfy them, and a code path that makes
-- only one of them cannot commit.

-- A settlement that is completed needs a tenant that is closed.
CREATE FUNCTION tenant_closure_completed_needs_closed() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.status = 'completed'
       AND NOT EXISTS (SELECT 1 FROM tenants WHERE brand_id = NEW.brand_id AND id = NEW.tenant_id AND status = 'closed') THEN
        RAISE EXCEPTION 'a completed closure settlement needs a closed tenant' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NULL;
END;
$$;
CREATE CONSTRAINT TRIGGER tenant_closure_completed_needs_closed AFTER INSERT OR UPDATE ON tenant_closure_settlements
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION tenant_closure_completed_needs_closed();

-- A tenant that is closing leaves that status only by closing, and only with a
-- completed settlement. A tenant that has a settlement is closed only with a
-- completed one. A tenant with no settlement can still be closed: that is the
-- hard close.
CREATE FUNCTION tenant_closed_needs_completed_closure() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
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
CREATE CONSTRAINT TRIGGER tenant_closed_needs_completed_closure AFTER UPDATE OF status ON tenants
DEFERRABLE INITIALLY DEFERRED FOR EACH ROW WHEN (OLD.status IS DISTINCT FROM NEW.status)
EXECUTE FUNCTION tenant_closed_needs_completed_closure();

ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_completed_needs_closed;
ALTER TABLE tenants ENABLE ALWAYS TRIGGER tenant_closed_needs_completed_closure;
