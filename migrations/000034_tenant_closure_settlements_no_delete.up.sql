-- A closure settlement is evidence of what was settled for a tenant, so it is
-- never deleted. The rule fires in every session replication role.
CREATE FUNCTION tenant_closure_settlements_refuse_delete() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'a closure settlement is never deleted' USING ERRCODE = 'check_violation';
END;
$$;
CREATE TRIGGER tenant_closure_settlements_no_delete BEFORE DELETE ON tenant_closure_settlements
FOR EACH ROW EXECUTE FUNCTION tenant_closure_settlements_refuse_delete();
ALTER TABLE tenant_closure_settlements ENABLE ALWAYS TRIGGER tenant_closure_settlements_no_delete;
