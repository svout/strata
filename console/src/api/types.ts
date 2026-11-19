/**
 * Console API types.
 *
 * PLACEHOLDER until the control-plane OpenAPI spec exists. console/CLAUDE.md
 * says API types come from the generated client; when the control plane
 * publishes its spec, this file is replaced by the generated output and the
 * compiler shows every screen that needs to change. Until then these types
 * are the contract the mock gateway (src/api/mock) implements.
 *
 * Conventions that the real API must keep:
 * - Money is an integer in minor units (pence) with an ISO currency, as in
 *   the payment core's ledger. Never a float.
 * - Times are epoch milliseconds (UTC).
 * - Every decision carries reason codes and the control that fired, and its
 *   plain-words `explanation` is produced server-side. The console renders
 *   it verbatim and never re-derives it (console/CLAUDE.md).
 */

export type Currency = "GBP";
export type Money = { minor: number; currency: Currency };

export type Outcome = "approve" | "review" | "block";
export type Layer = "mandate" | "risk" | "ledger";
export type Role = "owner" | "admin" | "member" | "viewer";
export type Weekday = 0 | 1 | 2 | 3 | 4 | 5 | 6; // 0 = Monday

export type Category =
  | "flights"
  | "hotels"
  | "rail"
  | "office supplies"
  | "components"
  | "software"
  | "refunds"
  | "events"
  | "advertising"
  | "groceries"
  | "electronics"
  | "gift cards";

/** Machine-readable cause attached to a decision (Glossary: reason code). */
export type ReasonCode =
  | "UNKNOWN_AGENT_KEY"
  | "SIGNATURE_INVALID"
  | "AGENT_PAUSED"
  | "NO_MANDATE"
  | "NONCE_REPLAYED"
  | "HOST_MISMATCH"
  | "CATEGORY_OUT_OF_SCOPE"
  | "MERCHANT_NOT_IN_SCOPE"
  | "CART_EXCEEDS_INTENT"
  | "PER_PAYMENT_CAP"
  | "DAILY_LIMIT"
  | "MONTHLY_LIMIT"
  | "OUTSIDE_TIME_WINDOW"
  | "SPLIT_PURCHASE"
  | "ABOVE_REVIEW_THRESHOLD"
  | "AMOUNT_DEVIATION"
  | "NEW_BENEFICIARY"
  | "CONTEXT_CHANGE"
  | "GRAPH_FANIN"
  | "SCORING_DEGRADED"
  | "WITHIN_MANDATE";

export type Merchant = { id: string; name: string; category: Category; host: string };

export type Agent = {
  id: string; // agt_travel-booker
  description: string;
  owner: string;
  status: "active" | "paused";
  createdAt: number;
  createdBy: string;
  pausedBy?: string;
  lastSeenAt: number | null;
};

export type ApiKey = {
  id: string;
  agentId: string;
  mode: "live" | "test";
  last4: string;
  createdAt: number;
  lastUsedAt: number | null;
  revokedAt: number | null;
};

/** Returned once, when a key is created. The secret is never readable again. */
export type CreatedKey = { key: ApiKey; secret: string };

export type Mandate = {
  id: string;
  name: string;
  agentId: string;
  categories: Category[];
  /** null = any merchant in the chosen categories */
  merchantIds: string[] | null;
  limits: { perPayment: Money; perDay: Money; perMonth: Money };
  days: Weekday[];
  /** null = any time of day; otherwise "HH:MM" in the workspace time zone */
  hours: { from: string; to: string } | null;
  review: { enabled: boolean; above: Money };
  status: "active" | "paused";
  version: number;
  updatedAt: number;
  updatedBy: string;
  usage: { today: Money; month: Money };
};

export type MandateInput = Pick<Mandate, "name" | "agentId" | "categories" | "merchantIds" | "limits" | "days" | "hours" | "review">;

export type MandateVersion = { version: number; at: number; by: string; change: string };

export type CheckStatus = "pass" | "review" | "fail";
/** One control evaluated in the pipeline, in order. */
export type Check = { layer: Layer; control: string; status: CheckStatus; detail: string };

export type RiskContribution = { code: ReasonCode; label: string; value: number };

