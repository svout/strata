# console

The Strata operator console: the screens a person uses to see every agent payment, understand why it went the way it did, clear the review queue, write spending rules, manage agents and keys, and prove the record hasn't been changed.

Built from the design canvas (boards P1–P10, MoodPlatform, UI kit) and the vault notes `Design/Platform Screens.md` and `Phases/P7 Operator Console and Demo.md`. Same stack (npm) and tokens as `strata-marketing`, so the two read as one product.

## Run it

```bash
cd console
npm install
npm run dev
```

Then open http://localhost:3001.

`npm run lint` (ESLint + `tsc --noEmit`), `npm test` (Vitest), `npm run build`. The root `make lint` and `make test` run `npm ci` and then the first two, and CI calls the same targets; nothing runs `npm run build` yet, so a build-only break isn't caught there. Node is pinned in `.nvmrc` (22), which CI reads; locally it holds only if your version manager uses it. The package manager is npm (Technology Choices, amended 2026-11-19): `package-lock.json` is the lock the tests pass against, so add dependencies with `npm install`, never a different tool.

## Sample data, for now

Until the gateway (P2–P5) and the control-plane API exist, the console talks to a **mock gateway that runs in the browser** (`src/api/mock`). It is not a set of static fixtures:

- 30 days of synthetic agent traffic are generated from a fixed seed and **evaluated in order** by a small stand-in for the pipeline (signature → mandate → policy → risk → decision), so limits, reviews and blocks are consistent with each other.
- Every decision, review, mandate edit, key change and member change is appended to a **real SHA-256 hash chain**. "Verify again" recomputes it; the tamper demo on the Ledger page edits an entry in place and verification names it.
- Money moves only on approve, as two postings that sum to zero.
- The eight threat-model scenarios run from the Attack lab and all end as the threat model expects (covered by tests).

Everything resets on reload, or from the user menu → Reset sample data. The sidebar's "Live agent traffic" switch adds a new payment every few seconds.

**This mock is not the dissertation's implementation.** It exists so the console has the same shapes and the same kinds of reasons the real gateway will return. Nothing it produces is a measured result: the timing bars say "sample".

### The honesty rule (hard rule, SPEC-002 18 and P7)

Any public or assessed use of the console (screenshots, demo video, slides, poster, report figures, social posts) must state that **the data is simulated and the decision engine is a prototype**. In the Go gateway only payment creation and the double-entry ledger exist, so an unlabelled demo claims a system that doesn't exist.

The console says so itself: a notice at the top of every screen (`src/components/shell/PrototypeNotice.tsx`, mounted once in `Providers`). Today it reads:

> Prototype. The data shown is simulated. In the gateway, only payment creation and the double-entry ledger exist. Everything else on these screens is simulated, including mandate, signature, policy, risk and audit-chain behaviour.

- The engine sentence is generated from `GATEWAY_IMPLEMENTED` (`src/config/prototype.ts`), a **positive list of what exists**. Everything not on it is declared simulated, so the list can only be wrong by claiming too little. Add an entry in the step that ships a behaviour; never write a list of what's missing, because that is only true if it is complete (the first version of this notice left out signatures and the audit chain).
- "The data shown is simulated" has its own condition, `IS_SAMPLE_DATA` (the mock is in use), because it stops being true at a different time.

`PrototypeNotice.test.tsx` pins today's exact text, the generated forms (including an empty list), the separate data condition, and one notice on two different pages rendered inside `Providers`. The notice is in the page flow, so a screenshot includes it, but it is not a substitute for a caption: captions still say it. It also doesn't reach everywhere: there is no `error.tsx` or `global-error.tsx` yet, and on a phone an open drawer or modal covers it, so a screenshot of either needs the caption.

## How it's put together

```
src/
  api/
    types.ts        the contract (placeholder for the generated client)
    client.ts       ConsoleApi: everything the console asks of the platform
    reasons.ts      reason-code labels (part of the contract)
    index.ts        picks the implementation (mock today)
    mock/           in-browser gateway: catalogue, engine, gateway, scenarios, sha256
  app/
    sign-in/        P1
    onboarding/     P2
    (console)/      the signed-in shell
      overview/     P3
      decisions/    P4 (+ detail drawer, ?id=)
      review/       P5
      mandates/     P6, and [id] = P7 editor (new → /mandates/new)
      agents/       P8
      ledger/       P9
      settings/     P10 (Workspace · Members · API keys · Webhooks)
      disputes/[id] dispute reconstruction (P7 phase deliverable)
      attack-lab/   attack replay (P7 phase deliverable)
  components/       brand (logo, chips), shell, ui kit (incl. Allowed), decision (layer trace, drawer),
                    charts (MUI X Charts wrappers: OutcomeBars, RankBars, MoneyBars, CompareBars, Spark)
  lib/              format, session + roles, permissions, hotkeys, data hook
```

Screens only see the `ConsoleApi` interface. When the control plane publishes its OpenAPI spec, generate the client, replace `src/api/types.ts` with the generated types, add an HTTP implementation of `ConsoleApi`, and switch it on with `NEXT_PUBLIC_STRATA_API=http` in `src/api/index.ts`. The compiler will show every screen that needs to change.

## Rules this follows (console/CLAUDE.md)

- Every decision shows its reason codes and the control that fired; the one-sentence explanation comes from the platform and is rendered verbatim.
- The dispute view renders the reconstruction it is given; nothing is re-derived in the browser.
- Writes are limited to reviews, mandates, agents and keys, and members. Decisions and ledger entries are never edited.
- Roles (P10): Owner, Admin, Member, Viewer. "View as" in the user menu switches role for demos; with real auth the role comes from the server.

## Charts

MUI X Charts (`@mui/x-charts`, MIT community tier), wrapped in `src/components/charts` so every chart shares the Strata rules: signal colours mean outcomes only, approved is cobalt 300 on outcome charts (P3), axis and grid colours follow light/dark mode via CSS, and every chart carries a text label. Clicking a bar on Overview opens Decisions filtered to that hour or day (`?since=&until=`), or to that reason.

## Roles

Four roles for people (agents never have one; their limits come from mandates and keys). The table lives in `src/lib/permissions.ts` and is pinned by `permissions.test.ts`. Controls a role can't use are shown disabled with a tooltip naming who can (`<Allowed>`), not hidden. The full table is in Settings → Members and in the user menu → "What can each role do?".

## Not done yet

Prototype gaps against `console/CLAUDE.md` (its rules are P7's target, so they stay as written):

- It writes reviews, mandates, agents, keys and members, not only policy edits, and only to the in-browser mock.
- `src/api/types.ts` is hand-written, not the generated client.
- No Playwright flow yet (a new dependency, so it needs its own step).

Also:

- Real sign-in (Google / GitHub OAuth) and the generated API client.
- Provider marks on the sign-in buttons (from the Google and GitHub brand kits).
- Webhook editing, review expiry (the P5 board leaves it to the owner), notifications.
