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
<!-- DO NOT EDIT: exported from vault "Specs/SPEC-002 Payment Core and Ledger.md" by make export-docs | source-sha256: 76f7f81aca0dd55e86471781aec20e89c8741cbfee44fcbf5a1e8be3845b6dd0 | content-sha256: ceae8063d3911d5ef61f882e3fe15269fcc2c254884446aa3f4ac28a2e0dfdfa -->

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
- [ ] **AC1 Money conservation:** across 10,000 randomised create/settle/fail/refund operations (property test, `rapid`), every payment's ledger entries sum to zero, and all account balances sum to zero.
- [ ] **AC2 No partial postings:** a failure injected at any point inside a posting leaves **zero** entries for that posting (property or fault-injection test).
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
- [x] 4a. **Schema v1 + test fixtures (Go).** *(0b661dd on `feat/spec-002-4a-schema`, 2026-10-04; not yet pushed/merged)*
  - `gateway/internal/store`: `migrations/00001_schema_v1.sql` (embedded), `Up`/`Down(ctx, *sql.DB)` through a goose v3.28.0 Provider (no globals).
  - `gateway/internal/testutil`: `Postgres(t)` (fresh migrated database per test, dropped WITH FORCE; pool capped at 8, container `max_connections=300`; a leaked connection fails the test after 30 s), `Redis(t)` (one of 16 logical DBs per test, flushed in and out). One container per test binary, Ryuk cleanup; skips without Docker locally, fails when `CI` is set. testcontainers-go v0.44.0.
  - Schema decisions (Ivan, 2026-10-04): uuid ids for accounts/payments, identity for ledger_entries; `direction` + positive `bigint` amount; GBP CHECK; `accounts.owner text NULL`; `payments.rail text NOT NULL` (CHECK added by subtask 9), `rail_ref`, nullable `mandate_id` (no FK until P2), payer ≠ payee CHECK, state text + CHECK; no `merchant_id` or `idempotency_key` on payments; `ledger_entries.payment_id NOT NULL`; `idempotency_keys` PK `(scope, key)` (scope default `''`), 32-byte `request_hash`, `completed` ⇔ `response_code` set (100–599). Zero-sum per payment is enforced by `ledger.Post` (subtask 5), not by 0001.
  - Tests: migrate up/down/up, 27 bad rows by SQLSTATE, testutil isolation tests. Suite ~3–4 s.
- [x] 4b. **Migration tooling + 3b follow-ups.** *(on `feat/spec-002-4b-migrate`, stacked on 4a, 2026-10-04; not yet pushed/merged)* `gateway/cmd/migrate` (with goose's Postgres session locker, from the 4a review), `make migrate`, `make dev-logs`, compose `pg_isready -h`, compose README, SPEC-002 re-export.
  - **Follow-ups from the 3b review:** make the `make dev` failure hint runnable (it prints a `docker compose … logs` command that fails without `GO_VERSION`; prefix it or add `make dev-logs`); add `-h 127.0.0.1` to the `pg_isready` healthcheck before migrations depend on it.
  - As built (Ivan, 2026-10-04): `cmd/migrate up` only (no `down`; `store.Down` stays test-only), exit 0/1/2, fixed-message errors, never prints the DSN. `store.Up`/`Down` take goose's PostgreSQL session advisory lock (1 s poll, 5 min), so concurrent runs serialise. `make migrate` runs on the host via `go run` with `DATABASE_URL ?=` the dev DSN (exported for that target only, unexpanded via `$(value)`); `make dev` does not migrate. `make dev-logs`; `pg_isready -h 127.0.0.1`.
  - Tests: concurrent Up ×4 (one version row, no lock left), Up waits on a held lock, `cmd/migrate` usage/config errors, password never printed (AC8), success path up-then-no-op.
  - Review nits deferred: second Ctrl-C doesn't kill during shutdown; no applied/no-op log (needs `store.Up` to return results — Registry interface change); fold both mains' pool setup into `store.OpenPool` at subtask 5; test lock-holder connection not unlocked on early fatal; comment that a failed goose unlock returns a locked session to the pool (matters before the gateway ever calls `store.Up`); `migrate -h` exits 2.
- [ ] 5. **Ledger.**
  - `internal/ledger`: `Post(ctx, tx, entries)` with a balanced-sum check, `SERIALIZABLE` plus retry, and `Balance(account)`.
  - Unit and integration tests.
  - **Follow-up from the 3a review:** this is the first step where the API shares the PostgreSQL pool, so stop `/readyz` from draining it: cache the readiness result for about 1 s (or singleflight), or give readiness its own 1-connection pool.
- [ ] 6. **Ledger properties.**
  - `internal/ledger/property_test.go` (`rapid`): AC1 at 10k operations, and AC2 by fault injection.
- [ ] 7. **Payment API + state machine.**
  - `internal/payments` (service + handlers): create and get, forward-only transitions, error codes.
  - Tests: handler tests, and state transitions that refuse to go backwards.
  - **Follow-up from the 4a review:** `payments.updated_at` has no trigger, so every transition must set `updated_at = now()`; test that it moves.
- [ ] 8. **Idempotency.**
  - `internal/idem` middleware: PostgreSQL key as source of truth, Redis in-flight lock, request hash, 409 on mismatch.
  - Test: AC3 with 100 concurrent goroutines against real containers.
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
- [ ] 9. **Logging follow-ups from the 1b review** (decide when convenient; fold into subtask 7 when domain types appear):
  - Make `cardholder` a hard word?
  - Should `pan_last4` and `card_last4` be loggable?
  - Log typed scalars (`type PaymentID string`) by their underlying kind instead of omitting them?
  - Add `bearer`, `jwt`, `session`, `otp`, `access_key`, `dsn`, `database_url`, and plural or digit forms (`signatures`, `apiKeys`, `cvv2`)?
  - Add a repo CLAUDE.md rule that error text is logged verbatim, so don't wrap secrets or URLs with query strings into errors?
