-- Metering extension on Kill Bill. Core records usage events with the
-- caller's event IDs, summarises them and forwards them to Kill Bill as
-- usage records. Kill Bill rates and invoices them: no price is stored here.
-- usage_events is partitioned by month. The migration creates the months
-- through 2028-12 and the partition sweeper keeps three months ahead. See
-- docs/modules/metering.md.

-- The meters of a brand. killbill_unit is the catalogue usage unit the
-- meter's usage is forwarded to. dimensions are the keys a usage event may
-- carry. A meter is retired, never deleted, because events reference it.
CREATE TABLE meters (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL REFERENCES brands (id),
    slug text NOT NULL,
    unit text NOT NULL,
    killbill_unit text NOT NULL,
    dimensions text[] NOT NULL DEFAULT '{}',
    status text NOT NULL DEFAULT 'active',
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT meters_slug CHECK (slug ~ '^[a-z0-9][a-z0-9-]{0,62}$'),
    CONSTRAINT meters_unit CHECK (unit IN ('minute', 'second', 'byte', 'gib_hour', 'request', 'token', 'count')),
    CONSTRAINT meters_killbill_unit CHECK (killbill_unit ~ '^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$'),
    CONSTRAINT meters_dimensions CHECK (cardinality(dimensions) <= 16),
    CONSTRAINT meters_status CHECK (status IN ('active', 'retired'))
);

CREATE UNIQUE INDEX meters_brand_slug_key ON meters (brand_id, slug);
CREATE UNIQUE INDEX meters_brand_id_key ON meters (brand_id, id);

-- A set of usage events the forwarder sends to Kill Bill as one usage
-- record: one tenant, Kill Bill unit, requested subscription and day.
-- tracking_id is a digest of the rollup, so a retry sends the same tracking
-- ID and Kill Bill records it once. subscription_id is the Kill Bill
-- subscription the rollup goes to, chosen once before the first send.
-- period_start is the earliest occurred_at of its events. record_at is the
-- latest one and becomes the usage record's date: Kill Bill 0.24.22 leaves a
-- record dated before the subscription started off the invoice, so a record
-- dated midnight would miss the usage of the day the subscription starts.
-- claim_token and claimed_until mark the one forwarder that sends it. A
-- rollup whose period starts before its subscription started, or whose
-- subscription is cancelled, is rejected with reject_reason and waits for
-- staff to reassign it to another subscription or discard it.
CREATE TABLE usage_rollups (
    id uuid PRIMARY KEY,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    killbill_unit text NOT NULL,
    requested_subscription_id uuid,
    subscription_id uuid,
    day date NOT NULL,
    period_start timestamptz NOT NULL,
    record_at timestamptz NOT NULL,
    quantity_micro bigint NOT NULL,
    events integer NOT NULL,
    tracking_id text NOT NULL,
    status text NOT NULL DEFAULT 'pending',
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    claim_token bytea,
    claimed_until timestamptz,
    last_error text,
    reject_reason text,
    rejected_at timestamptz,
    discarded_at timestamptz,
    created_at timestamptz NOT NULL DEFAULT now(),
    forwarded_at timestamptz,
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    CONSTRAINT usage_rollups_quantity CHECK (quantity_micro > 0 AND events > 0),
    CONSTRAINT usage_rollups_period CHECK (period_start <= record_at),
    CONSTRAINT usage_rollups_status CHECK (status IN ('pending', 'forwarded', 'rejected', 'discarded')),
    CONSTRAINT usage_rollups_forwarded CHECK (status <> 'forwarded' OR (subscription_id IS NOT NULL AND forwarded_at IS NOT NULL)),
    CONSTRAINT usage_rollups_rejected CHECK (status NOT IN ('rejected', 'discarded') OR (reject_reason IS NOT NULL AND rejected_at IS NOT NULL)),
    CONSTRAINT usage_rollups_discarded CHECK (status <> 'discarded' OR discarded_at IS NOT NULL),
    CONSTRAINT usage_rollups_error_length CHECK (last_error IS NULL OR char_length(last_error) <= 200),
    CONSTRAINT usage_rollups_reject_reason CHECK (reject_reason IS NULL OR reject_reason IN ('before_subscription_start', 'subscription_cancelled'))
);

