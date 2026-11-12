-- Dev-only seed (SPEC-002 11a): the two accounts `make chaos` pays between.
-- No API creates accounts yet, so a test driver would otherwise have to
-- write the ledger's table itself (ADR-117). Fixed ids, so the script knows
-- them; ON CONFLICT makes it safe to apply on every run. Applied by
-- `make dev-seed`, never by a migration: this data must not reach any real
-- database.
INSERT INTO accounts (id, owner, kind, currency) VALUES
    ('00000000-0000-4000-8000-00000000c4a1', 'dev:chaos-payer', 'user', 'GBP'),
    ('00000000-0000-4000-8000-00000000c4a2', 'dev:chaos-payee', 'merchant', 'GBP')
ON CONFLICT (id) DO NOTHING;
