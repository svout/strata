import { moneyShort, money } from "@/lib/format";
import { REASON_LABEL } from "../reasons";
import type { Agent, Check, CheckStatus, Decision, Mandate, Merchant, Money, Outcome, ReasonCode, RiskContribution } from "../types";

/**
 * A stand-in for the gateway's decision pipeline, so the console has
 * realistic, internally consistent data before P2–P5 exist.
 *
 * Order matches Architecture → Request pipeline:
 *   1 signature → 2 mandate → 3 policy → 4 risk → 5 decision + audit
 * and the failure rule from ADR-106: anything in 1–2 that can't be
 * verified fails closed (block); a scoring failure degrades to rules and is
 * marked degraded, never silently allowed.
 *
 * This file is NOT the dissertation's implementation. It exists to give
 * the console the same shapes and the same kinds of reasons the real
 * gateway will return.
 */

export type Flags = {
  badSignature?: boolean;
  replay?: boolean;
  cartExtra?: { name: string; amount: Money };
  contextChange?: boolean;
  newBeneficiary?: boolean;
  fanIn?: number;
  scorerDown?: boolean;
};

export type PaymentRequest = {
  id: string;
  at: number;
  agentId: string;
  merchantId: string;
  checkoutHost: string;
  amount: Money;
  flags: Flags;
  source: Decision["source"];
  scenario?: number;
};

export type EngineContext = {
  agent: Agent | null;
  hasActiveKey: boolean;
  /** The agent's mandates, active and paused. */
  mandates: Mandate[];
  merchant: Merchant & { typical: [number, number] };
  /** Approved spend under a mandate on the request's day and month, in pence. */
  usage: (mandateId: string) => { day: number; month: number };
  /** Same agent and merchant in the 10 minutes before this request. */
  recent: { count: number; sumMinor: number };
  rand: () => number;
};

export { REASON_LABEL } from "../reasons";

const DAY_NAMES = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
/** Monday = 0, to match Weekday. */
export const weekdayOf = (t: number) => (new Date(t).getDay() + 6) % 7;
const minutesOf = (t: number) => {
  const d = new Date(t);
  return d.getHours() * 60 + d.getMinutes();
};
const toMinutes = (hhmm: string) => {
  const [h, m] = hhmm.split(":").map(Number);
  return h * 60 + m;
};

export function windowText(m: Pick<Mandate, "days" | "hours">): string {
  const days =
    m.days.length === 7 ? "any day" : m.days.length === 5 && m.days.every((d) => d < 5) ? "weekdays" : m.days.map((d) => DAY_NAMES[d]).join(", ");
  return m.hours ? `${days}, ${m.hours.from}–${m.hours.to}` : days;
}

export function inWindow(m: Pick<Mandate, "days" | "hours">, at: number): boolean {
  if (!m.days.includes(weekdayOf(at) as Mandate["days"][number])) return false;
  if (!m.hours) return true;
  const t = minutesOf(at);
  return t >= toMinutes(m.hours.from) && t < toMinutes(m.hours.to);
}

const round2 = (n: number) => Math.round(n * 100) / 100;
const RISK_REVIEW_AT = 0.65;
export const MODEL_VERSION = "lgbm-v0.3 (sample)";

