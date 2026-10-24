ALTER TABLE tenants DROP CONSTRAINT tenants_status;
ALTER TABLE tenants ADD CONSTRAINT tenants_status CHECK (status IN ('active', 'suspended', 'closing', 'closed'));

CREATE TABLE tenant_closure_settlements (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    cutoff_at timestamptz NOT NULL,
    status text NOT NULL DEFAULT 'cancelling',
    cancellation_completed_at timestamptz,
    finance_requested_at timestamptz,
    finance_attempt_id uuid,
    finance_target_date date,
    finance_invoice_id uuid,
    finance_completed_at timestamptz,
    blocked_at timestamptz,
    completed_at timestamptz,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    last_error text,
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT tenant_closure_settlements_status CHECK (status IN ('cancelling', 'finance_pending', 'blocking', 'completed')),
    CONSTRAINT tenant_closure_settlements_error_length CHECK (last_error IS NULL OR char_length(last_error) <= 200)
);
CREATE INDEX tenant_closure_settlements_pending_idx ON tenant_closure_settlements (next_attempt_at) WHERE status <> 'completed';
CREATE FUNCTION tenant_closure_settlements_keep_cutoff() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.cutoff_at <> OLD.cutoff_at THEN RAISE EXCEPTION 'tenant closure cutoff is immutable' USING ERRCODE = 'insufficient_privilege'; END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER tenant_closure_settlements_cutoff_immutable BEFORE UPDATE ON tenant_closure_settlements FOR EACH ROW EXECUTE FUNCTION tenant_closure_settlements_keep_cutoff();

ALTER TABLE usage_events ADD COLUMN acceptance_protocol smallint NOT NULL DEFAULT 0;
ALTER TABLE usage_events ADD CONSTRAINT usage_events_acceptance_protocol_v1 CHECK (acceptance_protocol IN (0, 1));
ALTER TABLE usage_events ADD CONSTRAINT usage_events_raw_acceptance_v1 CHECK (acceptance_protocol = 0 OR reservation_id IS NULL);
ALTER TABLE usage_rollups ADD COLUMN send_protocol smallint NOT NULL DEFAULT 0;
ALTER TABLE usage_rollups ADD COLUMN event_set_fingerprint text;
ALTER TABLE usage_rollups ADD COLUMN target_endpoint_fingerprint text;
ALTER TABLE usage_rollups ADD COLUMN target_killbill_tenant_id uuid;
ALTER TABLE usage_rollups ADD COLUMN target_killbill_account_id uuid;
ALTER TABLE usage_rollups ADD COLUMN send_payload jsonb;
ALTER TABLE usage_rollups ADD COLUMN send_provenance text NOT NULL DEFAULT 'unknown';
ALTER TABLE usage_rollups ADD COLUMN send_started_at timestamptz;
ALTER TABLE usage_rollups ADD COLUMN claim_from_never boolean NOT NULL DEFAULT false;
ALTER TABLE usage_rollups ADD COLUMN closure_unresolved_reason text;
ALTER TABLE usage_rollups DROP CONSTRAINT usage_rollups_status;
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_status CHECK (status IN ('pending', 'forwarded', 'rejected', 'discarded', 'superseded'));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_send_protocol_v1 CHECK (send_protocol IN (0, 1));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_send_provenance CHECK (send_provenance IN ('unknown', 'never_attempted', 'possibly_attempted'));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_event_fingerprint_v1 CHECK (event_set_fingerprint IS NULL OR event_set_fingerprint ~ '^[0-9a-f]{64}$');
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_endpoint_fingerprint_v1 CHECK (target_endpoint_fingerprint IS NULL OR target_endpoint_fingerprint ~ '^[0-9a-f]{64}$');
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_evidence_v1 CHECK (send_protocol = 0 OR event_set_fingerprint IS NOT NULL);
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_target_v1 CHECK (send_protocol = 0 OR ((target_endpoint_fingerprint IS NULL AND target_killbill_tenant_id IS NULL AND target_killbill_account_id IS NULL) OR (target_endpoint_fingerprint IS NOT NULL AND target_killbill_tenant_id IS NOT NULL AND target_killbill_account_id IS NOT NULL AND subscription_id IS NOT NULL)));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_body_v1 CHECK (send_payload IS NULL OR jsonb_typeof(send_payload) = 'object');
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_start_body_v1 CHECK (send_protocol = 0 OR ((send_started_at IS NULL) = (send_payload IS NULL)));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_start_target_v1 CHECK (send_protocol = 0 OR send_started_at IS NULL OR (send_provenance = 'possibly_attempted' AND target_endpoint_fingerprint IS NOT NULL AND target_killbill_tenant_id IS NOT NULL AND target_killbill_account_id IS NOT NULL AND subscription_id IS NOT NULL));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_forward_receipt_v1 CHECK (send_protocol = 0 OR status <> 'forwarded' OR (send_started_at IS NOT NULL AND send_payload IS NOT NULL));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_claim_pair_v1 CHECK (send_protocol = 0 OR ((claim_token IS NULL) = (claimed_until IS NULL)));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_never_v1 CHECK (send_protocol = 0 OR send_provenance <> 'never_attempted' OR (claim_token IS NULL AND claimed_until IS NULL AND send_started_at IS NULL AND send_payload IS NULL AND NOT claim_from_never));
ALTER TABLE usage_rollups ADD CONSTRAINT usage_rollups_x_v1 CHECK (send_protocol = 0 OR status <> 'superseded' OR (send_provenance = 'never_attempted' AND send_started_at IS NULL AND claim_token IS NULL AND send_payload IS NULL));