CREATE UNIQUE INDEX usage_rollups_tracking_key ON usage_rollups (brand_id, tenant_id, tracking_id);
CREATE UNIQUE INDEX usage_rollups_scope_id_key ON usage_rollups (brand_id, tenant_id, id);
CREATE INDEX usage_rollups_due_idx ON usage_rollups (next_attempt_at) WHERE status = 'pending';
CREATE INDEX usage_rollups_rejected_idx ON usage_rollups (brand_id, tenant_id) WHERE status = 'rejected';

-- Usage events, append-only and partitioned by month of occurred_at.
-- killbill_unit is the meter's unit when the event was recorded. A Core
-- reservation that settled into usage names itself in reservation_id: the
-- event is written when the settlement completes, already forwarded, so the
-- forwarder skips it. rollup_id, forwarded_at and killbill_tracking_id are
-- the only columns that change, when the forwarder sends the event to Kill
-- Bill.
CREATE TABLE usage_events (
    id uuid NOT NULL,
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    meter_id uuid NOT NULL,
    event_id text NOT NULL,
    killbill_unit text NOT NULL,
    quantity_micro bigint NOT NULL,
    dimensions jsonb NOT NULL DEFAULT '{}',
    dimensions_key text NOT NULL,
    resource_ref text,
    subscription_id uuid,
    reservation_id uuid,
    occurred_at timestamptz NOT NULL,
    received_at timestamptz NOT NULL,
    rollup_id uuid,
    forwarded_at timestamptz,
    killbill_tracking_id text,
    PRIMARY KEY (occurred_at, id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, meter_id) REFERENCES meters (brand_id, id),
    FOREIGN KEY (brand_id, tenant_id, reservation_id) REFERENCES reservations (brand_id, tenant_id, id),
    FOREIGN KEY (brand_id, tenant_id, rollup_id) REFERENCES usage_rollups (brand_id, tenant_id, id),
    CONSTRAINT usage_events_quantity CHECK (quantity_micro > 0),
    CONSTRAINT usage_events_resource_ref_length CHECK (resource_ref IS NULL OR char_length(resource_ref) BETWEEN 1 AND 255),
    CONSTRAINT usage_events_dimensions_size CHECK (octet_length(dimensions::text) <= 4096)
) PARTITION BY RANGE (occurred_at);

CREATE INDEX usage_events_scope_time_idx ON usage_events (brand_id, tenant_id, occurred_at DESC, id DESC);
CREATE INDEX usage_events_unrolled_idx ON usage_events (brand_id, tenant_id, received_at)
    WHERE rollup_id IS NULL AND reservation_id IS NULL;
CREATE INDEX usage_events_rollup_idx ON usage_events (rollup_id) WHERE rollup_id IS NOT NULL;
CREATE INDEX usage_events_backlog_idx ON usage_events (received_at) WHERE forwarded_at IS NULL;

CREATE FUNCTION usage_events_reject_truncate() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    RAISE EXCEPTION 'usage_events is append-only' USING ERRCODE = 'insufficient_privilege';
END;
$$;

-- A catch-all for an event whose month has no partition yet. The partition
-- sweeper creates the partitions of the current month and the next three on
-- every pass, so it stays empty. Should a row land here, an operator moves
-- it by hand before creating that month's partition, as metering.md says.
CREATE TABLE usage_events_default PARTITION OF usage_events DEFAULT;

-- A TRUNCATE trigger of the partitioned table does not fire when a
-- partition is truncated directly, so every partition gets its own.
CREATE TRIGGER usage_events_no_truncate
    BEFORE TRUNCATE ON usage_events_default
    FOR EACH STATEMENT EXECUTE FUNCTION usage_events_reject_truncate();

-- usage_events_ensure_partition creates the partition of the month of day,
-- with its TRUNCATE guard, if it does not exist.
CREATE FUNCTION usage_events_ensure_partition(day date) RETURNS void
    LANGUAGE plpgsql AS $$
DECLARE
    month_start date := date_trunc('month', day)::date;
    name text := 'usage_events_' || to_char(month_start, 'YYYY_MM');
    ns text;
