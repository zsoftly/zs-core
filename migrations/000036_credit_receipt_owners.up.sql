-- A Kill Bill receipt, the invoice item of a credit, belongs to one credit,
-- whichever source made it: a code redemption or a staff grant. The owner is
-- recorded in the same transaction that puts the receipt on the credit, and a
-- second credit that would take the same receipt is refused, also when two
-- transactions race for it, because the owner row is a primary key.
CREATE TABLE credit_receipt_owners (
    killbill_credit_id uuid PRIMARY KEY,
    source text NOT NULL CHECK (source IN ('credit_code_redemptions', 'staff_credits')),
    row_id uuid NOT NULL UNIQUE
);
INSERT INTO credit_receipt_owners (killbill_credit_id, source, row_id)
SELECT killbill_credit_id, 'credit_code_redemptions', id FROM credit_code_redemptions WHERE killbill_credit_id IS NOT NULL
ON CONFLICT DO NOTHING;
INSERT INTO credit_receipt_owners (killbill_credit_id, source, row_id)
SELECT killbill_credit_id, 'staff_credits', id FROM staff_credits WHERE killbill_credit_id IS NOT NULL
ON CONFLICT DO NOTHING;

CREATE FUNCTION credit_receipt_claim() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.killbill_credit_id IS NULL THEN
        RETURN NEW;
    END IF;
    IF TG_OP = 'UPDATE' AND NEW.killbill_credit_id IS NOT DISTINCT FROM OLD.killbill_credit_id THEN
        RETURN NEW;
    END IF;
    BEGIN
        INSERT INTO credit_receipt_owners (killbill_credit_id, source, row_id) VALUES (NEW.killbill_credit_id, TG_TABLE_NAME, NEW.id);
    EXCEPTION WHEN unique_violation THEN
        RAISE EXCEPTION 'a credit receipt belongs to one credit' USING ERRCODE = 'check_violation';
    END;
    RETURN NEW;
END;
$$;
CREATE TRIGGER credit_code_redemptions_receipt_claim BEFORE INSERT OR UPDATE OF killbill_credit_id ON credit_code_redemptions
FOR EACH ROW EXECUTE FUNCTION credit_receipt_claim();
CREATE TRIGGER staff_credits_receipt_claim BEFORE INSERT OR UPDATE OF killbill_credit_id ON staff_credits
FOR EACH ROW EXECUTE FUNCTION credit_receipt_claim();

CREATE FUNCTION credit_receipt_owner_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'the owner of a credit receipt never changes' USING ERRCODE = 'check_violation';
END;
$$;
CREATE TRIGGER credit_receipt_owner_no_change BEFORE UPDATE OR DELETE ON credit_receipt_owners
FOR EACH ROW EXECUTE FUNCTION credit_receipt_owner_immutable();
CREATE TRIGGER credit_receipt_owner_no_truncate BEFORE TRUNCATE ON credit_receipt_owners
FOR EACH STATEMENT EXECUTE FUNCTION credit_receipt_owner_immutable();

ALTER TABLE credit_code_redemptions ENABLE ALWAYS TRIGGER credit_code_redemptions_receipt_claim;
ALTER TABLE staff_credits ENABLE ALWAYS TRIGGER staff_credits_receipt_claim;
ALTER TABLE credit_receipt_owners ENABLE ALWAYS TRIGGER credit_receipt_owner_no_change;
ALTER TABLE credit_receipt_owners ENABLE ALWAYS TRIGGER credit_receipt_owner_no_truncate;
