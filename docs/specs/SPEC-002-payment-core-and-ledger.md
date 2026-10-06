---
type: spec
id: SPEC-002
phase: "P1 Payment Core and Ledger"
track: A
status: approved
owner: ivan
adr:
  - ADR-103
  - ADR-105
  - ADR-113
  - ADR-115
created: 2026-10-03
---
<!-- DO NOT EDIT: exported from vault "Specs/SPEC-002 Payment Core and Ledger.md" by make export-docs | source-sha256: 2f1256f1df131f8271c4a233e9d2bb27d30c042694dd54e388647cb072133710 | content-sha256: 39d32e122fadb5b9ab67e77eaebbfad697fcbc6bfacf98041e99c9fa709b51f2 -->

# SPEC-002 Payment Core and Ledger

## Problem
No code exists yet: the repository has a skeleton and CI only. Every later phase needs money to move correctly first. Mandate checks (P2), policy (P3), scoring (P4) and the audit chain (P5) all sit in front of a payment that must post exactly once and balance. P1 is also the first Go service, so it has to set the project's patterns: config, logging, migrations, test fixtures, `make` entry points and the CI path.

## Goal and non-goals
**Goal:** a `gateway` Go service that creates payments through an idempotent API and posts them to a double-entry ledger in PostgreSQL. It settles them through a `RailAdapter`, starting with the internal ledger rail and a deterministic fake payment provider. Three properties are proven: money conservation, exactly-once under concurrent retries, and no ambiguous state after a crash.
**Non-goals:** signatures, mandates or agent identity (P2); policy (P3); risk scoring (P4); the hash-chained audit (P5); the console; performance tuning against the 25 ms budget (P6 measures it); real money or real card data (ADR-105). The `mandate_id` link to P2 stays nullable here.

## User stories
1. As an agent platform, I can create a payment with an `Idempotency-Key`. Retrying the same request returns the original result instead of paying twice.
2. As the operator, I can trust that every settled payment's ledger entries sum to zero and that no crash leaves a payment half-posted.
3. As the developer (and later the examiner), I can run `make dev` and `make test` on one machine and see the invariants proven.

