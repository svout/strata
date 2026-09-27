---
description: Break a new phase into workstreams in the vault (only after the previous phase's exit review)
argument-hint: <phase number and name>
---
Phase-level loop, for: $ARGUMENTS

1. Confirm the previous phase note's exit criteria are all ticked and its post-mortem is written. If not, stop.
2. Read the roadmap section for this phase (ask the human to paste it if it isn't in the vault).
3. Create `VAULT/Phase N - <Name>/` with: a phase note (same shape as `Phase 0 - Foundations`), one note per workstream from `Template - Workstream`, `Phase N - Decisions (ADRs)`, `Phase N - Glossary`, and an empty `Specs/`.
4. For each workstream, list the Reuse Registry items it depends on and any new shared modules it will create (add those as `planned` rows).
5. Update the Phases table and the Now block in the hub note. Ask the human to review before any spec is written.