CREATE OR REPLACE FUNCTION usage_rollups_preserve_possible_send() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE
 n boolean; c_shape boolean; c_live boolean; s_shape boolean;
 oldj jsonb; newj jsonb; same_target boolean; bind_target boolean;
 mixed boolean := false; now_at timestamptz := clock_timestamp();
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION USING ERRCODE='insufficient_privilege',MESSAGE='usage_rollup_delete_forbidden';
 END IF;
 IF TG_OP='INSERT' THEN
  -- I0: old writers default0, never granted a trusted first-send origin.
  IF NEW.send_protocol=0 THEN
   IF NEW.claim_token IS NOT NULL OR NEW.claimed_until IS NOT NULL THEN
    NEW.send_provenance := 'possibly_attempted'; NEW.claim_from_never := false;
   END IF;
   RETURN NEW;
  END IF;
  -- I1: complete new trusted unsent row; static checks independently validate shape.
  IF NEW.send_protocol=1 AND NEW.status='pending' AND NEW.send_provenance='never_attempted'
   AND NEW.claim_token IS NULL AND NEW.claimed_until IS NULL AND NOT NEW.claim_from_never
   AND NEW.send_started_at IS NULL AND NEW.send_payload IS NULL
   AND NEW.closure_unresolved_reason IS NULL AND NEW.event_set_fingerprint IS NOT NULL THEN RETURN NEW; END IF;
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_insert_forbidden';
 END IF;
 IF TG_OP<>'UPDATE' THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_operation_forbidden';
 END IF;
 oldj := to_jsonb(OLD); newj := to_jsonb(NEW);
 -- R00 exact no-op, including readback retry; it creates no new authority.
 IF newj=oldj THEN RETURN NEW; END IF;
 IF NEW.send_protocol IS DISTINCT FROM OLD.send_protocol OR
  (NEW.id,NEW.brand_id,NEW.tenant_id,NEW.killbill_unit,NEW.requested_subscription_id,
   NEW.day,NEW.period_start,NEW.record_at,NEW.quantity_micro,NEW.events,NEW.tracking_id,
   NEW.created_at,NEW.event_set_fingerprint)
  IS DISTINCT FROM
  (OLD.id,OLD.brand_id,OLD.tenant_id,OLD.killbill_unit,OLD.requested_subscription_id,
   OLD.day,OLD.period_start,OLD.record_at,OLD.quantity_micro,OLD.events,OLD.tracking_id,
   OLD.created_at,OLD.event_set_fingerprint) THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
 END IF;
 IF OLD.closure_unresolved_reason IS NOT NULL AND NEW.closure_unresolved_reason IS DISTINCT FROM OLD.closure_unresolved_reason THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
 END IF;
 -- Read only already application-locked tenant/cutoff/events. Never acquire reverse-order row locks here.
 SELECT EXISTS(
  SELECT 1 FROM tenants t JOIN tenant_closure_settlements k ON k.brand_id=t.brand_id AND k.tenant_id=t.id
  WHERE t.brand_id=OLD.brand_id AND t.id=OLD.tenant_id AND t.status='closing'
   AND EXISTS(SELECT 1 FROM usage_events e WHERE e.brand_id=OLD.brand_id AND e.tenant_id=OLD.tenant_id
    AND e.rollup_id=OLD.id AND e.reservation_id IS NULL AND e.occurred_at<=k.cutoff_at AND e.received_at<=k.cutoff_at)
   AND EXISTS(SELECT 1 FROM usage_events e WHERE e.brand_id=OLD.brand_id AND e.tenant_id=OLD.tenant_id
    AND e.rollup_id=OLD.id AND e.reservation_id IS NULL AND (e.occurred_at>k.cutoff_at OR e.received_at>k.cutoff_at))
 ) INTO mixed;
 -- R01: F's SOLE mutable field is NULL->exact mixed diagnostic, every other column frozen.
 IF OLD.status='forwarded' THEN
  IF mixed AND OLD.closure_unresolved_reason IS NULL AND NEW.closure_unresolved_reason='mixed_cutoff_send_unconfirmed'
   AND (newj-'closure_unresolved_reason')=(oldj-'closure_unresolved_reason') THEN RETURN NEW; END IF;
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
 END IF;
 -- R02: protocol0 legacy diagnostic only, no reset, promotion, target or receipt update.
 IF OLD.send_protocol=0 THEN
  IF OLD.closure_unresolved_reason IS NULL
   AND ((NEW.closure_unresolved_reason='legacy_usage_send_unconfirmed' AND OLD.status='pending')
    OR (NEW.closure_unresolved_reason='unknown_subscription_send_history' AND OLD.status='pending' AND OLD.subscription_id IS NULL)
    OR (NEW.closure_unresolved_reason='mixed_cutoff_send_unconfirmed' AND mixed AND OLD.status<>'superseded'))
   AND (NEW.last_error IS NOT DISTINCT FROM OLD.last_error OR NEW.last_error=NEW.closure_unresolved_reason)
   AND (newj-ARRAY['closure_unresolved_reason','last_error'])=(oldj-ARRAY['closure_unresolved_reason','last_error']) THEN RETURN NEW; END IF;
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
 END IF;
 IF OLD.send_protocol<>1 OR OLD.status IN('superseded','discarded') THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
 END IF;
 n := coalesce(OLD.status IN('pending','rejected') AND OLD.send_provenance='never_attempted'
  AND OLD.claim_token IS NULL AND OLD.claimed_until IS NULL AND NOT OLD.claim_from_never
  AND OLD.send_started_at IS NULL AND OLD.send_payload IS NULL AND OLD.closure_unresolved_reason IS NULL,false);
 c_shape := coalesce(OLD.status='pending' AND OLD.send_provenance='possibly_attempted' AND OLD.claim_from_never
  AND OLD.claim_token IS NOT NULL AND OLD.claimed_until IS NOT NULL
  AND OLD.send_started_at IS NULL AND OLD.send_payload IS NULL AND OLD.closure_unresolved_reason IS NULL,false);
 c_live := c_shape AND OLD.claimed_until>now_at;
 s_shape := coalesce(OLD.status='pending' AND OLD.send_provenance='possibly_attempted'
  AND OLD.send_started_at IS NOT NULL AND OLD.send_payload IS NOT NULL AND OLD.claim_token IS NOT NULL
  AND OLD.subscription_id IS NOT NULL AND OLD.target_endpoint_fingerprint IS NOT NULL
  AND OLD.target_killbill_tenant_id IS NOT NULL AND OLD.target_killbill_account_id IS NOT NULL,false);
 same_target := (NEW.subscription_id,NEW.target_endpoint_fingerprint,NEW.target_killbill_tenant_id,NEW.target_killbill_account_id)
  IS NOT DISTINCT FROM (OLD.subscription_id,OLD.target_endpoint_fingerprint,OLD.target_killbill_tenant_id,OLD.target_killbill_account_id);
 bind_target := NEW.subscription_id IS NOT NULL AND NEW.target_endpoint_fingerprint IS NOT NULL
  AND NEW.target_killbill_tenant_id IS NOT NULL AND NEW.target_killbill_account_id IS NOT NULL
  AND (OLD.subscription_id IS NULL OR NEW.subscription_id IS NOT DISTINCT FROM OLD.subscription_id)
  AND (OLD.target_endpoint_fingerprint IS NULL OR NEW.target_endpoint_fingerprint IS NOT DISTINCT FROM OLD.target_endpoint_fingerprint)
  AND (OLD.target_killbill_tenant_id IS NULL OR NEW.target_killbill_tenant_id IS NOT DISTINCT FROM OLD.target_killbill_tenant_id)
  AND (OLD.target_killbill_account_id IS NULL OR NEW.target_killbill_account_id IS NOT DISTINCT FROM OLD.target_killbill_account_id);
 -- R03: N->C, durable prior-never receipt issued by application ONLY after known COMMIT.
 IF n AND OLD.status='pending' AND NEW.status='pending' AND NEW.send_provenance='possibly_attempted'
  AND NEW.claim_from_never AND NEW.claim_token IS NOT NULL AND NEW.claimed_until>now_at
  AND (newj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance'])=
      (oldj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance']) THEN RETURN NEW; END IF;
 -- R04: first full target bind on N; R05: same current live C target bind.
 IF ((n AND OLD.status='pending') OR c_live) AND bind_target
  AND (newj-ARRAY['subscription_id','target_endpoint_fingerprint','target_killbill_tenant_id','target_killbill_account_id'])=
      (oldj-ARRAY['subscription_id','target_endpoint_fingerprint','target_killbill_tenant_id','target_killbill_account_id']) THEN RETURN NEW; END IF;
 -- R06: C->S exactly once, full stored body + target, token/deadline and every other column unchanged.
 IF c_live AND same_target AND OLD.subscription_id IS NOT NULL AND OLD.target_endpoint_fingerprint IS NOT NULL
  AND OLD.target_killbill_tenant_id IS NOT NULL AND OLD.target_killbill_account_id IS NOT NULL
  AND NEW.send_started_at IS NOT NULL AND NEW.send_payload IS NOT NULL AND jsonb_typeof(NEW.send_payload)='object'
  AND (newj-ARRAY['send_started_at','send_payload'])=(oldj-ARRAY['send_started_at','send_payload']) THEN RETURN NEW; END IF;
 -- R07: live pre-S same-token renewal only. Expiry never restores authority.
 IF c_live AND NEW.claimed_until>OLD.claimed_until
  AND (newj-'claimed_until')=(oldj-'claimed_until') THEN RETURN NEW; END IF;
 IF c_live AND NEW.claim_token IS NULL AND NEW.claimed_until IS NULL AND NOT NEW.claim_from_never
  AND NEW.send_provenance='never_attempted' AND same_target THEN
  -- R08: exact current owner's positive pre-POST refusal -> pending N, no other change.
  IF (newj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance'])=
      (oldj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance']) THEN RETURN NEW; END IF;
  -- R09: positive preflight retry with bounded safe Core reason/backoff, not a post-S retry.
  IF NEW.status='pending' AND NEW.attempts::bigint=OLD.attempts::bigint+1 AND NEW.next_attempt_at>now_at
   AND NEW.last_error IN('usage_preflight_unavailable','usage_payload_not_ready','usage_subscription_unconfirmed','usage_target_unconfirmed')
   AND (newj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','attempts','next_attempt_at','last_error'])=
       (oldj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','attempts','next_attempt_at','last_error']) THEN RETURN NEW; END IF;
  -- R10: future ordinary payload scheduling, never closing cutoff grace.
  IF NEW.status='pending' AND NEW.next_attempt_at>now_at
   AND EXISTS(SELECT 1 FROM tenants t WHERE t.brand_id=OLD.brand_id AND t.id=OLD.tenant_id AND t.status IN('active','suspended'))
   AND (newj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','next_attempt_at'])=
       (oldj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','next_attempt_at']) THEN RETURN NEW; END IF;
  -- R11: source-proven pre-POST reject, existing reason enum, exact original I.
  IF NEW.status='rejected' AND NEW.reject_reason IN('before_subscription_start','subscription_cancelled') AND NEW.rejected_at IS NOT NULL
   AND (newj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','status','reject_reason','rejected_at'])=
       (oldj-ARRAY['claim_token','claimed_until','claim_from_never','send_provenance','status','reject_reason','rejected_at']) THEN RETURN NEW; END IF;
 END IF;
 -- R12: trusted unsent rejected reassignment, sole deliberate target replacement exception.
 IF n AND OLD.status='rejected' AND NEW.status='pending' AND NEW.subscription_id IS NOT NULL
  AND NEW.target_endpoint_fingerprint IS NULL AND NEW.target_killbill_tenant_id IS NULL AND NEW.target_killbill_account_id IS NULL
  AND NEW.reject_reason IS NULL AND NEW.rejected_at IS NULL AND NEW.attempts=0 AND NEW.last_error IS NULL
  AND NEW.next_attempt_at IS NOT NULL
  AND (newj-ARRAY['subscription_id','target_endpoint_fingerprint','target_killbill_tenant_id','target_killbill_account_id','status',
                 'reject_reason','rejected_at','attempts','next_attempt_at','last_error'])=
      (oldj-ARRAY['subscription_id','target_endpoint_fingerprint','target_killbill_tenant_id','target_killbill_account_id','status',
                 'reject_reason','rejected_at','attempts','next_attempt_at','last_error']) THEN RETURN NEW; END IF;
 -- R13: D1 safe rejected discard; no event/receipt mutation and audit same caller transaction.
 IF n AND OLD.status='rejected' AND NEW.status='discarded' AND NEW.discarded_at IS NOT NULL
  AND (newj-ARRAY['status','discarded_at'])=(oldj-ARRAY['status','discarded_at']) THEN RETURN NEW; END IF;
 -- R14: split parent becomes X after paired event move in the SAME caller transaction.
 IF n AND OLD.status='pending' AND NEW.status='superseded' AND (newj-'status')=(oldj-'status')
  AND EXISTS(SELECT 1 FROM tenants t JOIN tenant_closure_settlements k ON k.brand_id=t.brand_id AND k.tenant_id=t.id
   WHERE t.brand_id=OLD.brand_id AND t.id=OLD.tenant_id AND t.status='closing'
    AND NOT EXISTS(SELECT 1 FROM usage_events e WHERE e.brand_id=OLD.brand_id AND e.tenant_id=OLD.tenant_id
     AND e.rollup_id=OLD.id AND e.reservation_id IS NULL AND e.occurred_at<=k.cutoff_at AND e.received_at<=k.cutoff_at)
    AND EXISTS(SELECT 1 FROM usage_events e WHERE e.brand_id=OLD.brand_id AND e.tenant_id=OLD.tenant_id
     AND e.rollup_id=OLD.id AND e.reservation_id IS NULL AND (e.occurred_at>k.cutoff_at OR e.received_at>k.cutoff_at))) THEN RETURN NEW; END IF;
 -- R15: original positive usage result S/U->F, never a duplicate bool or finance proof.
 IF s_shape AND NEW.status='forwarded' AND NEW.forwarded_at IS NOT NULL AND NEW.claim_token IS NULL
  AND NEW.claimed_until IS NULL AND NOT NEW.claim_from_never AND NEW.last_error IS NULL
  AND (newj-ARRAY['status','forwarded_at','claim_token','claimed_until','claim_from_never','last_error'])=
      (oldj-ARRAY['status','forwarded_at','claim_token','claimed_until','claim_from_never','last_error']) THEN RETURN NEW; END IF;
 -- R16: post-S unknown -> U, sticky fixed reason, exact original S/body/token/target retained.
 IF s_shape AND OLD.closure_unresolved_reason IS NULL AND NEW.status='pending' AND NOT NEW.claim_from_never
  AND NEW.closure_unresolved_reason IN('usage_send_unconfirmed','usage_duplicate_unconfirmed','usage_transport_unconfirmed')
  AND NEW.last_error=NEW.closure_unresolved_reason
  AND (newj-ARRAY['closure_unresolved_reason','last_error','claim_from_never'])=
      (oldj-ARRAY['closure_unresolved_reason','last_error','claim_from_never']) THEN RETURN NEW; END IF;
 -- R17: expired unconfirmed C -> U, never claim clear, renewal or fresh send.
 IF c_shape AND OLD.claimed_until<=now_at AND NOT NEW.claim_from_never
  AND NEW.closure_unresolved_reason='usage_claim_unconfirmed' AND NEW.last_error='usage_claim_unconfirmed'
  AND (newj-ARRAY['closure_unresolved_reason','last_error','claim_from_never'])=
      (oldj-ARRAY['closure_unresolved_reason','last_error','claim_from_never']) THEN RETURN NEW; END IF;
 -- R18: actual mixed S -> sticky closure hold; C/noS defers, N splits, F is R01 only.
 IF s_shape AND mixed AND OLD.closure_unresolved_reason IS NULL AND NEW.closure_unresolved_reason='mixed_cutoff_send_unconfirmed'
  AND (newj-'closure_unresolved_reason')=(oldj-'closure_unresolved_reason') THEN RETURN NEW; END IF;
 RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_rollup_update_forbidden';
END;
$$;
DROP TRIGGER IF EXISTS usage_rollups_possible_send_immutable ON usage_rollups;
CREATE TRIGGER usage_rollups_possible_send_immutable BEFORE INSERT OR UPDATE OR DELETE ON usage_rollups
 FOR EACH ROW EXECUTE FUNCTION usage_rollups_preserve_possible_send();

CREATE OR REPLACE FUNCTION usage_events_guard() RETURNS trigger
LANGUAGE plpgsql AS $$
DECLARE oldj jsonb; newj jsonb; source_parent usage_rollups%ROWTYPE; target_parent usage_rollups%ROWTYPE;
BEGIN
 IF TG_OP='DELETE' THEN
  RAISE EXCEPTION USING ERRCODE='insufficient_privilege',MESSAGE='usage_event_delete_forbidden';
 END IF;
 IF TG_OP<>'UPDATE' THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_event_operation_forbidden';
 END IF;
 oldj := to_jsonb(OLD); newj := to_jsonb(NEW);
 -- E00 exact no-op, including an identical legacy/reservation replay.
 IF newj=oldj THEN RETURN NEW; END IF;
 -- Preserve original15 evidence tuple AND added protocol. No allowed edge may change any other unlisted column.
 IF (NEW.id,NEW.brand_id,NEW.tenant_id,NEW.meter_id,NEW.event_id,NEW.killbill_unit,NEW.quantity_micro,
     NEW.dimensions,NEW.dimensions_key,NEW.resource_ref,NEW.subscription_id,NEW.reservation_id,NEW.occurred_at,
     NEW.received_at,NEW.acceptance_protocol)
 IS DISTINCT FROM
    (OLD.id,OLD.brand_id,OLD.tenant_id,OLD.meter_id,OLD.event_id,OLD.killbill_unit,OLD.quantity_micro,
     OLD.dimensions,OLD.dimensions_key,OLD.resource_ref,OLD.subscription_id,OLD.reservation_id,OLD.occurred_at,
     OLD.received_at,OLD.acceptance_protocol) THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_event_update_forbidden';
 END IF;
 -- Current wallet source inserts its reservation event already forwarded; no non-noop UPDATE is needed.
 -- Legacy raw0 also stays read-only. Neither case is promoted/linked to trusted raw protocol.
 IF OLD.reservation_id IS NOT NULL OR OLD.acceptance_protocol<>1 THEN
  RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_event_update_forbidden';
 END IF;
 IF NEW.rollup_id IS NOT NULL THEN
  SELECT * INTO target_parent FROM usage_rollups
   WHERE brand_id=NEW.brand_id AND tenant_id=NEW.tenant_id AND id=NEW.rollup_id;
 END IF;
 -- E01 trusted unrolled E -> N link, only rollup_id changes.
 IF OLD.rollup_id IS NULL AND NEW.rollup_id IS NOT NULL AND OLD.forwarded_at IS NULL AND OLD.killbill_tracking_id IS NULL
  AND target_parent.id IS NOT NULL AND target_parent.send_protocol=1 AND target_parent.status='pending'
  AND target_parent.send_provenance='never_attempted' AND target_parent.claim_token IS NULL AND target_parent.claimed_until IS NULL
  AND NOT target_parent.claim_from_never AND target_parent.send_started_at IS NULL AND target_parent.send_payload IS NULL
  AND target_parent.closure_unresolved_reason IS NULL AND target_parent.event_set_fingerprint IS NOT NULL
  AND target_parent.killbill_unit=OLD.killbill_unit
  AND target_parent.requested_subscription_id IS NOT DISTINCT FROM OLD.subscription_id
  AND target_parent.day=(OLD.occurred_at AT TIME ZONE 'UTC')::date
  AND (newj-'rollup_id')=(oldj-'rollup_id') THEN RETURN NEW; END IF;
 -- E02 unsent parent -> unsent child, only positive pre-cutoff pairs in one scoped split transaction.
 IF OLD.rollup_id IS NOT NULL AND NEW.rollup_id IS NOT NULL AND NEW.rollup_id IS DISTINCT FROM OLD.rollup_id THEN
  SELECT * INTO source_parent FROM usage_rollups
   WHERE brand_id=OLD.brand_id AND tenant_id=OLD.tenant_id AND id=OLD.rollup_id;
  IF OLD.forwarded_at IS NULL AND OLD.killbill_tracking_id IS NULL
   AND source_parent.id IS NOT NULL AND source_parent.send_protocol=1 AND source_parent.status='pending'
   AND source_parent.send_provenance='never_attempted' AND source_parent.claim_token IS NULL AND source_parent.claimed_until IS NULL
   AND NOT source_parent.claim_from_never AND source_parent.send_started_at IS NULL AND source_parent.send_payload IS NULL
   AND source_parent.closure_unresolved_reason IS NULL AND source_parent.event_set_fingerprint IS NOT NULL
   AND target_parent.id IS NOT NULL AND target_parent.send_protocol=1 AND target_parent.status='pending'
   AND target_parent.send_provenance='never_attempted' AND target_parent.claim_token IS NULL AND target_parent.claimed_until IS NULL
   AND NOT target_parent.claim_from_never AND target_parent.send_started_at IS NULL AND target_parent.send_payload IS NULL
   AND target_parent.closure_unresolved_reason IS NULL AND target_parent.event_set_fingerprint IS NOT NULL
   AND target_parent.killbill_unit=OLD.killbill_unit AND source_parent.killbill_unit=OLD.killbill_unit
   AND target_parent.requested_subscription_id IS NOT DISTINCT FROM OLD.subscription_id
   AND source_parent.requested_subscription_id IS NOT DISTINCT FROM OLD.subscription_id
   AND target_parent.day=(OLD.occurred_at AT TIME ZONE 'UTC')::date AND source_parent.day=target_parent.day
   AND (target_parent.subscription_id,target_parent.target_endpoint_fingerprint,target_parent.target_killbill_tenant_id,target_parent.target_killbill_account_id)
    IS NOT DISTINCT FROM
       (source_parent.subscription_id,source_parent.target_endpoint_fingerprint,source_parent.target_killbill_tenant_id,source_parent.target_killbill_account_id)
   AND EXISTS(SELECT 1 FROM tenants t JOIN tenant_closure_settlements k ON k.brand_id=t.brand_id AND k.tenant_id=t.id
    WHERE t.brand_id=OLD.brand_id AND t.id=OLD.tenant_id AND t.status='closing'
     AND OLD.occurred_at<=k.cutoff_at AND OLD.received_at<=k.cutoff_at)
   AND (newj-'rollup_id')=(oldj-'rollup_id') THEN RETURN NEW; END IF;
 END IF;
 -- E03 exact linked original positive F metadata, only once; parent F is changed in same caller transaction first.
 IF OLD.rollup_id IS NOT NULL AND NEW.rollup_id IS NOT DISTINCT FROM OLD.rollup_id
  AND OLD.forwarded_at IS NULL AND OLD.killbill_tracking_id IS NULL AND NEW.forwarded_at IS NOT NULL
  AND target_parent.id IS NOT NULL AND target_parent.send_protocol=1 AND target_parent.status='forwarded'
  AND target_parent.send_started_at IS NOT NULL AND target_parent.send_payload IS NOT NULL
  AND NEW.killbill_tracking_id IS NOT NULL AND NEW.killbill_tracking_id=target_parent.tracking_id
  AND (newj-ARRAY['forwarded_at','killbill_tracking_id'])=(oldj-ARRAY['forwarded_at','killbill_tracking_id']) THEN RETURN NEW; END IF;
 RAISE EXCEPTION USING ERRCODE='check_violation',MESSAGE='usage_event_update_forbidden';
END;
$$;
DROP TRIGGER IF EXISTS usage_events_append_only ON usage_events;
CREATE TRIGGER usage_events_append_only BEFORE UPDATE OR DELETE ON usage_events
 FOR EACH ROW EXECUTE FUNCTION usage_events_guard();