BEGIN
    SELECT n.nspname INTO ns FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
        WHERE c.oid = 'usage_events'::regclass;
    IF to_regclass(quote_ident(ns) || '.' || quote_ident(name)) IS NOT NULL THEN
        RETURN;
    END IF;
    EXECUTE format('CREATE TABLE %I.%I PARTITION OF %I.usage_events FOR VALUES FROM (%L) TO (%L)',
        ns, name, ns, month_start, (month_start + interval '1 month')::date);
    EXECUTE format('CREATE TRIGGER usage_events_no_truncate BEFORE TRUNCATE ON %I.%I FOR EACH STATEMENT EXECUTE FUNCTION %I.usage_events_reject_truncate()',
        ns, name, ns);
END;
$$;

DO $$
DECLARE
    m date;
BEGIN
    FOR m IN SELECT generate_series(date '2026-01-01', date '2028-12-01', interval '1 month')::date LOOP
        PERFORM usage_events_ensure_partition(m);
    END LOOP;
END;
$$;

CREATE FUNCTION usage_events_guard() RETURNS trigger
    LANGUAGE plpgsql AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'usage_events is append-only' USING ERRCODE = 'insufficient_privilege';
    END IF;
    IF (NEW.id, NEW.brand_id, NEW.tenant_id, NEW.meter_id, NEW.event_id, NEW.killbill_unit, NEW.quantity_micro,
        NEW.dimensions, NEW.dimensions_key, NEW.resource_ref, NEW.subscription_id, NEW.reservation_id, NEW.occurred_at,
        NEW.received_at) IS DISTINCT FROM
       (OLD.id, OLD.brand_id, OLD.tenant_id, OLD.meter_id, OLD.event_id, OLD.killbill_unit, OLD.quantity_micro,
        OLD.dimensions, OLD.dimensions_key, OLD.resource_ref, OLD.subscription_id, OLD.reservation_id, OLD.occurred_at,
        OLD.received_at) THEN
        RAISE EXCEPTION 'usage_events is append-only' USING ERRCODE = 'insufficient_privilege';
    END IF;
    RETURN NEW;
END;
$$;

CREATE TRIGGER usage_events_append_only
    BEFORE UPDATE OR DELETE ON usage_events
    FOR EACH ROW EXECUTE FUNCTION usage_events_guard();

CREATE TRIGGER usage_events_no_truncate
    BEFORE TRUNCATE ON usage_events
    FOR EACH STATEMENT EXECUTE FUNCTION usage_events_reject_truncate();

-- The idempotency record of each usage event: one row per tenant, meter and
-- caller event ID, with a fingerprint of the payload. It is kept apart from
-- usage_events because a unique index on a partitioned table must include
-- the partition key. Its event is checked when the transaction commits, as
-- the key is written first to detect a replay.
CREATE TABLE usage_event_keys (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    meter_id uuid NOT NULL,
    event_id text NOT NULL,
    usage_id uuid NOT NULL,
    occurred_at timestamptz NOT NULL,
    fingerprint text NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, meter_id, event_id),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, meter_id) REFERENCES meters (brand_id, id),
    FOREIGN KEY (occurred_at, usage_id) REFERENCES usage_events (occurred_at, id) DEFERRABLE INITIALLY DEFERRED,
    CONSTRAINT usage_event_keys_event_id CHECK (event_id ~ '^[\x21-\x7E]{1,255}$')
);

-- Usage per tenant, meter, day and dimension set, in micro-units. It is
-- derived from usage_events: each recording adds to it in the same
-- transaction, and a rebuild recomputes it from the events.
CREATE TABLE usage_summaries (
    brand_id uuid NOT NULL,
    tenant_id uuid NOT NULL,
    meter_id uuid NOT NULL,
    day date NOT NULL,
    dimensions_key text NOT NULL,
    dimensions jsonb NOT NULL,
    quantity_micro bigint NOT NULL,
    events bigint NOT NULL,
    updated_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (brand_id, tenant_id, meter_id, day, dimensions_key),
    FOREIGN KEY (brand_id, tenant_id) REFERENCES tenants (brand_id, id),
    FOREIGN KEY (brand_id, meter_id) REFERENCES meters (brand_id, id),
    CONSTRAINT usage_summaries_quantity CHECK (quantity_micro >= 0 AND events >= 0)
);

