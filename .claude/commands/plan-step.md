---
description: Plan the next subtask with a reuse check — no code yet
---
Stages 2–3 of the Step Loop. Use the **planner** agent.

Preconditions: the spec is `approved`. If not, stop and say so. Once the repo exists, make sure `docs/specs/` has the current export of the spec (copy vault → repo, one way) so the PR's AI review can see it.

Before planning, read every ADR the subtask cites (in the spec bullet, its as-built notes, and the Registry rows the plan reuses) in `VAULT/Project/Decisions (ADRs).md`. This is a required step, not a judgement call: a plan that contradicts an ADR it didn't read (e.g. which rail posts which ledger leg, ADR-123) is the failure this prevents.

Produce a plan of at most 20 lines:
- **Subtask:** (quote it from the spec)
- **Files:** exact paths to create / edit (read each existing one first)
- **Reuse check:** for every function/module needed → `reuse <Registry item>` | `extend <module>: <change>` | `new <home>: <why nothing fits>`
- **Tests:** which tests prove which acceptance criteria
- **Budgets:** perf budgets that apply
- **ADRs read:** the IDs, one line each on what constrains this step
- **Gates hit:** Registry interface change / new dependency / ADR-worthy / auth-RLS-secrets-deletion / >1 day — or "none"

If any gate is hit, or the plan exceeds ~8 files or two services, stop and ask (propose a split). Otherwise wait for "go".
