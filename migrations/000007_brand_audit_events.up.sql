-- Brand-level audit events, such as brand apply and staff grants, belong to a
-- brand and no tenant. They are stored with a NULL tenant_id, which no tenant
-- query can match because those filter on tenant_id = $2.
ALTER TABLE audit_events ALTER COLUMN tenant_id DROP NOT NULL;

-- Structured detail of an event, such as the diff of a brand apply. Never
-- holds secrets.
ALTER TABLE audit_events ADD COLUMN details jsonb;
ALTER TABLE audit_events ADD CONSTRAINT audit_events_details_object
    CHECK (details IS NULL OR jsonb_typeof(details) = 'object');

CREATE INDEX audit_events_brand_time_idx
    ON audit_events (brand_id, occurred_at DESC, id DESC)
    WHERE tenant_id IS NULL;
