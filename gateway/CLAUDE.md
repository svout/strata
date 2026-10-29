# gateway — local rules (Go)

- Handlers are thin: parse, call the pipeline, render. The pipeline stages live in `internal/{httpsig,mandate,policy,risk,audit,rail}`.
- Every stage contributes to one `Decision` value; no stage returns a bare bool or writes its own response.
- `internal/mandate` is the only package that knows AP2/ACP/TAP wire formats. Add a new protocol as an adapter, never as a branch in the pipeline.
- Fail closed in `httpsig` and `mandate`; degrade (rules fallback + `degraded` flag) in `risk`.
- No blocking I/O without a context deadline. Budgets are in the root CLAUDE.md; a stage that exceeds its budget fails the review.
- Redis is behind `FeatureSource` and `Idempotency`; no raw client calls elsewhere.
- Errors: typed, wrapped with `%w`, mapped to problem+json at the edge. Never leak internal detail to an agent.
- Tests: table-driven plus property-based (`rapid`) for ledger and idempotency invariants; `-race` in CI.
- Benchmarks live next to the code (`_test.go`, `Benchmark…`) — the dissertation quotes them.
- A comment that states *why* must be verifiable from the code or a cited ADR (or a measurement it names). Otherwise mark it as an assumption ("assumed: …") or leave it out. A confidently wrong reason is worse than none, because the next reader trusts it. (Ivan, 2026-10-28, after four such comments: Q11-i's "one leaf page", the 15-second coupling, and Q11-ii review nits 1–2.)