export function evaluate(req: PaymentRequest, ctx: EngineContext): Omit<Decision, "layers"> & { layers: Omit<Decision["layers"], "ledger"> } {
  const { agent, merchant, rand } = ctx;
  const checks: Check[] = [];
  type Fired = { code: ReasonCode; control: string; sentence: string };
  // Asserted, not annotated: `fail` assigns it inside a closure, which
  // TypeScript's narrowing can't see.
  let block = null as Fired | null;
  const reviews: Fired[] = [];
  const pass = (layer: Check["layer"], control: string, detail: string) => checks.push({ layer, control, status: "pass", detail });
  const fail = (layer: Check["layer"], control: string, detail: string, code: ReasonCode, sentence: string) => {
    checks.push({ layer, control, status: "fail", detail });
    block = { code, control, sentence };
  };
  const amountText = money(req.amount);
  const name = req.agentId;

  // ── 1. Transport: whose key signed this, and does the signature hold? ──
  let mandate: Mandate | null = null;
  if (!agent) {
    fail("mandate", "Agent registry", "Signing key isn't registered in this workspace", "UNKNOWN_AGENT_KEY", "No registered agent owns the key that signed this request.");
  } else if (!ctx.hasActiveKey) {
    fail("mandate", "Agent registry", `${name} has no active key`, "UNKNOWN_AGENT_KEY", `The key that signed this request has been revoked, so ${name} can't be verified.`);
  } else if (req.flags.badSignature) {
    fail("mandate", "Request signature", "Signature doesn't match the request body", "SIGNATURE_INVALID", "The request signature doesn't match its contents, so it may have been changed in transit.");
  } else {
    pass("mandate", "Request signature", `Signed by ${name}, verified (Ed25519, RFC 9421)`);
    if (agent.status === "paused") {
      fail("mandate", "Agent status", `${name} is paused${agent.pausedBy ? ` by ${agent.pausedBy}` : ""}`, "AGENT_PAUSED", `${name} is paused, so it can't spend until someone resumes it.`);
    }
  }

  // ── 2. Mandate chain: is there a mandate that covers this purchase? ──
  if (!block) {
    const active = ctx.mandates.filter((m) => m.status === "active");
    const covering = active.find((m) => m.categories.includes(merchant.category));
    mandate = covering ?? active[0] ?? null;
    if (!mandate) {
      fail("mandate", "Mandate", `${name} has no active mandate`, "NO_MANDATE", `${name} has no active mandate, so it isn't allowed to spend.`);
    } else if (req.flags.replay) {
      fail("mandate", "Single-use nonce", "This payment mandate's nonce was already used", "NONCE_REPLAYED", "This mandate was already used once. Each mandate can pay only once.");
    } else if (req.checkoutHost !== merchant.host) {
      fail("mandate", "Merchant identity", `Checkout at ${req.checkoutHost}, mandate names ${merchant.host}`, "HOST_MISMATCH", `The checkout at ${req.checkoutHost} isn't the ${merchant.name} site the mandate names.`);
    } else if (!covering) {
      fail("mandate", "Category scope", `"${merchant.category}" isn't in "${mandate.name}"`, "CATEGORY_OUT_OF_SCOPE", `${cap(merchant.category)} isn't something ${name} is allowed to buy.`);
    } else if (mandate.merchantIds && !mandate.merchantIds.includes(merchant.id)) {
      fail("mandate", "Merchant list", `${merchant.name} isn't on the list in "${mandate.name}"`, "MERCHANT_NOT_IN_SCOPE", `${merchant.name} isn't one of the merchants "${mandate.name}" allows.`);
    } else if (req.flags.cartExtra) {
      fail("mandate", "Cart ⊆ intent", `Cart adds "${req.flags.cartExtra.name}"`, "CART_EXCEEDS_INTENT", `The cart includes ${req.flags.cartExtra.name.toLowerCase()} that the person never approved.`);
    } else {
      pass("mandate", "Mandate", `"${mandate.name}" v${mandate.version} covers ${merchant.category}`);
      pass("mandate", "Single-use nonce", "Nonce is fresh");
      pass("mandate", "Merchant identity", `Checkout host matches ${merchant.name}`);
    }
  }

  // ── 3. Policy: caps, limits, time window, split purchases ──
  if (!block && mandate) {
    const m: Mandate = mandate;
    const used = ctx.usage(m.id);
    if (req.amount.minor > m.limits.perPayment.minor) {
      fail("mandate", "Per-payment cap", `${amountText} > ${moneyShort(m.limits.perPayment)}`, "PER_PAYMENT_CAP", `${amountText} is over the ${moneyShort(m.limits.perPayment)} cap for a single payment in "${m.name}".`);
    } else if (!inWindow(m, req.at)) {
      fail("mandate", "Time window", `Allowed ${windowText(m)}`, "OUTSIDE_TIME_WINDOW", `"${m.name}" only allows payments on ${windowText(m)}.`);
    } else if (used.day + req.amount.minor > m.limits.perDay.minor) {
      fail("mandate", "Daily limit", `${moneyShort({ minor: used.day + req.amount.minor, currency: "GBP" })} of ${moneyShort(m.limits.perDay)} today`, "DAILY_LIMIT", `This would take today's spend in "${m.name}" past its ${moneyShort(m.limits.perDay)} daily limit.`);
    } else if (used.month + req.amount.minor > m.limits.perMonth.minor) {
      fail("mandate", "Monthly limit", `${moneyShort({ minor: used.month + req.amount.minor, currency: "GBP" })} of ${moneyShort(m.limits.perMonth)} this month`, "MONTHLY_LIMIT", `This would take this month's spend in "${m.name}" past its ${moneyShort(m.limits.perMonth)} limit.`);
    } else {
      pass("mandate", "Per-payment cap", `${amountText} ≤ ${moneyShort(m.limits.perPayment)}`);
      pass("mandate", "Time window", `Allowed ${windowText(m)}`);
      pass("mandate", "Daily limit", `${moneyShort({ minor: used.day + req.amount.minor, currency: "GBP" })} of ${moneyShort(m.limits.perDay)} today`);
      const sum = ctx.recent.sumMinor + req.amount.minor;
      if (ctx.recent.count >= 2 && sum > m.limits.perPayment.minor) {
        const n = ctx.recent.count + 1;
        checks.push({ layer: "mandate", control: "Split-purchase check", status: "review", detail: `${n} payments in 10 minutes, ${moneyShort({ minor: sum, currency: "GBP" })} together` });
        reviews.push({ code: "SPLIT_PURCHASE", control: "Split-purchase check", sentence: `${n} payments to ${merchant.name} in 10 minutes add up to ${moneyShort({ minor: sum, currency: "GBP" })}, over the ${moneyShort(m.limits.perPayment)} cap. It looks like one purchase split up.` });
      }
      if (m.review.enabled && req.amount.minor > m.review.above.minor) {
        checks.push({ layer: "mandate", control: "Review threshold", status: "review", detail: `Over ${moneyShort(m.review.above)}` });
        reviews.push({ code: "ABOVE_REVIEW_THRESHOLD", control: "Review threshold", sentence: `It's over ${moneyShort(m.review.above)}, so "${m.name}" asks a person to confirm.` });
      }
    }
  }

  // ── 4. Risk: score with reasons; degrade to rules if the model is down ──
  let score: number | null = null;
  let contributions: RiskContribution[] = [];
  const degraded = !block && !!req.flags.scorerDown;
  if (!block && !degraded) {
    const typical = ((merchant.typical[0] + merchant.typical[1]) / 2) * 100;
    const dev = req.amount.minor / typical;
    const raw: [ReasonCode, number][] = [
      ["AMOUNT_DEVIATION", dev > 2.2 ? Math.min(0.62, (dev - 1) * 0.17) : Math.max(0.01, (dev - 1) * 0.05 + 0.02)],
      ["CONTEXT_CHANGE", req.flags.contextChange ? 0.64 : rand() * 0.04],
      ["NEW_BENEFICIARY", req.flags.newBeneficiary ? 0.2 : rand() * 0.03],
      ["GRAPH_FANIN", req.flags.fanIn ? 0.46 : rand() * 0.02],
    ];
    contributions = raw.map(([code, value]) => ({ code, label: REASON_LABEL[code], value: round2(value) })).sort((a, b) => b.value - a.value);
    score = round2(Math.min(0.99, 0.03 + rand() * 0.04 + contributions.reduce((s, c) => s + c.value, 0)));
    if (score >= RISK_REVIEW_AT) {
      const top = contributions[0];
      const typicalText = moneyShort({ minor: Math.round(typical), currency: "GBP" });
      const sentence: Record<string, string> = {
        AMOUNT_DEVIATION: `That's about ${round1(dev)}× the ${typicalText} ${name} usually pays ${merchant.name}.`,
        CONTEXT_CHANGE: `${name}'s key was used from a network and device it has never used before.`,
        NEW_BENEFICIARY: "This is the first payment to this beneficiary.",
        GRAPH_FANIN: `${req.flags.fanIn ?? 5} different agents paid this new beneficiary in the last hour.`,
      };
      reviews.push({ code: top.code, control: "Risk score", sentence: sentence[top.code] });
    }
  }

  // ── 5. Decision ──
  const outcome: Outcome = block ? "block" : reviews.length ? "review" : "approve";
  const fired = block ?? reviews[0] ?? null;
  const reasonCodes: ReasonCode[] = block ? [block.code] : reviews.length ? reviews.map((r) => r.code) : ["WITHIN_MANDATE"];
  if (degraded) reasonCodes.push("SCORING_DEGRADED");

  const mandateStatus: CheckStatus = block ? "fail" : reviews.some((r) => r.control !== "Risk score") ? "review" : "pass";
  const riskStatus: Decision["layers"]["risk"]["status"] = block ? "skipped" : reviews.some((r) => r.control === "Risk score") ? "review" : "pass";

  let explanation: string;
  if (outcome === "approve") {
    explanation = mandate
      ? `Approved. Inside "${mandate.name}" (${mandate.categories.join(", ")} up to ${moneyShort(mandate.limits.perPayment)}) and it looks like ${name}'s normal spending.`
      : "Approved.";
  } else if (outcome === "block") {
    explanation = `Blocked. ${fired?.sentence ?? ""}`;
  } else {
    explanation = `Waiting for a person. ${reviews.map((r) => r.sentence).join(" ")}`;
  }

  const mandateSummary = block
    ? (checks[checks.length - 1]?.detail ?? "Failed")
    : reviews.some((r) => r.control !== "Risk score")
      ? reviews.filter((r) => r.control !== "Risk score").map((r) => r.control).join(" · ")
      : `"${mandate?.name}" allows this`;
  const riskSummary = block
    ? "Not reached: stopped before scoring"
    : degraded
      ? "Model unavailable: rules only, marked degraded"
      : riskStatus === "review"
        ? `${contributions[0].label} · score ${score?.toFixed(2)}`
        : `Looks normal · score ${score?.toFixed(2)}`;

  const timings = {
    signature: round2(0.3 + rand() * 1.2),
    mandate: agent ? round2(0.6 + rand() * 1.8) : 0,
    policy: !block || checks.some((c) => c.control === "Per-payment cap" || c.control === "Time window" || c.control.includes("limit")) ? round2(0.5 + rand() * 2.4) : 0,
    risk: block ? 0 : degraded ? round2(0.2 + rand() * 0.4) : round2(1.8 + rand() * 4.8),
    audit: round2(1.1 + rand() * 2.7),
  };

  const items = [{ name: itemName(merchant.category), amount: req.flags.cartExtra ? { minor: req.amount.minor - req.flags.cartExtra.amount.minor, currency: "GBP" as const } : req.amount }];
  if (req.flags.cartExtra) items.push({ name: req.flags.cartExtra.name, amount: req.flags.cartExtra.amount });

  return {
    id: req.id,
    at: req.at,
    agentId: req.agentId,
    agentRegistered: !!agent,
    merchantId: merchant.id,
    checkoutHost: req.checkoutHost,
    amount: req.amount,
    outcome,
    reasonCodes,
    controlFired: fired ? fired.control : "None: every check passed",
    explanation,
    layers: {
      mandate: { status: mandateStatus, summary: mandateSummary },
      risk: { status: riskStatus, summary: riskSummary, score, model: block ? null : degraded ? "rules-only" : MODEL_VERSION, degraded, contributions },
    },
    checks,
    mandateId: mandate?.id ?? null,
    mandateVersion: mandate?.version ?? null,
    timingsMs: timings,
    review: outcome === "review" ? { status: "pending" } : null,
    source: req.source,
    scenario: req.scenario,
    postings: [],
    chain: mandate && agent
      ? {
          intent: {
            id: `int_${req.id.slice(4, 12)}`,
            signedBy: agent.owner,
            scope: `${mandate.categories.join(", ")} · ${mandate.merchantIds ? `${mandate.merchantIds.length} named merchants` : "any merchant in these categories"} · ≤ ${moneyShort(mandate.limits.perPayment)} each`,
          },
          cart: { id: `crt_${req.id.slice(4, 12)}`, items },
          payment: { id: `pmd_${req.id.slice(4, 12)}`, rail: "Stripe test mode", instrument: "Test card ending 4242", nonce: req.id.slice(-10), replayed: !!req.flags.replay },
        }
      : null,
  };
}

const round1 = (n: number) => Math.round(n * 10) / 10;
const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

function itemName(c: string): string {
  const names: Record<string, string> = {
    flights: "Economy return",
    hotels: "1 night, standard double",
    rail: "Off-peak return",
    "office supplies": "Paper and toner",
    components: "Sensor modules ×40",
    software: "Team seats, monthly",
    events: "Venue hire, half day",
    refunds: "Order refund",
    advertising: "Ad account top-up",
    electronics: "Laptop",
    groceries: "Weekly basket",
    "gift cards": "Digital gift card",
  };
  return names[c] ?? "Order";
}

/** Double-entry postings for money that moved. Always sums to zero. */
export function postingsFor(d: Pick<Decision, "agentId" | "merchantId" | "amount">, owner: string): Decision["postings"] {
  const debit = `acct_${owner.toLowerCase().replace(/[^a-z]+/g, "_")}`;
  const credit = `acct_${d.merchantId.replace(/^mer_/, "merchant_")}`;
  return [
    { account: debit, direction: "debit", amount: { minor: -d.amount.minor, currency: d.amount.currency } },
    { account: credit, direction: "credit", amount: d.amount },
  ];
}
