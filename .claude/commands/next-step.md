---
description: Orient — find the current phase, workstream, spec and next subtask from the vault
---
Stage 0 of the Step Loop. Read ONLY these, in order:
1. The **Now** block in `VAULT/Home.md`.
2. The spec it points to (if any), then that spec's workstream note.
3. The latest entry in `VAULT/Workflow/Session Log.md`.

Then reply in exactly this shape and stop:
- **Where:** Phase / workstream / spec
- **Next subtask:** the first unchecked subtask (or "needs a spec → run `/spec <workstream>`")
- **Done so far:** one line from the Session Log
- **Watch out:** any open question or blocker

If the Now block disagrees with the spec checkboxes, say so and propose the corrected Now block. Do not start planning or coding.
