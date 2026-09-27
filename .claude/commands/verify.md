---
description: Run scoped then full checks and tick acceptance criteria
---
Stage 5 of the Step Loop. Use the **tester** agent.

1. Run the scoped tests for the files changed in this step.
2. Run `make lint test` (before the Makefile exists: the closest equivalent, and say which).
3. For each acceptance criterion this subtask covers, state ✅/❌ with the test that proves it.
4. Check the perf budgets named in the plan; report numbers where measured.

Fix failures within the plan's scope. If a fix needs files outside the plan, stop and ask.
