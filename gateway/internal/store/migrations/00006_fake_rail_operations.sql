-- The fake PSP's own record of operations (SPEC-002 subtask 10a-ii-c).
--
-- rail.Fake used to keep its references in memory, so a restarted gateway
-- forgot them: Status(ref) said "unknown", recovery called Settle again, and
-- the fake moved the money a second time. UNIQUE (rail_ref, leg) on
-- ledger_entries would then hide the double payment from the ledger, and a
-- kill/restart test (subtask 11) would pass while the rail double-paid.
-- ADR-120's deterministic reference only means something if Status(ref)
-- survives a restart, so the fake's outcomes live here.
--
-- The table is the fake's, not the gateway's: it stands for the PSP's own
-- database, and only package rail reads or writes it (rail/store.go,
-- ADR-117). That is also why there is no foreign key to payments: a real PSP
-- knows nothing of our payments table, and the rail is handed a reference,
-- not a payment row.
--
-- It is created everywhere, production included, like the 'fake' value in
-- payments_rail_check and the seeded clearing:fake account (00005): the
-- schema is the same in every environment, and whether the fake may run is
-- decided in the wiring (GATEWAY_ALLOW_FAKE_RAIL, refused under
-- STRATA_ENV=production).
--
-- ref: the rail reference (rail.Ref, 'pay_<uuid>'), the primary key, so a
-- second insert for one reference is a conflict and the first writer wins.
-- 1-255 characters, as rail's validate and ledger_entries.rail_ref (00003).
--
-- outcome / reason: rail.Settled, rail.Pending or rail.Declined. A decline
-- always carries a (non-empty) reason and nothing else does, as
-- rail.Result requires.
--
-- moves: how many times money moved for the reference. It is a counter, not
-- a flag derived from outcome, so a bug that applied an operation twice
-- would show as 2 (tests read it through Fake.Moves).

-- +goose Up
CREATE TABLE fake_rail_operations (
    ref        text        PRIMARY KEY CHECK (char_length(ref) BETWEEN 1 AND 255),
    outcome    text        NOT NULL CHECK (outcome IN ('settled', 'pending', 'declined')),
    reason     text        NULL CHECK (reason <> ''),
    moves      int         NOT NULL CHECK (moves >= 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    updated_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT fake_rail_operations_reason_iff_declined CHECK ((outcome = 'declined') = (reason IS NOT NULL))
);

-- +goose Down
DROP TABLE fake_rail_operations;
