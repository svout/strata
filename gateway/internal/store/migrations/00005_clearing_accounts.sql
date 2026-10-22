-- Clearing accounts and the rail CHECK (SPEC-002 subtask 10a-ii-a, ADR-123).
--
-- One clearing account per rail, owned by the platform and named by
-- owner = 'rail:<name>'. Under ADR-123 a settled payment posts
-- payer -> clearing:<rail> (payments' payer_<id> posting), and the rail
-- moves clearing -> payee when it really moves the money (the internal
-- rail's pay_<id> posting). So clearing:internal nets to zero per payment,
-- and clearing:fake holds what the fake "paid out" to nobody.
--
-- The partial unique index makes the lookup by owner unambiguous: at most
-- one clearing account per rail. It is partial on kind = 'clearing' so it
-- says nothing about user or merchant owners, and NULL owners (clearing
-- accounts tests create by hand) never collide.
--
-- Both rails are seeded everywhere, production included: the schema is the
-- same in every environment, and whether the fake may run is decided in the
-- wiring (GATEWAY_ALLOW_FAKE_RAIL, refused under STRATA_ENV=production;
-- subtask 10a-ii-b). The same holds for the rail CHECK, which allows 'fake'.
--
-- The CHECK was deferred from 00001 until subtask 9 fixed the rail names
-- (rail.InternalName, rail.FakeName; ledger's TestEveryRailHasAClearingAccount
-- ties these literals to the constants). Adding a rail is a constraint swap
-- and a seeded clearing account, in one new migration.
--
-- The CHECK is added validated, not NOT VALID, on purpose: it scans payments
-- once, and any row on another rail fails the whole migration (seeds
-- included; goose runs the file in one transaction) rather than leaving rows
-- the constraint doesn't hold for. None should exist: payments.rail has only
-- ever been written from an adapter's Name(), and the table is small in P1.
--
-- This supersedes two comments in 00001, which can't be edited after merge:
-- "NULL [owner] for clearing accounts" (seeded clearing accounts now have
-- owner 'rail:<name>'; NULL is still allowed for hand-made ones) and "No
-- CHECK yet" on payments.rail.
--
-- Down deletes the two seeded accounts. It fails (foreign key) once any
-- ledger entry references them, which is intended: rolling back past this
-- migration must not discard postings.

-- +goose Up
CREATE UNIQUE INDEX accounts_clearing_owner_key ON accounts (owner) WHERE kind = 'clearing';

INSERT INTO accounts (owner, kind, currency) VALUES
    ('rail:internal', 'clearing', 'GBP'),
    ('rail:fake', 'clearing', 'GBP');

ALTER TABLE payments ADD CONSTRAINT payments_rail_check CHECK (rail IN ('internal', 'fake'));

-- +goose Down
ALTER TABLE payments DROP CONSTRAINT payments_rail_check;
DELETE FROM accounts WHERE kind = 'clearing' AND owner IN ('rail:internal', 'rail:fake');
DROP INDEX accounts_clearing_owner_key;