## Acceptance criteria
Each criterion maps to at least one automated test that runs in CI, unless marked *local*.
- [x] **AC1 Money conservation:** across 10,000 randomised create/settle/fail/refund operations (property test, `rapid`), every payment's ledger entries sum to zero, and all account balances sum to zero. *(Met at ledger level, subtask 6, 2026-10-05: `TestPropertyMoneyConservation`. Re-run through `internal/payments` after subtasks 7 and 10, when fail/settle drive real state; Ivan, 2026-10-05.)*
- [x] **AC2 No partial postings:** a failure injected at any point inside a posting leaves **zero** entries for that posting (property or fault-injection test). *(Met, subtask 6, 2026-10-05: `TestPropertyNoPartialPostings`.)*
- [ ] **AC3 Exactly once:** 100 concurrent requests with the same `Idempotency-Key` and body create exactly one payment and one set of ledger entries, and all 100 receive the same response. The same key with a different body returns `409 Conflict`.
- [ ] **AC4 Crash safety:** killing the gateway mid-settlement, then restarting it, leaves no payment in an ambiguous state. Every payment is either terminal (`settled`/`failed`) or `settling` with a recovery record that resolves it against the rail's state (chaos script; CI runs it against the fake PSP).
- [ ] **AC5 Webhooks:** a duplicated webhook and out-of-order callbacks from the payment provider never double-post or move a payment backwards (chaos script against the fake PSP).
- [ ] **AC6 Rail contract:** the internal-ledger rail and the fake PSP pass one shared `RailAdapter` contract test suite. The Stripe test-mode adapter passes it too when test keys are present, and skips cleanly otherwise.
- [ ] **AC7 Runnable:**
  - `make dev` starts the stack (gateway, PostgreSQL 16, Redis 7) from `infra/compose/`.
  - `make lint` and `make test` pass locally and in CI, and **CI calls the make targets** (the Reuse Registry's Make-targets note).
  - `/healthz` and `/readyz` (DB + Redis) respond.
- [ ] **AC8 Logs:** structured JSON logs carry IDs and amounts, never secrets, card data or webhook bodies (repo CLAUDE.md). A test asserts that a known secret value never appears in the log output.
- [ ] **AC9 Spec export:** `make export-docs` copies this spec into `docs/specs/` with a hash stamp (ADR-107), so PR review can see it.

## Design notes
**API:**
- `POST /v1/payments`: amount in minor units (`int64`), currency, payer account, payee account, optional `mandate_id`. Requires the `Idempotency-Key` header.
- `GET /v1/payments/{id}`.
- `POST /v1/rail/webhooks/{rail}`: provider callbacks.
- Errors are JSON with a stable `code`.

**States:** `created → settling → settled | failed`; `refunded` is a stretch goal. Transitions only ever move forward, enforced in SQL.

**Data:**
- **Tables:** `accounts` (id, owner, kind: user, merchant or clearing; currency), `payments` (as in Architecture › Data model (first cut), plus `state` and `rail_ref`), `ledger_entries` (payment_id, account_id, direction, amount; **sum per payment = 0**) and `idempotency_keys` (key, request hash, response, status, created_at). Exact columns and constraints as built: subtask 4a.
- **Isolation:** postings run at `SERIALIZABLE` and retry on serialization failure.
- **Migrations:** goose v3.28.0 Provider in `internal/store`, run with `make migrate` (`gateway/cmd/migrate up`) under goose's advisory lock; never hand-edited after merge.

**Idempotency:** PostgreSQL's unique key on `idempotency_keys` is the source of truth. Redis holds a short-lived in-flight lock, so concurrent duplicates wait rather than race. If Redis is down, correctness still comes from the PostgreSQL constraint.

**Crash safety:** before calling the rail, a `settling` row is written along with the rail request ID (an outbox). On start-up, a recovery pass queries the rail for every `settling` payment and finalises it.

**Performance budget:** none is enforced for P1, because the payment core sits outside the 25 ms decision pipeline. The test suite should run in under 60 s locally, excluding container start-up. Record a baseline p50/p99 for `POST /v1/payments` for P6.

## Reuse plan
Check Reuse Registry first. No component exists yet; P1 creates the first ones.
| Need | Reuse / extend / new | Registry item or new home |
|---|---|---|
| Double-entry posting, balances | new (row exists, `planned`) | `Ledger`: `gateway/internal/ledger` |
| Idempotent state-changing endpoints | new (row exists) | `Idempotency`: `gateway/internal/idem` |
| Payment-provider or internal rail | new (row exists) | `RailAdapter`: `gateway/internal/rail` |
| Postgres/Redis containers, factories | new (row exists) | Test fixtures: `gateway/internal/testutil` |
| Make entry points | new (row exists) | Make targets: `Makefile` |
| Local stack | new (row exists) | Compose stack: `infra/compose/` |
| CI | extend | CI workflow: switch the run steps to `make lint` / `make test` |
| Payment API handlers, config, logging, migrations | new | add Registry rows at wrap-up |
| Property tests, chaos script | new | add rows (flagged missing on 2026-09-27) |
| Spec export | new | `make export-docs` (`scripts/export-docs`), part of the Make-targets row |

## Subtasks
Each takes at most a day, fits one session and leaves `make test` green. Under ADR-113 and ADR-115, Claude implements and Ivan reviews and owns the result. Every step is declared in the AI Use Log.
- [x] 1. **Go skeleton + make.** *(PR #3, merged as e14704c, 2026-10-03; AC8 test included)*
  - `gateway/go.mod`, `cmd/gateway/main.go`, `internal/config` (env), `internal/httpx` (chi router, `slog` JSON logging, `/healthz`), `.golangci.yml`.
  - Root `Makefile` (`lint`, `test`, `dev` stub); CI switched to `make`.
  - Tests: health handler; config parsing.
- [x] 1b. **Logging + toolchain hardening** *(PR #4 3d0f3b4 + PR #5 a7334d2 slog-contract fix; main b65c04e, 2026-10-03)* (added 2026-10-03 after the subtask-1 review).
  - Redaction: also cover `slog.Group` keys and values inside `slog.Any` (maps and structs). Extend the key list (`signing_key`, `hmac_key`, `credential`, `cookie`, `body`, `payload`, `mandate`), and stop over-redacting verdict keys such as `signature_valid` and `token_count`.
  - Request ID: replace chi's `middleware.RequestID` with a small middleware. It generates a random ID with no hostname, and accepts an inbound `X-Request-Id` only if it's short and limited to safe characters.
  - Toolchain, **only if ADR-116 is accepted**: current Go in `go.mod`, the Makefile `GO_TOOLCHAIN` and CI; the latest golangci-lint v2, installed the same way in both; a version check in `make lint`; `make dev` uses the pinned toolchain.
  - Tests: group and map redaction, the key list (including words that must *not* be redacted), and request-ID generation and validation.
  - **Decided at planning (Ivan, 2026-10-03):** non-scalar `slog.Any` values (maps, structs, slices, `[]byte`) are **never emitted**: they log as `[OMITTED <type>]`. Errors log their message. A type that must be logged implements `slog.LogValuer` and is then redacted key by key. Adds a `make install-lint` target.
  - Touches `gateway/internal/httpx/{log.go,router.go,httpx_test.go}`, and `Makefile`, `.github/workflows/ci.yml`, `gateway/go.mod` if ADR-116 is accepted.
- [x] 2. **Spec export.** *(PR #6, 26fda74, main 4c82d1f, 2026-10-03; AC9 met)*
  - `scripts/export-docs` + `make export-docs`: copy `Specs/SPEC-*.md` from `$STRATA_VAULT` into `docs/specs/`, stamp a SHA-256, and fail if an export was hand-edited.
  - Export this spec.
- [x] 2b. **Export hardening** *(PR #7, eba23d4, main f87956d, CI green, 2026-10-03)* (added 2026-10-03 after the subtask-2 review; no change to current exports).
  - **Collisions:** fail if two vault notes slug to the same file, or if a target's stamp `src` differs from the note being exported (no silent overwrite).
  - **Input forms:** normalise CRLF to LF before hashing and writing; handle a UTF-8 BOM and frontmatter that ends at EOF without a newline; clear error (exit 1) on non-UTF-8 input.
  - **Fences:** match the closing fence by character and length (≥ the opening run), so a ```` block isn't closed by ```.
  - **Comments:** strip `%%…%%` across the whole body except inside fenced code (a comment spanning a fence no longer leaks); an unclosed `%%` hides everything to EOF, as Obsidian does.
  - **`check`:** also fail when a file's name isn't `slug(stamp src)` (a renamed or copied export).
  - **Tests:** one per item above (collision, src mismatch, CRLF, BOM, EOF frontmatter, 4-backtick fence, comment across a fence, unclosed comment, renamed export).
  - Touches `scripts/export_docs.py`, `scripts/test_export_docs.py`.
- [x] 2c. **Export: no silent loss** *(PR #8, 798ba37, main edb1b48, CI green, 2026-10-03)* (added 2026-10-03 after the subtask-2b review; no change to current exports).
  - **Warning:** when a `%%` comment is still open at end of file (e.g. an inline code span wrapped across lines with `%%` inside), print a stderr warning naming the note and the line where the comment opened. The exit code is unchanged, because Obsidian also hides to EOF.
  - **Line splitting:** split the body on `\n` only, not `str.splitlines()`, so U+2028, `\x85`, `\f` and `\v` can't open a fence mid-line and let a comment through.
  - **Small fixes:** write the BOM as `\ufeff` (not an invisible literal); the src-mismatch refusal says "delete it if the old source was renamed" instead of "move your change into the vault note".
  - **Tests:** the warning (code span split across lines), a U+2028 line, lone CR, tilde fence and a backtick info string, the existing export of a collision-blocked note not reported as an orphan, and a hand-edited orphan.
  - Touches `scripts/export_docs.py`, `scripts/test_export_docs.py`.
- [x] 3a. **Readiness (Go).** *(PR #9, c976cc4, main 89251e6, CI green, 2026-10-03)* Split from 3 at planning (2026-10-03, Ivan); no Docker needed.
  - `internal/httpx`: `ReadyCheck{Name, Check}`; `NewRouter(log, checks ...ReadyCheck)` (additive Registry change, approved). `/readyz` runs checks in parallel with a 1 s timeout each: 200 `ready` or 503 `not_ready` with `ok`/`fail`/`timeout` per check. Error text never goes in the response or the log.
  - `internal/config`: `DATABASE_URL` required (`ErrMissingDatabaseURL`); `REDIS_ADDR` defaults to `localhost:6379`; config never logged.
  - `cmd/gateway`: lazy `pgxpool` and go-redis clients (start even if DB or Redis is down; `/readyz` reports it); a malformed `DATABASE_URL` fails startup with a fixed message; close both on shutdown.
  - Logger: `dsn` and `database_url` become hard redaction terms (from Q9).
  - Tests with fake checks (ok, fail, timeout, no secret leak, 404 without checks) and config tests; no Docker. Re-export SPEC-002.
  - Touches `gateway/internal/httpx/{ready.go,ready_test.go,router.go,log.go,httpx_test.go}`, `gateway/internal/config/*`, `gateway/cmd/gateway/main.go`, `gateway/go.mod`, `gateway/go.sum`, `docs/specs/` (export).
- [x] 3b. **Compose stack.** *(PR #10, 71e6b9c, main d211656, CI green, 2026-10-03; AC7 met)* Needs Docker Desktop running.
  - `gateway/Dockerfile` (multi-stage; `CGO_ENABLED=0`; `gcr.io/distroless/static-debian12:nonroot`; `GO_VERSION` passed from `go.mod`), `gateway/.dockerignore`.
  - `infra/compose/compose.yml`: gateway, `postgres:16-alpine`, `redis:7-alpine`; healthchecks; named volume `pgdata`; ports on 127.0.0.1 only; dev-only credentials `agentgate/agentgate` inline (approved).
  - `make dev` (checks Docker, `compose up -d --build --wait`, polls `/readyz` up to 30 s); `make dev-down` (keeps the volume). CI stays unit-only.
  - Touches `gateway/Dockerfile`, `gateway/.dockerignore`, `infra/compose/compose.yml`, `infra/compose/README.md`, `Makefile`.
- [x] 4. **Test fixtures + schema v1.** *(done via 4a + 4b, 2026-10-04)* Split into 4a/4b at planning (2026-10-04, Ivan): 10 files was over the ~8-file guideline.
  - `internal/testutil` (containerised PostgreSQL/Redis; open question 1).
  - Migration tool wiring + migration `0001` (`accounts`, `payments`, `ledger_entries`, `idempotency_keys`); `make migrate`.
  - Test: migrations apply cleanly, up and down.
- [x] 4a. **Schema v1 + test fixtures (Go).** *(PR #12, 0b661dd, main d14140e, 2026-10-04)*
  - `gateway/internal/store`: `migrations/00001_schema_v1.sql` (embedded), `Up`/`Down(ctx, *sql.DB)` through a goose v3.28.0 Provider (no globals).
  - `gateway/internal/testutil`: `Postgres(t)` (fresh migrated database per test, dropped WITH FORCE; pool capped at 8, container `max_connections=300`; a leaked connection fails the test after 30 s), `Redis(t)` (one of 16 logical DBs per test, flushed in and out). One container per test binary, Ryuk cleanup; skips without Docker locally, fails when `CI` is set. testcontainers-go v0.44.0.
  - Schema decisions (Ivan, 2026-10-04): uuid ids for accounts/payments, identity for ledger_entries; `direction` + positive `bigint` amount; GBP CHECK; `accounts.owner text NULL`; `payments.rail text NOT NULL` (CHECK added by subtask 9), `rail_ref`, nullable `mandate_id` (no FK until P2), payer ≠ payee CHECK, state text + CHECK; no `merchant_id` or `idempotency_key` on payments; `ledger_entries.payment_id NOT NULL`; `idempotency_keys` PK `(scope, key)` (scope default `''`), 32-byte `request_hash`, `completed` ⇔ `response_code` set (100–599). Zero-sum per payment is enforced by `ledger.Post` (subtask 5), not by 0001.
  - Tests: migrate up/down/up, 27 bad rows by SQLSTATE, testutil isolation tests. Suite ~3–4 s.
- [x] 4b. **Migration tooling + 3b follow-ups.** *(PR #11, a24b6a8, main a660315, 2026-10-04)* `gateway/cmd/migrate` (with goose's Postgres session locker, from the 4a review), `make migrate`, `make dev-logs`, compose `pg_isready -h`, compose README, SPEC-002 re-export.
  - **Follow-ups from the 3b review:** make the `make dev` failure hint runnable (it prints a `docker compose … logs` command that fails without `GO_VERSION`; prefix it or add `make dev-logs`); add `-h 127.0.0.1` to the `pg_isready` healthcheck before migrations depend on it.
  - As built (Ivan, 2026-10-04): `cmd/migrate up` only (no `down`; `store.Down` stays test-only), exit 0/1/2, fixed-message errors, never prints the DSN. `store.Up`/`Down` take goose's PostgreSQL session advisory lock (1 s poll, 5 min), so concurrent runs serialise. `make migrate` runs on the host via `go run` with `DATABASE_URL ?=` the dev DSN (exported for that target only, unexpanded via `$(value)`); `make dev` does not migrate. `make dev-logs`; `pg_isready -h 127.0.0.1`.
  - Tests: concurrent Up ×4 (one version row, no lock left), Up waits on a held lock, `cmd/migrate` usage/config errors, password never printed (AC8), success path up-then-no-op.
  - Review nits deferred: second Ctrl-C doesn't kill during shutdown; no applied/no-op log (needs `store.Up` to return results — Registry interface change); fold both mains' pool setup into `store.OpenPool` at subtask 5; test lock-holder connection not unlocked on early fatal; comment that a failed goose unlock returns a locked session to the pool (matters before the gateway ever calls `store.Up`); `migrate -h` exits 2.
- [x] 5. **Ledger.** *(done via 5a + 5b, 2026-10-04)* Split at planning (Ivan, 2026-10-04) into 5a and 5b; they share no files.
  - [x] 5a. **Ledger.** *(PR #13, a8c23aa + ed7a0db + 2e3f090, main 0bb983b, CI green, 2026-10-04)*
    - `internal/ledger`: `Post(ctx, tx, paymentID, entries)` with a balanced-sum check (≥ 2 legs, amounts > 0, debits = credits, overflow-checked). `Post` requires a `SERIALIZABLE` transaction and rejects any other; it can't retry inside the caller's transaction, so the retry lives in `store.Serializable(ctx, pool, fn)` (retries 40001/40P01, 5 attempts, jittered 10–200 ms backoff, respects `ctx`).
    - `Balance(account)` = credits − debits for every account kind (the schema's `user`/`merchant`/`clearing` kinds aren't asset/liability classes, so there is no debit-normal kind).
    - Unit and integration tests.
    - Ids are `uuid.UUID` (google/uuid) in the exported API; converted to `pgtype.UUID` only at the SQL boundary (Ivan at review, 2026-10-04: driver types stay in the data layer).
    - Review nits deferred: fold the isolation check into the INSERT (`WHERE current_setting('transaction_isolation') = 'serializable'`, `ErrNotSerializable` on 0 rows) to save a round trip per `Post`; explicit `$1::uuid` cast in the `INSERT … SELECT unnest`.
  - [x] 5b. **Pool and readiness** (must land before subtask 7). *(PR #14, 9b6b419, main c54f36d, CI green, 2026-10-04)*
    - **Follow-up from the 3a review:** this is the first step where the API shares the PostgreSQL pool, so stop `/readyz` from draining it: cache the readiness result for about 1 s with one shared in-flight check (a mutex, no new dependency); no second pool.
    - Fold both mains' pool setup into `store.OpenPool` (4b review nit), keeping each main's current error messages.
    - As built: `store.OpenPool(ctx, dsn)` is lazy (pgx defaults, no connect) and returns only `ErrInvalidDSN`/`ErrPoolSetup`; each main maps them with `errors.Is` to its unchanged messages. `/readyz` caches its result (503 included) for 1 s from the end of a run; concurrent callers share one run (mutex + done channel) on a detached context, so a disconnecting caller can't cut it short; failures are logged once per run. `NewRouter` signature unchanged; no new dependency.
    - Tests: OpenPool hides the password on a malformed DSN, stays lazy on an unreachable host; readyz: 50 callers → 1 run, TTL cache then rerun, cached 503, one log per run, cancelled caller doesn't time out others. Gateway suite 7.8 s with `-race`; readyz ×10 no flakes.
    - Review nits deferred: no `cmd/gateway` test for its error strings (give `run` a `getenv` seam); `ErrPoolSetup` branch untested (defensive); a disconnected `/readyz` caller logs status 0 instead of 503; put `run`'s store/close block in a `defer`; `context.Background()` reads clearer than `WithoutCancel`; tests for TTL-from-finish and all-callers-cancel; move the store package doc to `doc.go`.
- [x] 6. **Ledger properties.** *(PR #15, e06e69c, main 82f49fb, 2026-10-05; AC1 at ledger level, AC2 met)*
  - `internal/ledger/property_test.go` (`rapid`): AC1 at 10k operations, and AC2 by fault injection.
  - Decisions at planning (Ivan, 2026-10-05): AC1 ticked at ledger level now, re-run through the payment service after subtasks 7 and 10; a refund is the reverse posting on the same `payment_id`; a test-only `BEFORE INSERT` trigger, only in AC2's own test database (not a migration); `-short` may run fewer ops.
  - As built: `pgregory.net/rapid` v1.3.0 (direct). AC1: one test DB, 100 checks × 100 ops (create/settle with optional fee leg/fail/refund) against a test-local balance model; per-check and global SQL sums; an op counter fails the test below 10,000 outside `-short` (`-short`: 2,000, since rapid divides `-rapid.checks` by 5). AC2: own test DB; one subtest per fault kind, each its own `rapid.Check` (isolation query, insert send, mid-INSERT row k, error after `Post`, backend killed before COMMIT, ctx cancel at call 1–3), via a test-only `faultTx` wrapping `pgx.Tx`; asserts 0 entries and unchanged balances, then a control post must land all legs. `paymentSum` helper extracted in `ledger_test.go` (COALESCE → 0). No change to `ledger.go`.
  - Verify: `make lint test` green; mutation checks (commit-despite-error, dropped legs, corrupt refunds) each caught. Suite 18.2 s with `-race` (was 7.8 s; ~13 s is the kill-backend subtest reconnecting the pool), ledger package ~15.5 s, 5.0 s under `-short`; no flakes over 5 runs. Review: 0 blockers; should 1 (draw the fault → one subtest per fault so `-short` can't skip one) fixed.
  - Review nits deferred: helpers inside `rapid.Check` take the outer `t`, so an infra failure loses rapid's shrunk case and seed; no per-check deadline (`t.Context()`); `pg_terminate_backend(pid, 5000)` returning false would fail on a loaded runner; per-payment queries (one `GROUP BY` per check would do); `faultTx` comment should say only `QueryRow`/`Exec` are intercepted; comment that `minTotalOps` assumes `-rapid.checks` ≥ 100; optionally fewer kill-backend checks if the suite time matters.
  - Follow-up for subtasks 7/10: `fail` is model-only and settle/refund don't touch `payments.status`; re-run AC1 through the payment service so state transitions are covered.
- [x] 7. **Payment API + state machine.** *(7a, 7b-i, 7b-ii-a, 7b-ii-b; code complete 2026-10-05)*
  - `internal/payments` (service + handlers): create and get, forward-only transitions, error codes.
  - Tests: handler tests, and state transitions that refuse to go backwards.
  - **Follow-up from the 4a review:** `payments.updated_at` has no trigger, so every transition must set `updated_at = now()`; test that it moves.
  - Split at planning (Ivan, 2026-10-05): ~10 files and a Router/Logger interface change. 7b depends on 7a.
  - Decisions (Ivan, 2026-10-05, planner defaults): `Idempotency-Key` required in 7 but only checked for presence and length 1–255 (400 `idempotency_key_required`), no storage until 8; POST mounted now. 7 stops at `created` and posts nothing; the AC1 re-run moves to subtask 10 (the column is `state`, not `status`). `created → failed` is allowed directly. `rail` = constant `"internal"` set by the service. ADR-117: `payments` is the data layer for its own table, so `New(*pgxpool.Pool)` and its SQL live there; exported types and handlers stay driver-free (ADR-117 amended and accepted by Ivan, 2026-10-05: SQL in one `store.go` per owning package, no queries on another package's tables, transactions/retry via `store`). Forward-only = conditional `UPDATE … WHERE state = ANY($from)`, no trigger. `NewRouter` returns `*chi.Mux`. Q9 answered as below.
  - [x] 7a. **HTTP edge + logging follow-ups.** *(9cdccfe, PR #16 merged 2026-10-05, `main` f5e4638)* `httpx/router.go`: `NewRouter` returns `*chi.Mux`; exported `WriteJSON` and `WriteProblem(w, status, code, title)` (`application/problem+json` with a `code` member). `httpx/log.go`: open question 9 decisions. Tests in `httpx_test.go`; spec re-export.
    - As built: `NewRouter(log, checks...) *chi.Mux`; `WriteJSON(w, status, v)` (was private `writeJSON`, `ready.go` updated) and `WriteProblem(w, status, code, title)` share `writeBody`, which marshals first (encode failure → fixed 500 `internal_error` problem, nothing from the value) and sets `X-Content-Type-Options: nosniff` + `Cache-Control: no-store`. Logger: hard substrings + `cardholder`, `bearer`, `jwt`, `accesskey`, `passcode`; hard words + `otp`, `totp`, `hotp`, `pinblock`; soft + `session`, `sessionkey`, `track`, `trackdata` (explicit `tokens` dropped: plural rule); trailing digits dropped, plurals matched against term lists only; named string/int/uint/float/bool logged by underlying kind via reflect (not `uintptr`, pointers or `uuid.UUID`, which log OMITTED). Error-text rule added to Repo Kit `CLAUDE.md` (repo copy uncommitted with Ivan's other edits). No new dependency.
    - Verify: `make lint test` green, 19.4 s; ~25-key redactor probe found 5 leaks (`totp_code`, `passcode`, `sessionkey`, `pinblock`, `track2`), fixed (Ivan); wider list → open question 10. Review: 0 blockers; should 2 (empty body on encode failure) fixed with nits uintptr + nosniff/no-store; should 1 → Q10; should 3 (chi Recoverer prints panic value/stack to stderr, AC8) → first task of 7b.
    - Review nits deferred: `payload_sha256` redacted (`sha256` → `sha`, not a keep-suffix; add `sha`?); `WriteProblem` accepts empty `code`/`title`; `pin_ok`/`jwt_alg` redacted because hard words ignore keep-suffixes (by design); no test for the `uintptr` → OMITTED change.
  - [x] 7b. **Payments.** **First (7a review, AC8):** replace chi's `middleware.Recoverer`, which prints the panic value and stack to stderr outside the redacting logger, with our own recover middleware: fixed message + request ID + stack through `log` (never the panic value), `WriteProblem(500, "internal_error", …)`, re-panic on `http.ErrAbortHandler`. Log ids with `id.String()` (`uuid.UUID` logs as OMITTED). Then `internal/payments` (`payments.go`, `http.go` + tests): create/get, `MarkSettling`/`MarkSettled`/`MarkFailed`, error codes, `rapid` property that transitions never go backwards; mount in `cmd/gateway`; spec re-export.
    - Split at planning (Ivan, 2026-10-05): 7b-i recover middleware (2 files), 7b-ii `internal/payments` + `cmd/gateway` + re-export. ADR-117 amendment accepted 2026-10-05.
    - [x] 7b-i. **Recover middleware.** *(e74d467, PR #17 merged 2026-10-05, `main` 7cdc4b1)* As built: unexported `recoverer(log)` in `httpx/router.go` replaces `middleware.Recoverer` (same slot, inside `requestLog`). Logs "handler panicked" + `request_id` + `stack` (`debug.Stack()`: frame args are hex words, never dereferenced), writes the fixed 500 `internal_error` problem, re-panics `http.ErrAbortHandler` (identity, as net/http). Adds nothing once the response started: own `startedWriter` (final `WriteHeader`, `Write`, `Flush`, `Hijack`; `Unwrap` for `ResponseController`), because chi's writer leaves `Status()` 0 after a bare Flush. Tests: `TestRecoverer_HidesPanicValue` (string/error/struct secret absent from log, body, stderr), `…_RepanicsErrAbortHandler`, `…_AfterResponseStartedAddsNothing` (WriteHeader/Write/Flush). Mutation checks: chi Recoverer fails the first; Flush not marking fails the third.
      - Verify: `make lint test` green; `go test -race -count=1 ./...` 16.2 s. Review: 0 blockers; should (Flush/Hijack leave Status 0) fixed via `startedWriter`; nits fixed: stack comment, `captureStderr` closes the pipe in defer, write-then-panic test. Untested: Hijack path, interim 1xx.
      - For 7b-ii: `http.Server` has no `ErrorLog`, so a panic in `requestID`/`requestLog` (outside the recoverer) or a stdlib server error still prints to stderr; route it through the redacting logger when wiring `cmd/gateway`. `requestLog` logs without `defer`, so an aborted request gets no log line (nit). Still AC8 for 7b-ii: the `id.String()` test.
    - Split at planning (Ivan, 2026-10-05): 7b-ii-a payments package (`payments.go`, `store.go`, `http.go` + 2 test files); 7b-ii-b mount in `cmd/gateway`, `httpx.NewServerErrorLog` + test, re-export. Decisions (Ivan, planner defaults): request `{amount, currency, payer_account_id, payee_account_id, mandate_id?}`, unknown fields rejected, 64 KiB cap; 201 + `Location` + full payment (`id, state, amount, currency, payer_account_id, payee_account_id, mandate_id, rail, rail_ref, created_at, updated_at`), GET same shape. Codes: 400 `invalid_request`/`invalid_payment_id`/`idempotency_key_required`; 413 `request_too_large`; 422 `invalid_amount`/`unsupported_currency`/`same_account`/`account_not_found` (FK 23503, no reads of `accounts`, ADR-117); 404 `payment_not_found`; 500 `internal_error`. Transitions service-only in 7 (webhooks in 9/10); `MarkSettling(ctx, id, railRef)` writes `rail_ref`; same-state = `ErrInvalidTransition` (10 treats it as done). Server error log: fixed "http server error", "panic outside handler" for net/http's panic prefix, no stack (`slog.NewLogLogger` would log the panic value as the message). `requestLog` via `defer` out of scope. `POST /v1/payments` unauthenticated until P2 (acknowledged). POST p50/p99 baseline deferred to 8.
    - [x] 7b-ii. **Payments package.** `internal/payments` (`payments.go`, `store.go` for its SQL per ADR-117, `http.go` + tests), mount in `cmd/gateway/main.go`, spec re-export.
      - [x] 7b-ii-a. **Payments package.** *(9f5f024, PR #18 merged 2026-10-05, `main` b7f4eec)* As built: `New(pool)`, `Create`/`Get`, `MarkSettling(ctx, id, railRef)`/`MarkSettled`/`MarkFailed`, each transition one `UPDATE … WHERE state = ANY(from)` (0 rows → re-read: `ErrNotFound` or `ErrInvalidTransition`); FK 23503 → `ErrAccountNotFound`; timestamps UTC. Handler `POST /`, `GET /{id}`: key check first, `MaxBytesReader` 64 KiB, `DisallowUnknownFields`, exactly one JSON value (second `Decode` must be `io.EOF`). Additions beyond the decisions: `ErrMissingAccount` (nil account id → 400 `invalid_request`, checked before amount/currency), `ErrMissingRailRef`; JSON tags on `Payment`. Tests: round trip, 9 rejects, every start×target pair, 20 settle/fail races, rapid property, 19 HTTP error cases, DB failure 500, AC8 id-as-text + secrets absent. Mutation checks: dropping `state = ANY` fails transitions; raw uuid log fails AC8; `dec.More()` fails trailing `}`/`]`/padding.
        - Verify: `make lint test` green; `-race -count=1 ./...` 16.7 s; payments ×3 runs no flakes; ADR-117 grep clean. Review: 0 blockers; should 1 (trailing `}`/`]` and padding past 64 KiB accepted) fixed; nits fixed: missing-account first, UTC, "bytes" wording, FK comment for P2's mandate FK.
        - Deferred: should 2 (no context deadline on queries, gateway/CLAUDE.md) → 7b-ii-b request-timeout middleware; separate response type once 8/10 diverge; rapid 20 checks under `-short`; the deliberate `querier` copy (Registry notes it).
      - [x] 7b-ii-b. **Wiring.** *(on `feat/spec-002-7b-ii-b-wiring`, not pushed; see the commit in the Session Log)* Mount `payments.New(pool).Handler(log)` at `/v1/payments` in `cmd/gateway/main.go`; `httpx.NewServerErrorLog(log)` (fixed message, "panic outside handler" for net/http's prefix, no stack) as `http.Server.ErrorLog`, with a test; request-timeout middleware in `httpx.NewRouter` so every query has a deadline (gateway/CLAUDE.md); spec re-export.
        - Decisions (Ivan, 2026-10-05, planner defaults): own `withTimeout` middleware (not chi `middleware.Timeout`, which writes a bodyless 504 after the handler), constant 5 s (half of `WriteTimeout`), not configurable (`NewRouter` signature unchanged; unexported `newRouter(log, timeout, …)` for tests); chain `requestID → requestLog → recoverer → withTimeout`; health routes included; a timed-out request → 503 `request_timeout` in `payments.fail`, logged as error; server error log lines at Error.
        - As built: `cmd/gateway` mounts `payments.New(pool).Handler(log)` at `/v1/payments` and sets `ErrorLog: httpx.NewServerErrorLog(log)`. `NewServerErrorLog` → `log.New(serverErrorWriter{l}, "", 0)`; each Write (net/http writes a panic + stack in one) → one fixed Error line, never `p`. `withTimeout(d)` = `context.WithTimeout` + `defer cancel`, writes nothing. `payments.fail`: `DeadlineExceeded` → 503 `request_timeout` + `Retry-After: 1` (Ivan at review: an agent retrying blindly is the traffic this gateway exists for), logged at Error; `context.Canceled` (client gone) → Debug, nothing written (status 0 in the request log). Tests: panic outside every recoverer on a real `httptest` server (value, stack and net/http text absent from log and stderr); direct writes → both fixed messages; deadline ~5 s on mounted and own routes; `pg_sleep(30)` cut at 200 ms with `DeadlineExceeded`; expired request → 503 + `Retry-After`; cancelled request → no body, no Error line. Mutation checks: without `withTimeout` the deadline and slow-query tests fail (query ran 31 s); logging `p` fails both error-log tests; without the Canceled case a 500 is written to a gone client. Smoke test on `make dev`: POST 201 + `Location`, GET 200, "payment created" with `payment_id` text.
        - Verify: `make lint test` green; `-race -count=1 ./...` 16.9 s; httpx + payments `-count=3` no flakes; binary builds. Review: 0 blockers; should (client disconnect logged as Error 500) fixed; nits: HTTP/2 panic-prefix comment added, `Retry-After` added (Ivan). Follow-up: every ErrorLog line is Error, so repeated accept errors (fd exhaustion loop) or TLS scanner noise would be loud; revisit with TLS/rate-limited logging. The Debug line for a cancelled request isn't asserted (test logger at Info).
- [ ] 8. **Idempotency.**
  - `internal/idem` middleware: PostgreSQL key as source of truth, Redis in-flight lock, request hash, 409 on mismatch.
  - Test: AC3 with 100 concurrent goroutines against real containers.
  - Split at planning (Ivan, 2026-10-05): **8a** PostgreSQL key + request hash + mismatch + AC3, no Redis (`internal/idem/{idem,store}.go` + tests, `payments.CreateIdempotent`, handler replay/409); **8b** Redis in-flight lock behind a `Locker` interface, wiring in `cmd/gateway`, `created_at` index migration, `BenchmarkCreatePayment` + `make bench` and the POST p50/p99 baseline deferred from 7b-ii. Both stack on 7b-ii-b.
  - Decisions (Ivan, 2026-10-05; planner defaults plus Ivan's additions):
    1. Not middleware: the key and the payment commit in **one** `store.Serializable` transaction (`idem.Claim` → insert payment → `idem.Complete`); `idem` is the data layer for `idempotency_keys` and takes the `pgx.Tx`, like `ledger.Post`. Decisions (ADRs) › ADR-118 Idempotency records commit in the effect's transaction (proposed).
    2. A concurrent duplicate waits (8b: on the Redis lock, up to the request deadline; 8a: on the primary key, then replays after the serialization retry); deadline exceeded → 503 `request_timeout`. No 409 `in_progress`: AC3 needs all 100 responses identical.
    3. Redis down → PostgreSQL only, logged at Warn **and a degraded-mode counter incremented** (Ivan: degraded operation is a measured behaviour, not just a log line). 8b.
    4. Request hash = SHA-256 over **(caller identity, method, path, canonical body)** (Ivan: otherwise agent A's key could replay a response made for agent B's request on another endpoint). Canonical body = the decoded request re-marshalled (strict decoding makes it lossless). Caller is a placeholder (`""`, the same as `scope`) until mandates land in P2, when it becomes the agent ID. Test: same key and body on a different route → not a replay.
    5. Only the 201 is stored; 4xx and 5xx are not (validation is deterministic, 5xx/503 must stay retryable).
    6. No replay header: replays are byte-identical. A replay is logged at Info with the payment ID and the key (as a digest; see 8a as built).
    7. Retention 24 h. Claiming an expired key overwrites it inside the transaction, so correctness never depends on a sweeper. The `created_at` index lands in 8b; the periodic `DELETE` sweeper is deferred until after P1 (follow-up).
    8. Benchmark without a hard budget yet; the recorded baseline carries the **machine specification and commit SHA** (Ivan: Chapter 5 cites these; a number without its machine is not evidence).
  - [x] 8a. **PostgreSQL idempotency.** `internal/idem` (`idem.go`, `store.go`, `idem_test.go`), `payments.CreateIdempotent`, handler replay + 409 `idempotency_key_mismatch` + `store.ErrRetryLimit` → 503, AC3 test (100 goroutines), mismatch/route/expiry/rollback tests, `rapid` key-reuse property; spec re-export. *(on `feat/spec-002-8a-idempotency`, stacked on 7b-ii-b, not pushed; see the commit in the Session Log)*
    - As built: `internal/idem` (`idem.go`, `store.go`: `idempotency_keys` only). `ParseKey(caller, header)`, `Key.Digest()` (first 16 bytes of SHA-256(scope, key), hex: logged instead of the key), `Hash(Request{Caller, Method, Path, Body})` with u64 length prefixes, `Claim(ctx, tx, key, hash)` (reads first, then inserts `in_progress`; expired > 24 h → overwrite in-tx; completed + same hash → stored response; other hash → `ErrMismatch`), `Complete(ctx, tx, key, hash, resp)` (matches the hash; RowsAffected must be 1). `payments.CreateIdempotent` → `CreateResult{PaymentID, Status, Body, Replayed}`; the stored body is the `Payment` JSON and `WriteJSON(json.RawMessage)` makes first response and replays byte-identical. `Create` unexported (tests only). Handler: canonical body = `json.Marshal(createRequest)`, path = `TrimSuffix(URL.Path, "/")`, caller `idem.AnonymousCaller`; logs "payment created"/"payment replayed" with `payment_id`, `amount`, `idempotency_key_digest`; `idem.ErrMismatch` → 409, `store.ErrRetryLimit` → 503 `try_again_later` + `Retry-After: 1` (code name Ivan's call at review). Ivan kept the digest over the raw key (keys are caller-chosen text; the AC8 test sends a secret in one).
      - Tests: AC3 (100 goroutines: 1 payment, 1 key, 100 identical responses incl. `Location`; 1 created + 99 replayed logs), same key + other body → 409 while reordered JSON replays, failures not stored (corrected retry → 201), replay log, `TestFail_Mapping`; idem: hash covers each field + boundary, different route → mismatch (Ivan's case, idem level: no second HTTP route yet), other scope fresh, rollback leaves no key, expired reclaimed, expired-key race (second claimer replays the first's response), Complete without claim / wrong hash fails; `rapid` key-reuse property (counts every payment). Mutation checks: no Claim → 100 payments; path not hashed → route test fails; insert without read-first → losers get 23505 → 500 (why claim reads first); stray insert on replay → property fails.
      - Verify: `make lint test` green; `-race -count=1 ./...` 17.3 s; AC3 + race ×10 = 20/20, AC3 0.06–0.09 s warm. Review: 0 blockers; shoulds fixed (property counts all payments, retry-limit test, `Create` unexported, expired race test); nits fixed: hash-format contract in ADR-118, replay-decode comment, `Complete` matches the hash. Removed a `rapid` failfile from a 7b-ii-a mutation run that had been committed by mistake.
      - AC3 not ticked: "one set of ledger entries" needs subtask 10 (`ledger.Post` in the same tx, ADR-118); add a `ledger_entries` count to the AC3 test there.
      - Follow-ups: `payments` imports `net/http` for the stored status (service knows HTTP; fine under ADR-118); `Digest` is unsalted, so very short keys are guessable from logs (keys aren't credentials; HMAC if needed); hash chi's route pattern instead of the trimmed path once routes have parameters; after mutation checks, delete `testdata/rapid` failfiles before committing.
  - [ ] 8b. **Redis lock + expiry index + benchmark.** `idem/lock.go` (`Locker`: `SET NX PX` + token, Lua compare-and-delete, bounded poll-wait, 50 ms per call, fail open + Warn + degraded counter), wiring in `cmd/gateway`, migration `00002` (`created_at` index), `BenchmarkCreatePayment` (with/without Redis) + `make bench`, baseline with machine spec and commit SHA.
- [ ] 9. **Rail interface + internal and fake rails.**
  - `internal/rail`: the `RailAdapter` interface, the internal-ledger rail, and a deterministic fake PSP with scripted delays and failures.
  - A shared contract test suite (AC6).
- [ ] 10. **Settlement outbox + recovery.**
  - `settling` record before the rail call; recovery pass on start-up.
  - Webhook endpoint with dedupe and forward-only transitions.
  - Tests: AC4 and AC5 as integration tests.
- [ ] 11. **Chaos script.**
  - `scripts/chaos/` drives `make dev`: kill mid-settlement, duplicate webhook, reorder callbacks. It asserts AC4 and AC5 and runs in CI against the fake PSP.
  - Add `make chaos`.
- [ ] 12. **Stripe test-mode adapter** (stretch; open questions 5 and 6).
  - `internal/rail/stripe`: PaymentIntents in test mode and webhook signature verification.
  - Passes the AC6 contract suite when keys are present, and skips otherwise.
- [ ] 13. **Observability + log safety** (stretch: OpenTelemetry).
  - Tracing spans per stage and a baseline latency note for P6.
  - The AC8 log-redaction test is **not** stretch: it lands with subtask 1 if this one slips.

## Open questions
- [x] 1. **New dependencies** (each needs Ivan's OK; the repo CLAUDE.md says to stop and ask). The settled ones are already in Technology Choices: chi and `rapid`. Recommended:
  - `jackc/pgx/v5`, with hand-written SQL rather than `sqlc`: avoids generated code for a small schema.
  - `pressly/goose` for migrations.
  - `redis/go-redis/v9`.
  - `testcontainers-go` for fixtures, rather than tests depending on `make dev`.
  - `stripe/stripe-go` (subtask 12 only).
  - The OpenTelemetry Go SDK (subtask 13). → **Answered 2026-10-03:** approved as recommended (pgx/v5 + hand-written SQL, goose, go-redis/v9, testcontainers-go; stripe-go and OTel SDK for 12–13).
- [x] 2. **The `accounts` table** is in P1's deliverables but not in Architecture › Data model (first cut). Add it there with kinds user, merchant and clearing? → **Answered 2026-10-03:** yes. Added to Architecture › Data model (first cut).
- [x] 3. **Currency:** single currency (GBP) for P1, with amounts as `int64` minor units, and multi-currency left out of scope? → **Answered 2026-10-03:** GBP only; `int64` minor units.
- [x] 4. **Redis in P1:** keep it (in-flight lock, as the phase note says), or use PostgreSQL only until P3 needs Redis for velocity? The spec keeps Redis, but correctness never depends on it. → **Answered 2026-10-03:** keep Redis (in-flight lock); PostgreSQL stays the source of truth.
- [ ] 5. **Stripe test mode** needs a Stripe account and test keys. Ethics Answers Q4 says "sandbox via CLI, no account registration", but the Stripe CLI's `stripe login` normally needs an account. Which is it? Alternatives: `stripe-mock`, or stay on the fake PSP for P1.
- [ ] 6. **Time:** ADR-114 (proposed) gives P1 about 3 weeks at 10–15 h/week. Subtasks 1–11 are the core, which is already tight. Subtasks 12 (Stripe) and 13 (OpenTelemetry) are stretch and may move to P2. Agree?
- [ ] 7. **Module path:** `github.com/svout/agentgate/gateway`, one module per service rather than a root `go.work`?
- [ ] 8. **Branch protection** on `main` is still off. Turn it on before the first P1 PR, so CI must pass before merge?
- [x] 9. **Logging follow-ups from the 1b review** (decide when convenient; fold into subtask 7 when domain types appear): → **Answered 2026-10-05 (Ivan, planner defaults; built in 7a):** `cardholder` hard; `pan_last4`/`card_last4` stay redacted (ADR-105); named scalars logged by underlying string/int/bool kind, never via `Stringer`; add `bearer`, `jwt`, `otp`, `accesskey` (hard), `session` (soft), plural/digit normalisation (`dsn`/`databaseurl` already in since 3a); error-text rule added to the repo CLAUDE.md via Repo Kit.
  - Make `cardholder` a hard word?
  - Should `pan_last4` and `card_last4` be loggable?
  - Log typed scalars (`type PaymentID string`) by their underlying kind instead of omitting them?
  - Add `bearer`, `jwt`, `session`, `otp`, `access_key`, `dsn`, `database_url`, and plural or digit forms (`signatures`, `apiKeys`, `cvv2`)?
  - Add a repo CLAUDE.md rule that error text is logged verbatim, so don't wrap secrets or URLs with query strings into errors?
- [ ] 10. **Wider redaction list** (found by the 7a verify probe; not decided): these keys are logged in clear today: `seed`, `mnemonic`, `nonce`, `salt`, `signed_url`/`presigned_url`, `iban`, `account_number`, `routing_number`, `ssn`, `dob`, `expiry`/`exp_month`. Which should be redacted (secret-like: seed, mnemonic, signed URLs; personal or bank data: iban, account/routing number, ssn, dob, card expiry)? Recommended: redact all of them; `nonce` and `salt` soft (so `nonce_id` etc. stay). The five clear leaks (`totp`/`hotp`, `passcode`, `sessionkey`, `pinblock`, card `track` data) were fixed in 7a (Ivan, 2026-10-05). Also from the 7a review (pre-existing, not new risk): one-word lower-case compounds still log in clear: `accesstoken`, `refreshtoken`, `authtoken`, `idtoken`, `sessiontoken`, `apitoken`, `cardnumber`, `pincode`, `otpcode`, `track2data` (digit inside a word isn't split), `passphrase`, `pwd`. Likely fix: treat any word ending in `token` as soft, split digits inside words, add the rest as terms.
