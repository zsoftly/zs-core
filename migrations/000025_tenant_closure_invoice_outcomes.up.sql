CREATE TABLE tenant_closure_invoice_outcomes (
    id uuid PRIMARY KEY,
    attempt_id uuid NOT NULL REFERENCES tenant_closure_invoice_attempts (id),
    kind text NOT NULL CHECK (kind IN ('started', 'post_unknown', 'no_invoice', 'invoice_id', 'evidenced', 'read_mismatch')),
    invoice_id uuid,
    expected_currency text,
    created_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    UNIQUE (attempt_id, kind),
    CHECK (
        (kind = 'started' AND invoice_id IS NULL AND expected_currency IS NOT NULL AND expected_currency ~ '^[A-Z]{3}$')
        OR (kind IN ('post_unknown', 'no_invoice') AND invoice_id IS NULL AND expected_currency IS NULL)
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

CREATE FUNCTION tenant_closure_invoice_outcome_tie() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.kind IN ('evidenced', 'read_mismatch') AND NOT EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o
        WHERE o.attempt_id = NEW.attempt_id AND o.kind = 'invoice_id' AND o.invoice_id = NEW.invoice_id
    ) THEN
        RAISE EXCEPTION 'closure invoice outcome must name the recorded invoice' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_invoice_outcome_tie BEFORE INSERT ON tenant_closure_invoice_outcomes
FOR EACH ROW EXECUTE FUNCTION tenant_closure_invoice_outcome_tie();

CREATE FUNCTION tenant_closure_invoice_outcome_exclusive() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Take the attempt lock first, so two concurrent inserts are serialized
    -- here and the second sees the first, whatever the caller did before.
    PERFORM 1 FROM tenant_closure_invoice_attempts WHERE id = NEW.attempt_id FOR UPDATE;
    IF NEW.kind = 'no_invoice' AND EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o WHERE o.attempt_id = NEW.attempt_id AND o.kind IN ('started', 'invoice_id', 'post_unknown')
    ) THEN
        RAISE EXCEPTION 'closure invoice already started, so it cannot have nothing to invoice' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.kind IN ('started', 'invoice_id') AND EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o WHERE o.attempt_id = NEW.attempt_id AND o.kind = 'no_invoice'
    ) THEN
        RAISE EXCEPTION 'closure invoice has nothing to invoice, so it cannot start' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_invoice_outcome_exclusive BEFORE INSERT ON tenant_closure_invoice_outcomes
FOR EACH ROW EXECUTE FUNCTION tenant_closure_invoice_outcome_exclusive();
