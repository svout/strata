---
type: spec
id: SPEC-001
phase: "P0 Proposal and Threat Model"
status: approved
revision: 2
revised: 2026-10-02
owner: ivan
adr:
  - ADR-105
  - ADR-110
  - ADR-112
  - ADR-113
created: 2026-09-26
---
<!-- DO NOT EDIT: exported from vault "Specs/SPEC-001 Proposal Package.md" by make export-docs | source-sha256: 6230c2daf587aaf4f0a5a85dff790a4f773bd1a2db6410d299a83e6d4dbe414f | content-sha256: bcd26ac4d18d59929d3e127cea84992d7deaba36c1bc96f1ab7107a331d459e3 -->

# SPEC-001 Proposal Package

## Problem
The topic is agreed (2026-09-26, Supervisor Log), but nothing written exists that the supervisor can sign off: no proposal document, no agreed threat model, no measurement method for every success criterion, no ethics position, thin literature, and no repo. Without these, P1 has no approved scope to build against.

**Revision 2 (2026-10-02).** Two facts changed the spec after revision 1 was approved:
- **The handbook sets the proposal's shape.** The COMP6013 handbook (COMP6013 Requirements) requires a **1,000–1,500-word** proposal in its own seven-part structure, including a Gantt chart, a version management plan and a data management plan. It must be submitted with ethics form **E1** in Semester 1 week 4. There are no extensions on formative deadlines.
- **ADR-112 makes assessed text Ivan's.** Every word of assessed text is written by Ivan. The revision-1 Proposal and the Ch 1–2 drafts were written by Claude, so they are **research notes only** and cannot be submitted. *Superseded on 2026-10-02 by ADR-113:* the supervisor agreed AI use across the project on condition that every use is declared. Claude may draft again, and Ivan edits, owns and can explain everything submitted.

Revision 2 keeps the revision-1 groundwork (threat model, metrics, literature, repo) and replans the rest by who may do it.