export type Decision = {
  id: string;
  at: number;
  agentId: string; // may be an unregistered id such as agt_unknown
  agentRegistered: boolean;
  merchantId: string;
  checkoutHost: string;
  amount: Money;
  outcome: Outcome;
  /** All reason codes, most important first. */
  reasonCodes: ReasonCode[];
  /** The control that decided the outcome, e.g. "Per-payment cap". */
  controlFired: string;
  /** One sentence, plain words, produced by the gateway. Render verbatim. */
  explanation: string;
  layers: {
    mandate: { status: CheckStatus; summary: string };
    risk: { status: CheckStatus | "skipped"; summary: string; score: number | null; model: string | null; degraded: boolean; contributions: RiskContribution[] };
    ledger: { seq: number; hash: string };
  };
  checks: Check[];
  mandateId: string | null;
  mandateVersion: number | null;
  timingsMs: { signature: number; mandate: number; policy: number; risk: number; audit: number };
  review: null | { status: "pending" } | { status: "approved" | "declined"; by: string; at: number; reason?: string; seq: number };
  source: "agent" | "attack-lab" | "onboarding";
  scenario?: number;
  /** Double-entry postings; empty when no money moved. Sums to zero. */
  postings: { account: string; direction: "debit" | "credit"; amount: Money }[];
  chain: null | {
    intent: { id: string; signedBy: string; scope: string };
    cart: { id: string; items: { name: string; amount: Money }[] };
    payment: { id: string; rail: string; instrument: string; nonce: string; replayed: boolean };
  };
};

export type DecisionFilter = {
  outcome?: "approved" | "review" | "blocked" | "declined";
  agentId?: string;
  reason?: ReasonCode;
  q?: string;
  since?: number;
  /** Exclusive end of the window, for drilling into one bar of a chart. */
  until?: number;
};

export type LedgerEventType =
  | "decision.approved"
  | "decision.review"
  | "decision.blocked"
  | "review.approved"
  | "review.declined"
  | "mandate.created"
  | "mandate.updated"
  | "mandate.paused"
  | "mandate.resumed"
  | "agent.registered"
  | "agent.paused"
  | "agent.resumed"
  | "key.created"
  | "key.revoked"
  | "member.invited"
  | "member.role_changed"
  | "member.removed";

export type LedgerEntry = {
  seq: number;
  at: number;
  event: LedgerEventType;
  subject: string;
  actor: string; // "strata" for the gateway, else a person
  ref: { kind: "decision" | "mandate" | "agent" | "key" | "member"; id: string };
  /** Canonical JSON of the record; the digest is SHA-256 over this string. */
  payload: string;
  digest: string;
  prevHash: string;
  hash: string;
};

export type ChainStatus = { ok: true; entries: number; checkedAt: number } | { ok: false; entries: number; checkedAt: number; firstBroken: number };

export type Member = { id: string; name: string; email: string; role: Role; lastActiveAt: number | null; initials: string };
export type Invite = { id: string; email: string; role: Exclude<Role, "owner">; sentAt: number };
export type Workspace = { id: string; name: string; timeZone: string };
export type Webhook = { id: string; url: string; events: string[]; enabled: boolean };

export type OverviewRange = "today" | "7d" | "30d";
export type Overview = {
  range: OverviewRange;
  checked: number;
  approved: number;
  waiting: number;
  blocked: number;
  paidOut: Money;
  /** One bucket per hour (today) or per day (7d, 30d), with its start time for drill-down. */
  byBucket: { label: string; start: number; end: number; approve: number; review: number; block: number }[];
  /** Last 7 days, oldest first: the sparklines on the tiles. */
  trend: { day: string; checked: number; approved: number; waiting: number; blocked: number }[];
  topControls: { code: ReasonCode; label: string; count: number }[];
  recentlyBlocked: Decision[];
};

/** One day of one agent's activity (Agents page). */
export type AgentDay = { day: string; start: number; spent: Money; approve: number; review: number; block: number };

export type DisputeStep = { at: number; title: string; detail: string };
export type Dispute = { decision: Decision; steps: DisputeStep[]; entry: LedgerEntry; chain: ChainStatus };

export type ReplayResult = {
  days: number;
  requests: number;
  draft: Record<Outcome, number>;
  current: Record<Outcome, number> | null;
  changed: { at: number; merchant: string; amount: Money; before: Outcome; after: Outcome }[];
};

export type Scenario = {
  n: number;
  title: string;
  attacker: string;
  control: string;
  expected: Outcome;
};

export type DeclineReason = "wrong merchant" | "too expensive" | "not needed" | "other";
