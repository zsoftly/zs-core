ALTER TABLE tenant_closure_settlements ADD COLUMN drain_completed_at timestamptz;

CREATE TABLE tenant_closure_invoice_attempts (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    killbill_tenant_id uuid NOT NULL,
    killbill_account_id uuid NOT NULL,
    target_date date NOT NULL,
    status text NOT NULL DEFAULT 'intent' CHECK (status = 'intent'),
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenant_closure_settlements (brand_id, tenant_id),
    CHECK (status = 'intent')
);

CREATE FUNCTION tenant_closure_invoice_attempt_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' AND NEW.status <> 'intent' THEN
        RAISE EXCEPTION 'closure invoice attempt must begin as intent' USING ERRCODE = 'check_violation';
    END IF;
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'closure invoice attempt is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_invoice_attempt_guard BEFORE INSERT OR UPDATE OR DELETE ON tenant_closure_invoice_attempts
FOR EACH ROW EXECUTE FUNCTION tenant_closure_invoice_attempt_guard();
CREATE TRIGGER tenant_closure_invoice_attempt_no_truncate BEFORE TRUNCATE ON tenant_closure_invoice_attempts
FOR EACH STATEMENT EXECUTE FUNCTION tenant_closure_invoice_attempt_guard();
