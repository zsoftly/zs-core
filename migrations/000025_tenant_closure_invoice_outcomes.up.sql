CREATE TABLE tenant_closure_invoice_outcomes (
    id uuid PRIMARY KEY,
    attempt_id uuid NOT NULL REFERENCES tenant_closure_invoice_attempts (id),
    kind text NOT NULL CHECK (kind IN ('started', 'post_unknown', 'invoice_id', 'evidenced', 'read_mismatch')),
    invoice_id uuid,
    expected_currency text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (attempt_id, kind),
    CHECK (
        (kind = 'started' AND invoice_id IS NULL AND expected_currency IS NOT NULL AND expected_currency ~ '^[A-Z]{3}$')
        OR (kind = 'post_unknown' AND invoice_id IS NULL AND expected_currency IS NULL)
        OR (kind IN ('invoice_id', 'evidenced', 'read_mismatch') AND invoice_id IS NOT NULL AND expected_currency IS NULL)
    )
);

CREATE FUNCTION tenant_closure_invoice_outcome_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP <> 'INSERT' THEN
        RAISE EXCEPTION 'closure invoice outcome is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_invoice_outcome_guard BEFORE UPDATE OR DELETE ON tenant_closure_invoice_outcomes
FOR EACH ROW EXECUTE FUNCTION tenant_closure_invoice_outcome_guard();
CREATE TRIGGER tenant_closure_invoice_outcome_no_truncate BEFORE TRUNCATE ON tenant_closure_invoice_outcomes
FOR EACH STATEMENT EXECUTE FUNCTION tenant_closure_invoice_outcome_guard();
