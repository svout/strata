# AgentGate

A mandate enforcement and risk gateway for AI agent payments. Final year project; the dissertation argues that enforcement at this layer catches what the agentic payment protocols leave open, and measures what it costs.

## Where context lives
- **VAULT** = the Strata Obsidian vault, added as a directory (path in `$STRATA_VAULT`, set in `.claude/settings.local.json`). It owns planning: phases, specs, ADRs, Reuse Registry, logs. The repo owns code.
- The vault is the memory: a hook loads its Now block and last Session Log entry at session start; `/wrap-up` updates it at the end of every step. A `[strata]` reminder means work is unrecorded.
- Start every session with `/next-step`. It reads the **Now** block in `VAULT/Home.md`.
- Load context in this order, stopping when you have enough: Now block → current spec → its phase note → the relevant rows of `VAULT/Workflow/Reuse Registry.md` → the files the plan names.
- `docs/specs/` and `docs/adr/` are read-only exports from the vault, stamped with a hash. Edit the vault note and run `make export-docs`; never edit the export.
- **On any conflict in `docs/specs/` or `docs/adr/` (merge, rebase, cherry-pick), take the newer export whole; never hand-merge it.** The vault is the source of truth, and a hand-merged export fails its own stamp (that is what blocked `make export-docs` before). If neither side is current, resolve to either, then re-run `make export-docs`.
- **Don't export onto a branch that lacks the code the spec describes.** A new branch off `main` while a stack is unmerged would carry ticks and as-built notes for work `main` doesn't have; merge the stack first, then branch.

## Process (one session = one step = one ≤1-day subtask)
`/next-step` → `/spec` (if no approved spec) → `/plan-step` → build → `/verify` → `/review-step` → `/wrap-up` → `/clear`.
Procedure: `VAULT/Workflow/Step Loop.md`. Principles: `VAULT/Workflow/Development Operating System.md`.

## Architecture
- `gateway/` — Go. The hot path: RFC 9421 signature → mandate chain → policy → risk score → decision → audit → rail.
- `control-plane/` — Python (FastAPI). Policies, agent and merchant registry, keys, model training and ONNX export. Never on the hot path.
- `console/` — Next.js + TypeScript. Operator view: decisions, reason codes, mandate explorer, dispute replay.
- `harness/` — Python. Benign agent traffic, the eight attack scenarios, results generation.
- Postgres (ledger + audit chain), Redis (velocity, nonces, idempotency), Redpanda (decision events).

## Commands
`make dev` · `make test` · `make lint` · `make bench` (k6) · `make attack` (scenario suite) · `make demo` · `make export-docs` · `make migrate`
Scoped while building: `go test ./internal/<pkg> -run <Test>` · `uv run pytest <path>::<test>` · `pnpm --filter console vitest related <file>`

## AI use on this module (read this first)
This is an assessed university project. The supervisor has agreed that AI tools may be used across the project **on condition that every use is declared** (ADR-113 in `VAULT/Project/Decisions (ADRs).md`).

- **Declare every session.** The session-end hook adds a row to `VAULT/Dissertation/AI Use Log.md`; `/wrap-up` corrects it to what actually happened. That note is submitted as the "Acknowledgement of AI" appendix, so placeholder rows must never reach submission.
- **Ivan must understand everything that ships.** Anything described in the report has to be demonstrable on request, and unexplained work counts against methodological rigour. So: explain non-obvious code as you write it, keep functions readable over clever, and leave a comment where a design choice was made and why.
- **Verify sources.** Never invent a citation. Anything suggested for the bibliography must be a real, locatable source that Ivan reads before citing.
- Assessed prose (proposal, progress report, final report, slides, poster) may be drafted here, but Ivan edits and owns it, and word limits are hard: proposal 1,000–1,500; progress report 2,500–3,500; final report 8,000–10,000.

## Rules that decide reviews
- **Fail closed on authorisation, degrade on scoring.** An unverifiable signature or mandate is rejected. A scoring failure falls back to rules and marks the decision `degraded` — never a silent allow.
- Protocol formats (AP2, ACP, Visa TAP) are known **only** inside `internal/mandate`. Everything downstream sees the canonical mandate type.
- Every decision carries reason codes. No bare scores, no unexplained blocks.
- Every state-changing endpoint is idempotent; every money movement is double-entry and sums to zero.
- Audit records are written in the same transaction as the decision, chained to the previous hash. Never log instead of chaining.
- Features are defined once in `control-plane/ml/features.py` and mirrored in Go, with parity tests. A feature added on one side only is a bug.
- No secrets, tokens, mandate bodies or personal data in logs — log IDs and digests.
- Error text is logged verbatim (the redactor checks keys, not messages), so never wrap secrets, DSNs, tokens or URLs with query strings into an error; return a sentinel and keep the detail out.
- No real payment credentials or personal data anywhere. Sandbox and synthetic only.
- **No startup path may call anything capable of applying a migration;** startup may only *read* the schema version (`store.SchemaVersion`). Migrations run only through `make migrate` (`cmd/migrate`). A startup path that can migrate is how a deploy silently migrates production: this includes library calls with migration side effects (goose's `Provider.GetDBVersion` creates the version table and takes the migration lock), so check a library call's side effects before using it at startup.

## Performance budgets (the dissertation's cost claim)
Signature 2 ms · mandate 3 ms · policy 4 ms · scoring 8 ms · decision+audit 5 ms · **total added p99 < 25 ms** at 200 rps. A change that breaks a budget is not done; measure with `make bench` before claiming otherwise.

## Stop and ask a human when
Acceptance criteria are unclear; the change touches a Registry interface, crypto, or the audit chain format; a new dependency is needed; an ADR-worthy decision appears; the step is bigger than one day.

## Commits
Conventional Commits, one per step, with the spec ID: `feat(gateway): verify cart mandate against intent [SPEC-004]`.
`main` is protected: every change goes through a PR with green `lint`, `test` and `chaos`, merged with a merge commit (never squash or rebase); never push to `main`.
