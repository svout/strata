---
description: Review this step's diff against CLAUDE.md, the spec, the Registry and budgets
---
Stage 6 of the Step Loop. Use the **reviewer** agent on `git diff` (staged + unstaged) for this step.

Report findings as `blocker | should | nit` with file:line. Blockers include: duplicated logic that exists in the Registry; bypassed tenant helper; hand-edited generated files; untyped endpoints; missing tests for a service function; N+1 queries; secrets or note contents in logs; a Registry interface changed without the spec saying so; whole-file rewrites that dropped existing code.

Fix blockers now; list the rest for the human.
