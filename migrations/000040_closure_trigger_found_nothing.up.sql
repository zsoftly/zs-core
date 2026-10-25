-- The closure invoice trigger can answer the bare 404 of nothing to invoice
-- after the dry run showed something, because Kill Bill made the invoice of the
-- cancel in between. When a second dry run finds nothing left and the account
-- has no invoice of the cancel, no_invoice is recorded after the start. It is
-- still ruled out by an invoice ID or an unknown outcome, and a start is still
-- ruled out by no_invoice, so a real invoice is never hidden. An unknown
-- outcome is also ruled out after no_invoice.
CREATE OR REPLACE FUNCTION tenant_closure_invoice_outcome_exclusive() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    -- Take the attempt lock first, so two concurrent inserts are serialized
    -- here and the second sees the first, whatever the caller did before.
    PERFORM 1 FROM tenant_closure_invoice_attempts WHERE id = NEW.attempt_id FOR UPDATE;
    IF NEW.kind = 'no_invoice' AND EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o WHERE o.attempt_id = NEW.attempt_id AND o.kind IN ('invoice_id', 'post_unknown')
    ) THEN
        RAISE EXCEPTION 'closure invoice has an invoice or an unknown outcome, so it cannot have nothing to invoice' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.kind IN ('started', 'invoice_id', 'post_unknown') AND EXISTS (
        SELECT 1 FROM tenant_closure_invoice_outcomes o WHERE o.attempt_id = NEW.attempt_id AND o.kind = 'no_invoice'
    ) THEN
        RAISE EXCEPTION 'closure invoice has nothing to invoice, so it cannot start' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
