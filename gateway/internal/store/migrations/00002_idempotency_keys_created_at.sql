-- Index on idempotency_keys.created_at (SPEC-002 subtask 8b, decision 7).
-- Expired keys (idem.Retention, 24 h) are overwritten on the next claim, so
-- correctness never needs it; it is for the periodic sweeper deferred past P1,
-- which deletes by age. Plain CREATE INDEX (not CONCURRENTLY): the table is
-- small in P1 and this keeps the migration inside goose's transaction.

-- +goose Up
CREATE INDEX idempotency_keys_created_at_idx ON idempotency_keys (created_at);

-- +goose Down
DROP INDEX idempotency_keys_created_at_idx;
