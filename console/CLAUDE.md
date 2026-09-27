# console — local rules (Next.js + TypeScript)

- Read-only on decisions and audit; the only writes are policy edits through the control-plane API.
- API types come from the generated client in `src/api`; no hand-written types, no `any`.
- Every decision shown must display its reason codes and the control that fired. A screen that shows an outcome without the why is incomplete.
- The dispute view renders the reconstruction from the audit chain verbatim — don't re-derive anything in the browser.
- Keep it legible to a non-engineer: that is the point of the screen in the dissertation.
- Tests: Vitest for components; one Playwright flow for the demo path.
