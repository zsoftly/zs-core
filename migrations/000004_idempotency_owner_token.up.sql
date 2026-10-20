-- Every reservation carries a random owner token. Complete and Release match
-- it, so a request whose lease was taken over cannot write onto or delete the
-- reservation of the request that took it over.
ALTER TABLE idempotency_keys ADD COLUMN owner_token bytea;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_owner_token_length
    CHECK (owner_token IS NULL OR octet_length(owner_token) = 16);

-- completed_unstored: the command ran but its result could not be stored. The
-- key replays idempotency_key_in_progress until it expires, so the command does
-- not run a second time.
ALTER TABLE idempotency_keys ADD COLUMN failure_reason text;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_failure_reason_length
    CHECK (failure_reason IS NULL OR char_length(failure_reason) <= 1000);

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_status;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_status
    CHECK (status IN ('pending', 'completed', 'completed_unstored'));

ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_completed_has_result;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_completed_has_result CHECK (
    status <> 'completed' OR (result_status IS NOT NULL AND result_body IS NOT NULL AND completed_at IS NOT NULL)
);
