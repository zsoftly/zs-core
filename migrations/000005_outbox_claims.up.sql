-- A relay claims due events before it delivers them. claim_token identifies
-- the claim and claimed_until ends it, so a crashed relay's events become due
-- again once the claim expires. The claim query orders by next_attempt_at,
-- occurred_at, id, which outbox_dispatch_ready_idx already serves.
ALTER TABLE outbox ADD COLUMN claimed_until timestamptz;
ALTER TABLE outbox ADD COLUMN claim_token bytea;
