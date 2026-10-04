-- Schema v1 (SPEC-002 subtask 4): accounts, payments, the double-entry ledger
-- and idempotency records. Never edit this file after merge; add a new
-- migration instead.
--
-- Conventions used throughout:
--   * Money is bigint minor units (pence), never numeric/float, and always > 0.
--     Direction (debit/credit) carries the sign, not the amount.
--   * Currency is fixed to GBP in P1 (SPEC-002). The CHECK is there so a
--     later multi-currency phase must change it on purpose, in a migration.
--   * Enumerations are text + CHECK rather than Postgres ENUM types: adding a
--     value is then a one-line constraint swap, and ENUM values can't be removed.

-- +goose Up

CREATE TABLE accounts (
    id         uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    -- External reference to whoever owns the account (a user or merchant id
    -- from outside this database). NULL for clearing accounts, which belong to
    -- the platform itself rather than to any party.
    owner      text        NULL,
    kind       text        NOT NULL CHECK (kind IN ('user', 'merchant', 'clearing')),
    currency   char(3)     NOT NULL CHECK (currency = 'GBP'),
    created_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE payments (
    id               uuid        PRIMARY KEY DEFAULT gen_random_uuid(),
    payer_account_id uuid        NOT NULL REFERENCES accounts (id),
    payee_account_id uuid        NOT NULL REFERENCES accounts (id),
    amount           bigint      NOT NULL CHECK (amount > 0),
    currency         char(3)     NOT NULL CHECK (currency = 'GBP'),
    -- Lifecycle: created -> settling -> settled | failed (refunded is a stretch
    -- goal). This CHECK only limits the values; forward-only transitions are
    -- enforced in SQL by the payment state machine (SPEC-002 subtask 7).
    state            text        NOT NULL DEFAULT 'created'
                                 CHECK (state IN ('created', 'settling', 'settled', 'failed', 'refunded')),
    -- Which rail settles this payment. No CHECK yet: the RailAdapter subtask
    -- (SPEC-002 subtask 9) adds one once the rail names are fixed.
    rail             text        NOT NULL,
    -- The rail's own request/transaction id, written with state = 'settling'
    -- before the rail is called, so crash recovery can look it up.
    rail_ref         text        NULL,
    -- The mandate that authorised this payment. No foreign key yet: the
    -- mandates table arrives in P2, whose migration adds the constraint.
    mandate_id       uuid        NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    -- Set explicitly by the application on every state change (no trigger).
    updated_at       timestamptz NOT NULL DEFAULT now(),
    -- A payment from an account to itself moves no money and would post two
    -- legs that cancel on one account; reject it outright.
    CHECK (payer_account_id <> payee_account_id)
);

-- One row per leg of a double-entry posting. The invariant "debits = credits
-- per payment" is NOT enforced by this schema: ledger.Post (SPEC-002
-- subtask 5) writes all legs of a posting in one transaction and checks the
-- sum before committing. A database-level guard (e.g. a deferred constraint
-- trigger) is a possible later migration, not part of this one.
CREATE TABLE ledger_entries (
    -- An identity rather than a uuid: entries are append-only and never
    -- referenced from outside, and a sequence gives a natural insertion order.
    id         bigint      GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    payment_id uuid        NOT NULL REFERENCES payments (id),
    account_id uuid        NOT NULL REFERENCES accounts (id),
    direction  text        NOT NULL CHECK (direction IN ('debit', 'credit')),
    amount     bigint      NOT NULL CHECK (amount > 0),
    created_at timestamptz NOT NULL DEFAULT now()
);

-- Postgres does not index foreign keys automatically. These serve "entries for
-- a payment" (posting checks, GET) and "entries for an account" (balances).
CREATE INDEX ledger_entries_payment_id_idx ON ledger_entries (payment_id);
CREATE INDEX ledger_entries_account_id_idx ON ledger_entries (account_id);

-- The source of truth for idempotency (SPEC-002): the primary key makes a
-- second INSERT of the same (scope, key) fail, however many requests race.
-- Redis only holds a short-lived in-flight lock in front of this table.
-- Expiry/TTL of old keys belongs to the idempotency subtask (SPEC-002 subtask 8).
CREATE TABLE idempotency_keys (
    -- '' in P1. P2 sets it to the calling agent's id, so two agents may reuse
    -- the same Idempotency-Key without colliding.
    scope         text        NOT NULL DEFAULT '',
    key           text        NOT NULL CHECK (char_length(key) BETWEEN 1 AND 255),
    -- SHA-256 of the canonical request. Same key + different hash -> 409.
    request_hash  bytea       NOT NULL CHECK (octet_length(request_hash) = 32),
    status        text        NOT NULL CHECK (status IN ('in_progress', 'completed')),
    -- The stored response replayed to retries. response_code is set exactly
    -- when status = 'completed' (the paired CHECK below), so a retry can never
    -- find a "completed" key with nothing to replay, or an in-progress key
    -- that already looks finished. It must be a valid HTTP status; NULL passes
    -- the range CHECK, as CHECKs treat NULL as not false.
    response_code int         NULL CHECK (response_code BETWEEN 100 AND 599),
    response_body bytea       NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (scope, key),
    CHECK ((status = 'completed') = (response_code IS NOT NULL))
);

-- +goose Down

-- Reverse order of creation, so no table is dropped while another still
-- references it.
DROP TABLE idempotency_keys;
DROP TABLE ledger_entries;
DROP TABLE payments;
DROP TABLE accounts;
