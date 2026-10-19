-- A posting that settles a rail operation happens at most once, enforced by
-- the database (SPEC-002 subtask 9, ADR-120). A posting is several rows (one
-- per leg), and a refund posts reversed legs for the same payment, so neither
-- payment_id nor a single column can be unique. Instead, a posting made with
-- ledger.PostRef carries the rail reference and numbers its legs from 1:
-- (rail_ref, leg) is unique, so a second posting with the same reference
-- collides on leg 1 however the two writers race. ledger.Posted is only the
-- fast path; this constraint is the guarantee.
--
-- Postings without a reference (ledger.Post: refunds, tests) leave both
-- columns NULL, which the unique constraint ignores. The CHECK keeps the two
-- columns set together.

-- +goose Up
ALTER TABLE ledger_entries
    ADD COLUMN rail_ref text     NULL CHECK (char_length(rail_ref) BETWEEN 1 AND 255),
    ADD COLUMN leg      smallint NULL CHECK (leg >= 1),
    ADD CONSTRAINT ledger_entries_rail_ref_leg_paired CHECK ((rail_ref IS NULL) = (leg IS NULL)),
    ADD CONSTRAINT ledger_entries_rail_ref_leg_key UNIQUE (rail_ref, leg);

-- +goose Down
ALTER TABLE ledger_entries
    DROP CONSTRAINT ledger_entries_rail_ref_leg_key,
    DROP CONSTRAINT ledger_entries_rail_ref_leg_paired,
    DROP COLUMN leg,
    DROP COLUMN rail_ref;
