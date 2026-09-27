---
description: Draft a spec (with ≤1-day subtasks) for a workstream
argument-hint: <workstream number or name>
---
Stage 1 of the Step Loop, for workstream: $ARGUMENTS

1. Read that workstream note in `VAULT/Phase N - */`, the phase note's build order, and `VAULT/Workflow/Reuse Registry.md` rows linked to it. Nothing else.
2. Pick the next free id by listing `VAULT/Phase N - */Specs/` (SPEC-001, SPEC-002…; numbers are global across phases).
3. Create `VAULT/Phase N - */Specs/SPEC-NNN <Feature>.md` from `Template - Spec` (in `VAULT/Templates`), `status: draft`:
   - acceptance criteria copied/sharpened from the workstream, each testable;
   - Reuse plan table filled from the Registry;
   - subtasks of ≤ 1 day each, ordered so each leaves the build green, each naming the files/modules it touches;
   - open questions for anything the notes don't settle.
4. Set the workstream frontmatter `spec: "[[SPEC-NNN …]]"` and `status: spec`.
5. Reply with the subtask list and open questions. Ask the human to set `status: approved`. Do not code.
