---
description: Record the step in the vault, update Now, Registry and logs, then commit
---
Stage 7 of the Step Loop. Do all of these, then show a 5-line summary:

1. Tick the subtask in the spec. All ticked → spec `status: done`; export it (overwrite) to `docs/specs/`; set the workstream `status` and the phase-note table.
2. Rewrite the **Now** block in `VAULT/Home.md` to point at the next subtask.
3. `VAULT/Workflow/Reuse Registry.md`: add rows for new shareable code; flip `planned → exists` with the real path; add a Changelog line. Edit only the rows you touch.
4. Prepend an entry to `VAULT/Workflow/Session Log.md` (format at the top of that note).
5. `VAULT/Dissertation/AI Use Log.md`: the session-end hook adds a placeholder row for every session that changed files — replace its category and description with what you actually did (categories are listed in that note). A session with no file changes but real help given (explanations, literature, review) gets a row written by hand. This note is submitted as the "Acknowledgement of AI" appendix, so it has to be true and specific.
6. If context was wrong at any point this session, add a row to `VAULT/Workflow/Friction Log.md` with a cause tag.
7. Decision made? Draft an ADR in the phase's `Decisions (ADRs)` note from `Template - ADR`.
8. Commit: `<type>(<scope>): <what> [SPEC-NNN]`. Don't push unless asked.

Then tell the human to `/clear` before the next step.
