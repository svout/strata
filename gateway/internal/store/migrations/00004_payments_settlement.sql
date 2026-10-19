-- Settlement support for payments (SPEC-002 subtask 10a-i): why a payment
-- failed, and the index the recovery pass reads its backlog from.
--
-- failure_reason: a decline is the payer's problem; an invalid request is
-- ours: the gateway built a request the rail refused as wrong in itself
-- (rail.ErrInvalidRequest). The two must not look alike in the data, so
-- failure_reason is set exactly when state = 'failed'.
--
-- No backfill: before this migration nothing outside the tests' throwaway
-- databases called payments.MarkFailed, so no failed rows exist.
--
-- payments_settling_idx serves payments.Recover's one query: settling
-- payments on one rail, older than a grace period, in (updated_at, id)
-- keyset order. Column order is rail (equality), then updated_at (the age
-- cutoff and the first sort key), then id (the tie-break). Partial on
-- state = 'settling', so it holds only the backlog, not every payment ever
-- made, and a row leaves it when it settles or fails.

-- +goose Up
ALTER TABLE payments
    ADD COLUMN failure_reason text NULL CHECK (failure_reason IN ('declined', 'invalid_request')),
    ADD CONSTRAINT payments_failure_reason_iff_failed CHECK ((state = 'failed') = (failure_reason IS NOT NULL));

CREATE INDEX payments_settling_idx ON payments (rail, updated_at, id) WHERE state = 'settling';

-- +goose Down
DROP INDEX payments_settling_idx;
ALTER TABLE payments
    DROP CONSTRAINT payments_failure_reason_iff_failed,
    DROP COLUMN failure_reason;
