-- Index idempotency keys by a hash of the key (SPEC-002 Q11-ii, ADR-126).
--
-- Why: idem's claim reads the key before inserting it (ADR-118), and under
-- SERIALIZABLE that read takes an SSI predicate lock on the btree leaf page
-- it searched. Clients choose keys, and often choose sequential ones, so
-- concurrent creates on distinct keys searched the same leaf of the
-- (scope, key) primary key, wrote into what the other had read, and one of
-- them rolled back with 40001: ~0.95 rollbacks per request (SPEC-002 Q11-i).
-- Keyed by SHA-256 instead, neighbouring keys land on unrelated pages.
--
-- The (scope, key) primary key is DROPPED, not kept beside the new one. That
-- looks like a safety regression and is not: that btree is the contended
-- index, and keeping it would keep the conflicts (every insert still writes
-- into it, and its page is what the other transaction's search locked).
-- "One record per (scope, key)" is still enforced by the database, by two
-- constraints together:
--   * the primary key on key_hash: one row per hash; and
--   * key_hash_matches: the stored hash must equal SHA-256 of the row's own
--     scope and key, so a row cannot carry a hash that is not its key's.
-- Together they give at most one row per (scope, key). Without the CHECK the
-- guarantee would rest on Go's idem.keyHash never drifting: a changed
-- encoding would insert a second row for a key already present, which is a
-- duplicate payment. With it, a drifted keyHash fails every insert closed
-- (23514).
--
-- The hash, byte for byte the same as idem.keyHash: for scope then key, the
-- byte length as a big-endian int64 (int8send), then the UTF-8 bytes. The
-- length prefix makes the encoding unambiguous: ('a', 'bc') and ('ab', 'c')
-- differ.
--
-- Why a CHECK and not GENERATED ALWAYS AS ... STORED: PostgreSQL requires a
-- generated column's expression to be IMMUTABLE, and convert_to (like any
-- text-to-bytes function) is only STABLE, as its result depends on the
-- server encoding; PostgreSQL 16 rejects the column. A CHECK accepts it.
-- The cost of that: PostgreSQL assumes CHECK conditions are immutable, and
-- this one holds only while server_encoding is UTF8 (fixed when the database
-- is created; ours are). Restoring a dump into a non-UTF8 database would
-- reject rows loudly, never accept a wrong one. Not done: wrapping
-- convert_to in a function falsely marked IMMUTABLE.
--
-- Large tables: this runs inside goose's transaction, with a plain backfill
-- UPDATE and a plain (blocking) index build, which is fine at P1 sizes. On a
-- large live table the fix is not just "add CONCURRENTLY": CREATE INDEX
-- CONCURRENTLY cannot run in a transaction. It would take a no-transaction
-- migration (goose's NO TRANSACTION annotation), a batched backfill as its own step,
-- a concurrent unique index build, then the constraint swap using that index.

-- +goose Up
ALTER TABLE idempotency_keys ADD COLUMN key_hash bytea;

-- The backfill below and the CHECK after it spell out the same expression,
-- and must stay byte-identical (as idem.keyHash must match both). A mismatch
-- fails this migration, since adding the CHECK validates the backfilled rows.
UPDATE idempotency_keys SET key_hash = sha256(
    int8send(octet_length(scope)::bigint) || convert_to(scope, 'UTF8') ||
    int8send(octet_length(key)::bigint)   || convert_to(key, 'UTF8'));

-- NOT NULL as well as the CHECK: a CHECK passes NULL (NULL is not false).
ALTER TABLE idempotency_keys ALTER COLUMN key_hash SET NOT NULL;

-- Validates every backfilled row as it is added, so a backfill that
-- disagreed with the check would fail the migration here.
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_key_hash_matches CHECK (key_hash = sha256(
    int8send(octet_length(scope)::bigint) || convert_to(scope, 'UTF8') ||
    int8send(octet_length(key)::bigint)   || convert_to(key, 'UTF8')));

-- Same constraint name as before, on the new column.
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (key_hash);

-- +goose Down
-- (scope, key) is unique in every valid v7 table (the two constraints above
-- imply it), so restoring the old primary key cannot fail on existing rows.
ALTER TABLE idempotency_keys DROP CONSTRAINT idempotency_keys_pkey;
ALTER TABLE idempotency_keys ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (scope, key);
ALTER TABLE idempotency_keys DROP COLUMN key_hash;