## Goal and non-goals
**Goal:** a proposal package the supervisor signs off: proposal document, finished Threat Model, Research Questions and Evaluation covering every success criterion, ethics form submitted, 12–15 sources in Literature Notes, and an empty repo with Strata/Repo Kit/Repo Kit installed and CI green.
**Goal (revision 2):** Ivan submits his own 1,000–1,500-word proposal plus E1 on Moodle by S1 week 4, after the supervisor has seen it. Under ADR-113, Claude may draft, plan and review. Ivan edits and owns everything submitted and can explain every part. Every AI contribution is declared in AI Use Log.
**Non-goals:** any gateway, control-plane or console code; choosing ML models or features beyond naming them; final chapter prose (the revision-1 Ch. 1–2 drafts feed the progress report's Introduction and Background review); fixing calendar dates (plan stays in week numbers until deadlines are known).

## User stories
1. As the supervisor, I can read one short document and approve the scope, RQs, method and plan.
2. As the student, I know for each success criterion exactly how it will be measured before building anything.
3. As the examiner (later), I can trace every attack scenario to the control it tests and the expected outcome.

## Acceptance criteria
Planning spec: each criterion is checked by inspection of the named note, not by code tests.
- [x] Proposal document exists with sections: problem, gap, RQs, method, plan (week numbers), risks, ethics; within the length the department sets (default ≤ 3,000 words if none given). **Restored (ADR-113):** this draft (~2,400 words) is the starting point for the rev-2 proposal criterion below. It must still be cut to handbook length and owned by Ivan.
- [x] Each of the eight scenarios in Threat Model names its attacker class, the control under test, the expected outcome (allow / step up / block), and the phase that builds the control.
- [x] Every success criterion in Project Brief › Success criteria maps to a row in Research Questions and Evaluation › Metrics with a metric and instrument. This includes ledger invariants and chapter mapping, which have no row today.
- [x] Literature Notes holds ≥ 12 sources: the four protocol specs, ≥ 4 peer-reviewed papers, each with the per-source template filled, and a one-paragraph gap statement citing them.
- [ ] Ethics form submitted (or written confirmation it isn't needed), recorded in Supervisor Log; consistent with ADR-105.
- [x] Repo created, `Repo Kit/install.sh` run, CI passes on an empty build (lint + empty test job); repo URL recorded in Strata/Home.
- [ ] Supervisor sign-off on scope and RQs recorded in Supervisor Log with the date.
- [x] First drafts of Chapter 1 (Introduction) and Chapter 2 (Background) exist in `Dissertation/`. **Restored (ADR-113):** they feed the progress report's Introduction and Background review (S1 week 12), which is out of scope for this spec.

**Revision 2 criteria**
- [ ] **Ivan's proposal** exists. Ivan has edited it and owns it, and can explain every part of it; starting from Claude's draft is allowed (ADR-113). It has the handbook's seven sections, including a **Gantt chart** (Project management), a **version management plan** (Methodology), a **data management plan** consistent with ADR-110 (Project management) and a bibliography in University or IEEE style. It is **1,000–1,500 words**, counted by the handbook rule: headings, tables and in-text citations included; cover, contents, references and appendices excluded.
- [ ] **E1** approved by the supervisor and attached to the proposal (replaces the ethics criterion above when met).
- [ ] **Proposal and E1 submitted on Moodle** by the S1 week 4 deadline; date and confirmation recorded in Supervisor Log.
- [ ] **AI Use Log is complete** for every Claude contribution to date: drafts, ethics answers, scaffolding, CI follow-ups and reviews. It is ready to become the "Acknowledgement of AI" appendix.
- [ ] **Supervisor's AI agreement in writing:** Sam's email confirming ADR-113 is pasted into AI Use Log and noted in Supervisor Log.
- [ ] **Sources are reconciled:** every source in Literature Notes appears in Reference Library with a working URL, and each one cited in the proposal is marked *read by Ivan*.
- [ ] **A weekly project log exists** with the handbook's fields (planned objectives, tasks done, targets met, supervisor meetings), and the current week is filled in by Ivan.

## Design notes
**Outputs:** `Dissertation/Proposal.md` (new; export to the department's format at the end), edits to `Project/Threat Model.md`, `Dissertation/Research Questions and Evaluation.md`, `Dissertation/Literature Notes.md`, `Dissertation/Supervisor Log.md`, new `Dissertation/Ch1 Introduction.md`, `Dissertation/Ch2 Background.md`.
**Repo:** empty monorepo skeleton only (`gateway/`, `control-plane/`, `console/` placeholders per Strata/Project/Architecture); CI = one workflow running lint and an empty test target.
**Division of labour (ADR-113, rev. 2):** each subtask names an owner.
- **Claude** may draft, plan, implement permitted work and review.
- **Ivan** edits and owns everything submitted, makes the submissions, and must be able to explain all of it.
- **Declaration rule:** every subtask, whoever owns it, adds or updates an AI Use Log row when Claude contributed (ADR-113 rule 1). Sources are read by Ivan before they are cited (rule 3).

**Performance budget:** n/a for this spec. Latency targets are *stated* in the proposal from Strata/Project/Architecture › Latency budget, not measured.

## Reuse plan
Check Strata/Workflow/Reuse Registry first. No Registry rows belong to P0 (all rows are code components for P1–P7); this spec reuses existing notes instead.
| Need | Reuse / extend / new | Registry item or new home |
|---|---|---|
| Threat scenarios | extend | Threat Model (eight scenarios already drafted) |
| RQs, metrics, experiment design | extend | Research Questions and Evaluation |
| Problem, pitch, success criteria | reuse | Project Brief, Market and Positioning |
| Architecture and stack for the method section | reuse | Strata/Project/Architecture, Technology Choices, Decisions (ADRs) |
| Source list and per-source template | extend | Literature Notes |
| Repo CLAUDE.md and `.claude/` | reuse | Strata/Repo Kit/Repo Kit via `install.sh` |
| Proposal document | extend (rev. 2) | `Dissertation/Proposal.md`: Claude's draft, cut and restructured to the handbook, then edited and owned by Ivan |
| Gantt chart, data-management plan, version-management plan | new | `Project/Gantt Chart.md`, `Project/Data Management Plan.md`, `Project/Version Management Plan.md` |
| Module rules, rubric, deadlines | reuse | COMP6013 Requirements |
| AI acknowledgement record | extend | AI Use Log |
| Source index | extend | Reference Library |
| Weekly project log | new | `Dissertation/Project Log.md` (structure by Claude, entries by Ivan) |

## Subtasks
Each ≤ 1 day, one session. "Green" here means every note it touches is consistent and linked.

### Revision 1 (2026-09-26 → 09-27)
- [x] 1. **Threat model pass**: add attacker class and building phase to each of the eight scenarios; check residual risks. Touches `Project/Threat Model.md`.
- [x] 2. **Metrics coverage**: add metric rows for ledger invariants (chaos tests, P1) and chapter mapping; tie each row to a success criterion. Touches `Dissertation/Research Questions and Evaluation.md`, `Project/Project Brief.md` (links only).
- [x] 3. **Literature I, specs**: read and note the four protocol specs (AP2, ACP, TAP, RFC 9421) with the per-source template. Touches `Dissertation/Literature Notes.md`.
- [x] 4a. **Literature II, papers**: find and note ≥ 5 peer-reviewed papers (prompt injection, delegation, tamper-evident logging, fraud ML, explainability) plus the arXiv agent-payment preprints, venues verified. Touches `Dissertation/Literature Notes.md`.
- [x] 4b. **Literature III, industry + gap**: note the three industry sources to reach ≥ 12, then write the gap paragraph citing the notes. Touches `Dissertation/Literature Notes.md`.
- [x] 5. **Proposal draft**: write `Dissertation/Proposal.md` (problem, gap, RQs, method, week-numbered plan, risks, ethics) from the notes above.
- [x] 6a. **Ethics answers**: pre-draft `Dissertation/Ethics Answers.md` from the vault; resolve the dataset question (ADR-110). Touches `Dissertation/Ethics Answers.md`, `Dissertation/Proposal.md` §7, `Project/Decisions (ADRs).md`.
- [ ] ~~6b. **Ethics submission** (human): submit the department form or obtain a written exemption; Claude records it in `Dissertation/Supervisor Log.md` and ticks the AC.~~ → replaced by 16 below.
- [x] 7. **Repo bootstrap**: create the repo, run `Repo Kit/install.sh`, add skeleton dirs and a CI workflow with lint + empty test; confirm green; record URL in `Home.md`. First step run outside vault mode.
- [x] 7c. **CI follow-ups**: apply the subtask-7 review findings in one PR. Anchor ignores (`/data/`, bin/build/out), fix pnpm/Node setup for the console, ruff `src`, pin golangci-lint, cancel-in-progress only on PRs, control-plane README phases, decide `*.onnx`. Touches `.gitignore`, `.github/workflows/ci.yml`, `control-plane/README.md`.
- [x] 8a. **Ch 2 draft**: first draft of Ch. 2 Background (~4,000 words) from Literature Notes. Touches `Dissertation/Ch2 Background.md`.
- [x] 8b. **Ch 1 draft + links**: first draft of Ch. 1 Introduction (~1,500 words); link both chapters from the Write-up Plan. Touches `Dissertation/Ch1 Introduction.md`, `Dissertation/Write-up Plan.md`.
- [ ] ~~9. **Sign-off**: send the proposal, apply feedback, record sign-off.~~ → replaced by 16 below.

### Revision 2 (2026-10-02): owner in brackets
- [x] 10. **[Claude] AI Use Log completeness.**
  - Add the missing rows: Ethics Answers (09-27), Ch 2 and Ch 1 drafts (09-27), the Proposal §7 rewording (09-27), the CI follow-ups (7c, 09-27), and this spec revision with the three plans (10-02).
  - Correct the scaffolding date to 09-27.
  - Touches `Dissertation/AI Use Log.md`.
- [x] 11. **[Claude] Reference Library reconciliation** (literature-search).
  - Update the AP2 row to v0.2 terms with the repo URL and commit `e1ea56d`; add the x402 URL.
  - Make sure every Literature Notes source has a Library row and vice versa, and add a "read by Ivan" column (empty).
  - Touches `Dissertation/Reference Library.md`.
- [x] 12. **[Claude] Project log structure.**
  - Create `Dissertation/Project Log.md` with the handbook's weekly fields and one empty block per semester week.
  - Pre-fill factual dates only (meetings already in Supervisor Log); Ivan writes the entries.
  - Touches `Dissertation/Project Log.md`.
- [ ] 13. **[Ivan] Remaining supervisor items.**
  - Moodle dates.
  - Progress report deadline: S1 week 12 (handbook) or week 13 (lecture)?
  - Ethics form: E1 alone, or E1 + E2?
  - Is a simulated evaluation acceptable (deterministic agents, ADR-111; sandbox rail)?
  - Repo: keep it private, or make it public (and when)?
  - Claude records the answers in Supervisor Log. Settled already (ADR-113, 2026-10-02): AI use across the project with full declaration, the scaffolding in 7 and 7c stands, the vault phase plan is the Gantt source, and drafting is permitted with Ivan editing, owning and able to explain all submitted text.
- [ ] 14. **[Claude drafts → Ivan edits and owns] Proposal to the handbook.** *Claude's draft done 2026-10-02 (1,413 words; rev 1 in Proposal rev1 (notes)); waiting for Ivan's edit.*
  - Cut and restructure Proposal into the handbook's seven sections at 1,000–1,500 words.
  - Pull in the Gantt chart (18), the version-management plan (20) and the data-management plan (19).
  - Ivan then edits it until it is his, and can explain every part.
  - Touches `Dissertation/Proposal.md`.
- [ ] 15. **[Claude] Review Ivan's edited draft**, with an optional trim pass.
  - Check against the 5 × 20 rubric, the word count by the handbook rule (more than 10% over is not marked), the formatting rules, consistency with the ADRs and Threat Model, and that every citation exists in Literature Notes and has been read by Ivan.
  - Comments go in `Dissertation/Proposal Review.md`; a trim pass is allowed if Ivan asks.
- [ ] 16. **[Ivan] E1 + submission** (supersedes 6b and 9).
  - Fill in E1 (using Ethics Answers as notes), get Sam's approval, and submit the proposal plus E1 on Moodle by S1 week 4.
  - Claude records the date and confirmation in Supervisor Log and ticks the P0 deliverables.
- [ ] 17. **[Claude] Plan re-anchor** (vault plan; blocked on Moodle dates).
  - Map the phase table in Strata/Home to semester weeks, with the progress report as a hard milestone (S1 week 12 vs 13: confirm).
  - Add the poster and presentation to P8 Dissertation Write-up.
  - Touches `Home.md`, `Phases/P8 Dissertation Write-up.md` and the week-date table in `Dissertation/Project Log.md`.
- [x] 18. **[Claude] (b) Gantt chart.**
  - P0–P8 against semester weeks, in Mermaid, with week numbers and a note to re-anchor once the Moodle dates are confirmed.
  - Include how the plan fits around Ivan's other modules.
  - Touches `Project/Gantt Chart.md`.
- [x] 19. **[Claude] (c) Data-management plan.**
  - The Drive folder structure (weekly meeting logs, requirements, testing documentation, reports, ethics material, literature).
  - Where each class of data lives (vault, GitHub, Drive, local `/data/`), plus naming, versioning, backup, access and retention.
  - Touches `Project/Data Management Plan.md`.
- [x] 20. **[Claude] (d) Version-management plan.**
  - Branches, commit convention, tags at milestones, backups, and the repository URLs for the progress-report appendix.
  - Touches `Project/Version Management Plan.md`.
- [x] 21. **[Claude] Stale-notes cleanup** (added 2026-10-02).
  - Project Glossary: the RFC 9421 row wrongly says signatures prevent replay; the Mandate row uses AP2 v0.1 terms (intent/cart/payment).
  - Market and Positioning: AP2 v0.1 terms; add Lan et al. (2026) as prior work for run-time replay defence.
  - ADR-111: v0.1 mandate wording (terms only, not the decision).
  - Write-up Plan: the Ch 2 row's content cell is stale and cites Market and Positioning.
  - Touches `Project/Project Glossary.md`, `Project/Market and Positioning.md`, `Project/Decisions (ADRs).md` (ADR-111 text only), `Dissertation/Write-up Plan.md`.

## Open questions
- [x] What format and length does the department want for the proposal? This affects subtask 5 and the export. → **Answered (handbook):** 1,000–1,500 words, seven-part structure, Arial 11, 1.5 spacing; more than 10% over is not marked.
- [x] Is an ethics form needed for synthetic and sandbox data only, and who approves it? This affects subtask 6. → **Answered (handbook):** form E1 is always required; the supervisor approves it, then it is submitted with the proposal.
- [ ] What are the submission and interim deadlines? Until they're known, the plan stays in week numbers. → **Partly answered:** proposal in S1 week 4, progress report in S1 week 12 (handbook) or week 13 (lecture), final report in S2 week 10. The Moodle dates are still needed (subtask 13).
- [x] How are marks split between build, evaluation and writing? This affects how much proposal space goes to method versus evaluation. → **Answered (handbook):** module 10/90; the proposal rubric is 5 × 20 (introduction, product overview, methodology, project management, written presentation).
- [x] Where should the repo live (GitHub personal or university GitLab), and is it public or private? This affects subtask 7. → **Answered 2026-09-27:** personal GitHub, private — https://github.com/svout/strata.
- [x] Should the Ch. 1–2 drafts be in this spec, or moved to a separate P8 write-up spec? → **Kept in SPEC-001** (8a/8b done 2026-09-27).
