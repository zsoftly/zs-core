CREATE TABLE sweep_cursor (
    kind text PRIMARY KEY CHECK (kind IN ('creation', 'closure')),
    last_eligible_at timestamptz,
    last_tenant_id uuid,
    updated_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE credit_code_redemptions ADD COLUMN original_endpoint_kind text;
ALTER TABLE credit_code_redemptions ADD COLUMN original_killbill_tenant_id uuid;
ALTER TABLE credit_code_redemptions ADD COLUMN original_killbill_account_id uuid;
ALTER TABLE credit_code_redemptions ADD COLUMN original_currency text;
ALTER TABLE credit_code_redemptions ADD COLUMN original_amount_minor bigint;
ALTER TABLE credit_code_redemptions ADD COLUMN original_description text;
ALTER TABLE staff_credits ADD COLUMN original_endpoint_kind text;
ALTER TABLE staff_credits ADD COLUMN original_killbill_tenant_id uuid;
ALTER TABLE staff_credits ADD COLUMN original_killbill_account_id uuid;
ALTER TABLE staff_credits ADD COLUMN original_currency text;
ALTER TABLE staff_credits ADD COLUMN original_amount_minor bigint;
ALTER TABLE staff_credits ADD COLUMN original_description text;

CREATE FUNCTION credit_original_target_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'INSERT' THEN
        IF (NEW.original_endpoint_kind IS NULL AND NEW.original_killbill_tenant_id IS NULL AND NEW.original_killbill_account_id IS NULL AND NEW.original_currency IS NULL AND NEW.original_amount_minor IS NULL AND NEW.original_description IS NULL)
           OR (NEW.original_endpoint_kind IN ('redeem', 'grant') AND NEW.original_killbill_tenant_id IS NOT NULL AND NEW.original_killbill_account_id IS NOT NULL AND NEW.original_currency ~ '^[A-Z]{3}$' AND NEW.original_amount_minor > 0 AND NEW.original_description IS NOT NULL) THEN
            RETURN NEW;
        END IF;
        RAISE EXCEPTION 'original credit target is incomplete' USING ERRCODE = 'check_violation';
    END IF;
    IF (NEW.original_endpoint_kind, NEW.original_killbill_tenant_id, NEW.original_killbill_account_id, NEW.original_currency, NEW.original_amount_minor, NEW.original_description)
       IS DISTINCT FROM
       (OLD.original_endpoint_kind, OLD.original_killbill_tenant_id, OLD.original_killbill_account_id, OLD.original_currency, OLD.original_amount_minor, OLD.original_description) THEN
        RAISE EXCEPTION 'original credit target is immutable' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER credit_code_redemptions_original_target_immutable BEFORE INSERT OR UPDATE ON credit_code_redemptions FOR EACH ROW EXECUTE FUNCTION credit_original_target_immutable();
CREATE TRIGGER staff_credits_original_target_immutable BEFORE INSERT OR UPDATE ON staff_credits FOR EACH ROW EXECUTE FUNCTION credit_original_target_immutable();

CREATE TABLE tenant_closure_bundle_holds (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    id uuid NOT NULL,
    cutoff_at timestamptz NOT NULL,
    killbill_tenant_id uuid NOT NULL,
    killbill_account_id uuid NOT NULL,
    bundle_id uuid NOT NULL,
    base_subscription_id uuid,
    reason text NOT NULL CHECK (reason IN ('already_cancelled_base', 'addon_active', 'addon_ineligible', 'addon_unknown')),
    evidence jsonb NOT NULL CHECK (jsonb_typeof(evidence) = 'object'),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    correlation_id text NOT NULL,
    PRIMARY KEY (brand_id, tenant_id, id),
    UNIQUE (id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenant_closure_settlements (brand_id, tenant_id),
    UNIQUE (brand_id, tenant_id, cutoff_at, bundle_id, reason),
    CHECK (reason <> 'already_cancelled_base' OR base_subscription_id IS NOT NULL)
);
CREATE FUNCTION tenant_closure_bundle_hold_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'closure bundle hold evidence is immutable'; END;
$$;
CREATE TRIGGER tenant_closure_bundle_hold_no_change BEFORE UPDATE OR DELETE ON tenant_closure_bundle_holds FOR EACH ROW EXECUTE FUNCTION tenant_closure_bundle_hold_immutable();
CREATE TRIGGER tenant_closure_bundle_hold_no_truncate BEFORE TRUNCATE ON tenant_closure_bundle_holds FOR EACH STATEMENT EXECUTE FUNCTION tenant_closure_bundle_hold_immutable();

CREATE TABLE closure_hold_resolution (
    hold_id uuid PRIMARY KEY,
    decision text NOT NULL CHECK (decision IN ('release', 'manual_forfeit')),
    resolved_by text NOT NULL,
    resolved_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    upstream_readback_sha256 text NOT NULL CHECK (upstream_readback_sha256 ~ '^[0-9a-f]{64}$'),
    FOREIGN KEY (hold_id) REFERENCES tenant_closure_bundle_holds (id)
);
CREATE FUNCTION closure_hold_resolution_immutable() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN RAISE EXCEPTION 'closure hold resolution is immutable'; END;
$$;
CREATE TRIGGER closure_hold_resolution_no_change BEFORE UPDATE OR DELETE ON closure_hold_resolution FOR EACH ROW EXECUTE FUNCTION closure_hold_resolution_immutable();
CREATE TRIGGER closure_hold_resolution_no_truncate BEFORE TRUNCATE ON closure_hold_resolution FOR EACH STATEMENT EXECUTE FUNCTION closure_hold_resolution_immutable();
