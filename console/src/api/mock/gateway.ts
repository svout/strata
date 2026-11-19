import { addMoney, gbp, money, moneyShort, zero } from "@/lib/format";
import { mandateSentence } from "@/lib/mandateSentence";
import type { ConsoleApi } from "../client";
import type {
  Agent,
  AgentDay,
  ApiKey,
  ChainStatus,
  CreatedKey,
  Decision,
  DecisionFilter,
  DeclineReason,
  Dispute,
  DisputeStep,
  Invite,
  LedgerEntry,
  LedgerEventType,
  Mandate,
  MandateInput,
  MandateVersion,
  Member,
  Merchant,
  Money,
  Outcome,
  Overview,
  OverviewRange,
  ReplayResult,
  Role,
  Scenario,
  Webhook,
  Workspace,
} from "../types";
import { AGENTS, MANDATES, MERCHANTS, PEOPLE, UNKNOWN_AGENT_PER_DAY } from "./catalogue";
import { REASON_LABEL, evaluate, postingsFor, type EngineContext, type Flags, type PaymentRequest } from "./engine";
import { GENESIS, canonicalJson, chainHash, sha256 } from "./sha256";
import { SCENARIOS, scenarioRequests } from "./scenarios";

const MIN = 60_000;
const HOUR = 60 * MIN;
const DAY = 24 * HOUR;
const HISTORY_DAYS = 30;
const PENDING_FOR = 3 * HOUR; // reviews younger than this are still waiting

type MerchantRow = Merchant & { typical: [number, number] };
type PendingEvent = { at: number; event: LedgerEventType; subject: string; actor: string; ref: LedgerEntry["ref"]; data: Record<string, unknown>; decisionId?: string };

/** Small seeded PRNG (mulberry32): the same seed always builds the same workspace. */
function rng(seed: number) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const ALPHABET = "abcdefghjkmnpqrstuvwxyz23456789";
const startOfDay = (t: number) => {
  const d = new Date(t);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
};
const startOfMonth = (t: number) => {
  const d = new Date(t);
  d.setHours(0, 0, 0, 0);
  d.setDate(1);
  return d.getTime();
};
const clone = <T>(v: T): T => structuredClone(v);

/**
 * In-memory implementation of ConsoleApi. Everything lives in the browser
 * tab and resets on reload (or with reset()). Writes go through the same
 * rules the real platform will enforce: decisions and ledger entries are
 * append-only, every change is a new chained ledger entry, money only moves
 * on approve, and postings always sum to zero.
 */
export class MockGateway implements ConsoleApi {
  private rand: () => number;
  private merchantsList: MerchantRow[] = MERCHANTS.map((m) => ({ ...m }));
  private agentsList: Agent[] = [];
  private keysList: ApiKey[] = [];
  private mandatesList: Mandate[] = [];
  private history = new Map<string, MandateVersion[]>();
  private decisionsList: Decision[] = []; // oldest first
  private requests = new Map<string, PaymentRequest>();
  private entries: LedgerEntry[] = [];
  private verification: ChainStatus = { ok: true, entries: 0, checkedAt: 0 };
  private tampered: { seq: number; payload: string } | null = null;
  private membersList: Member[] = [];
  private invitesList: Invite[] = [];
  private ws: Workspace = { id: "ws_demo", name: "Strata Demo", timeZone: "Europe/London" };
  private listeners = new Set<() => void>();
  /** Time-dependent state while seeding (paused-at times). */
  private pausedAt = new Map<string, number>();

  constructor(private seed = 20261113, private now = () => Date.now()) {
    this.rand = rng(seed);
    this.build();
  }

  // ───────────────────────────── seeding ─────────────────────────────

  private id(prefix: string, n = 12) {
    let s = "";
    for (let i = 0; i < n; i++) s += ALPHABET[Math.floor(this.rand() * ALPHABET.length)];
    return `${prefix}_${s}`;
  }

  private build() {
    const now = this.now();
    this.rand = rng(this.seed);
    this.decisionsList = [];
    this.requests.clear();
    this.entries = [];
    this.history.clear();
    this.tampered = null;
    this.pausedAt.clear();
    const events: PendingEvent[] = [];

    // People
    this.membersList = PEOPLE.map((p, i) => ({ ...p, lastActiveAt: i === 0 ? now : now - (i * 7 + 3) * HOUR }));
    this.invitesList = [{ id: "inv_lena", email: "lena@finance-partner.example", role: "viewer", sentAt: now - 2 * DAY - 3 * HOUR }];
    events.push({ at: now - 2 * DAY - 3 * HOUR, event: "member.invited", subject: "lena@finance-partner.example as viewer", actor: "Ivan Kovalchuk", ref: { kind: "member", id: "inv_lena" }, data: { role: "viewer" } });

    // Agents and keys
    this.agentsList = AGENTS.map((a) => {
      const createdAt = now - a.createdDaysAgo * DAY;
      events.push({ at: createdAt, event: "agent.registered", subject: a.id, actor: a.createdBy, ref: { kind: "agent", id: a.id }, data: { owner: a.owner } });
      return { id: a.id, description: a.description, owner: a.owner, status: "active", createdAt, createdBy: a.createdBy, lastSeenAt: null };
    });
    this.keysList = [];
    for (const a of this.agentsList) {
      const live: ApiKey = { id: this.id("key", 8), agentId: a.id, mode: "live", last4: this.id("x", 4).slice(2), createdAt: a.createdAt + 5 * MIN, lastUsedAt: null, revokedAt: null };
      const test: ApiKey = { id: this.id("key", 8), agentId: a.id, mode: "test", last4: this.id("x", 4).slice(2), createdAt: a.createdAt + 2 * MIN, lastUsedAt: a.createdAt + DAY, revokedAt: null };
      this.keysList.push(live, test);
      for (const k of [test, live]) events.push({ at: k.createdAt, event: "key.created", subject: `sk_${k.mode}_••••${k.last4} · ${a.id}`, actor: a.createdBy, ref: { kind: "key", id: k.id }, data: { agent: a.id, mode: k.mode } });
    }
    const marketing = this.agentsList.find((a) => a.id === "agt_marketing")!;
    const pausedAgentAt = now - 4 * DAY - 2 * HOUR;
    this.pausedAt.set("agt_marketing", pausedAgentAt);
    events.push({ at: pausedAgentAt, event: "agent.paused", subject: "agt_marketing", actor: "Maya Robinson", ref: { kind: "agent", id: "agt_marketing" }, data: {} });

    // Mandates and their version history
    this.mandatesList = MANDATES.map((s) => {
      const versions: MandateVersion[] = s.history.map((h, i) => ({ version: i + 1, at: now - h.daysAgo * DAY - (i + 1) * 37 * MIN, by: h.by, change: h.change }));
      this.history.set(s.id, [...versions].reverse());
      versions.forEach((v, i) =>
        events.push({
          at: v.at,
          event: i === 0 ? "mandate.created" : v.change === "Paused" ? "mandate.paused" : "mandate.updated",
          subject: `${s.name} · ${v.change}`,
          actor: v.by,
          ref: { kind: "mandate", id: s.id },
          data: { version: v.version },
        }),
      );
      if (s.paused) this.pausedAt.set(s.id, versions[versions.length - 1].at);
      const last = versions[versions.length - 1];
      return {
        id: s.id,
        name: s.name,
        agentId: s.agentId,
        categories: s.categories,
        merchantIds: s.merchantIds,
        limits: { perPayment: gbp(s.perPayment), perDay: gbp(s.perDay), perMonth: gbp(s.perMonth) },
        days: s.days,
        hours: s.hours,
        review: { enabled: s.reviewAbove != null, above: gbp(s.reviewAbove ?? s.perPayment) },
        status: "active",
        version: versions.length,
        updatedAt: last.at,
        updatedBy: last.by,
        usage: { today: zero, month: zero },
      } satisfies Mandate;
    });

    // Thirty days of agent traffic, evaluated in time order
    const reqs = this.syntheticTraffic(now);
    for (const req of reqs) {
      const d = this.decide(req, true);
      events.push(this.decisionEvent(d));
      if (d.outcome === "review" && now - d.at > PENDING_FOR) {
        const ok = this.rand() < 0.74;
        const by = ["Maya Robinson", "Tom Adeyemi", "Ivan Kovalchuk"][Math.floor(this.rand() * 3)];
        const at = d.at + Math.floor(3 + this.rand() * 40) * MIN;
        d.review = { status: ok ? "approved" : "declined", by, at, reason: ok ? undefined : "not needed", seq: 0 };
        if (ok) d.postings = postingsFor(d, this.ownerOf(d.agentId));
        events.push(this.reviewEvent(d, ok, by, at, ok ? undefined : "not needed"));
      }
    }

    // Final state, then write the ledger in time order
    marketing.status = "paused";
    marketing.pausedBy = "Maya Robinson";
    this.mandatesList.find((m) => m.id === "mnd_ads")!.status = "paused";
    this.pausedAt.clear();
    events.sort((a, b) => a.at - b.at);
    for (const e of events) this.append(e);
    this.verification = { ok: true, entries: this.entries.length, checkedAt: now - 6 * MIN };
    for (const a of this.agentsList) {
      const last = [...this.decisionsList].reverse().find((d) => d.agentId === a.id);
      a.lastSeenAt = last?.at ?? null;
      const live = this.keysList.find((k) => k.agentId === a.id && k.mode === "live");
      if (live) live.lastUsedAt = last?.at ?? null;
    }
  }

  private syntheticTraffic(now: number): PaymentRequest[] {
    const out: PaymentRequest[] = [];
    const start = startOfDay(now - HISTORY_DAYS * DAY);
    const merchant = (id: string) => this.merchantsList.find((m) => m.id === id)!;
    const pickTime = (day: number) => {
      // Most traffic in the working day, a little at night
      const busy = this.rand() < 0.88;
      const hour = busy ? 7 + this.rand() * 14 : this.rand() * 24;
      return day + Math.floor(hour * HOUR);
    };
    for (let day = start; day < now; day += DAY) {
      const weekday = (new Date(day).getDay() + 6) % 7;
      for (const a of AGENTS) {
        const created = now - a.createdDaysAgo * DAY;
        let n = a.perDay * (0.6 + this.rand() * 0.8);
        if (a.weekdaysOnly && weekday >= 5) n *= 0.08; // the odd weekend attempt, which gets blocked
        n = Math.floor(n) + (this.rand() < n % 1 ? 1 : 0);
        for (let i = 0; i < n; i++) {
          const at = pickTime(day);
          if (at > now - MIN || at < created) continue;
          let m = merchant(a.merchants[Math.floor(this.rand() * a.merchants.length)]);
          if (m.id === "mer_venue" && this.rand() < 0.85) m = merchant("mer_railnow"); // events are rare
          let pounds = m.typical[0] + this.rand() * (m.typical[1] - m.typical[0]) * 0.85;
          const flags: Flags = {};
          let host = m.host;
          const roll = this.rand();
          if (roll < 0.012) m = merchant(a.id === "agt_travel-booker" ? "mer_lumen" : "mer_giftvault"); // out of scope
          else if (roll < 0.026) pounds = m.typical[1] * (2.4 + this.rand() * 1.6); // spike
          else if (roll < 0.034) flags.replay = true;
          else if (roll < 0.04) flags.contextChange = true;
          else if (roll < 0.043) host = `checkout.${m.host.split(".")[1]}-pay.example`;
          else if (roll < 0.047) flags.scorerDown = true;
          const amount = gbp(Math.round(pounds * 100) / 100);
          out.push({ id: this.id("dec"), at, agentId: a.id, merchantId: m.id, checkoutHost: host, amount, flags, source: "agent" });
        }
      }
      const unknown = Math.floor(UNKNOWN_AGENT_PER_DAY * (0.5 + this.rand())) + (this.rand() < 0.3 ? 1 : 0);
      for (let i = 0; i < unknown; i++) {
        const at = pickTime(day);
        if (at > now - MIN) continue;
        const m = merchant("mer_lumen");
        out.push({ id: this.id("dec"), at, agentId: "agt_unknown", merchantId: m.id, checkoutHost: m.host, amount: gbp(Math.round((600 + this.rand() * 900) * 100) / 100), flags: {}, source: "agent" });
      }
    }
    // Two fixed rows the design boards use: a £412 hotel and a large supplier order
    out.push({ id: "dec_hotel412demo", at: now - 7 * MIN, agentId: "agt_travel-booker", merchantId: "mer_harbour", checkoutHost: merchant("mer_harbour").host, amount: gbp(412), flags: {}, source: "agent" });
    const supplierAt = this.lastWorkingMinute(now - 19 * MIN);
    out.push({ id: "dec_supplier9800", at: supplierAt, agentId: "agt_procure-bot", merchantId: "mer_brightline", checkoutHost: merchant("mer_brightline").host, amount: gbp(9800), flags: {}, source: "agent" });
    // …and two recent holds, so the review queue isn't empty on first load
    out.push({ id: "dec_hotel640demo", at: now - 33 * MIN, agentId: "agt_travel-booker", merchantId: "mer_harbour", checkoutHost: merchant("mer_harbour").host, amount: gbp(640), flags: {}, source: "agent" });
    out.push({ id: "dec_saas780demo", at: now - 71 * MIN, agentId: "agt_saas-renewals", merchantId: "mer_clouddesk", checkoutHost: merchant("mer_clouddesk").host, amount: gbp(780), flags: {}, source: "agent" });
    return out.sort((a, b) => a.at - b.at);
  }

  /** Procurement only runs on weekdays 07:00–20:00; keep the demo row inside that. */
  private lastWorkingMinute(t: number) {
    const d = new Date(t);
    for (let i = 0; i < 8; i++) {
      const wd = (d.getDay() + 6) % 7;
      const mins = d.getHours() * 60 + d.getMinutes();
      if (wd < 5 && mins >= 7 * 60 && mins < 20 * 60) return d.getTime();
      if (wd < 5 && mins >= 20 * 60) {
        d.setHours(19, 41, 0, 0);
        return d.getTime();
      }
      d.setDate(d.getDate() - 1);
      d.setHours(19, 41, 0, 0);
    }
    return t;
  }

  private ownerOf(agentId: string) {
    return this.agentsList.find((a) => a.id === agentId)?.owner ?? "owner";
  }

  /** Approved spend under a mandate on the day and month of `at`, in pence. */
  private usageAt(mandateId: string, at: number) {
    const day0 = startOfDay(at);
    const month0 = startOfMonth(at);
    let day = 0;
    let month = 0;
    for (let i = this.decisionsList.length - 1; i >= 0; i--) {
      const d = this.decisionsList[i];
      if (d.at < month0) break;
      if (d.mandateId !== mandateId || d.at > at || d.postings.length === 0) continue;
      month += d.amount.minor;
      if (d.at >= day0) day += d.amount.minor;
    }
    return { day, month };
  }

  /** Run the engine against the current (or, while seeding, the then-current) state. */
  private decide(req: PaymentRequest, seeding = false): Decision {
    const merchant = this.merchantsList.find((m) => m.id === req.merchantId)!;
    const agentNow = this.agentsList.find((a) => a.id === req.agentId) ?? null;
    let agent = agentNow;
    if (agent && seeding) {
      const p = this.pausedAt.get(agent.id);
      agent = { ...agent, status: p != null && req.at >= p ? "paused" : "active", pausedBy: p != null ? "Maya Robinson" : undefined };
    }
    const mandates = this.mandatesList
      .filter((m) => m.agentId === req.agentId)
      .map((m) => {
        const versions = this.history.get(m.id) ?? [];
        const created = versions[versions.length - 1]?.at ?? 0;
        if (seeding && req.at < created) return null;
        const p = this.pausedAt.get(m.id);
        const status: Mandate["status"] = seeding ? (p != null && req.at >= p ? "paused" : "active") : m.status;
        const version = seeding ? versions.filter((v) => v.at <= req.at).length || 1 : m.version;
        return { ...m, status, version };
      })
      .filter((m): m is Mandate => m !== null);
    const recentList = this.decisionsList.filter(
      (d) => d.agentId === req.agentId && d.merchantId === req.merchantId && d.outcome !== "block" && d.at <= req.at && req.at - d.at < 10 * MIN,
    );
    const ctx: EngineContext = {
      agent,
      hasActiveKey: agent ? this.keysList.some((k) => k.agentId === agent.id && k.mode === "live" && !k.revokedAt) : false,
      mandates,
      merchant,
      usage: (id) => this.usageAt(id, req.at),
      recent: { count: recentList.length, sumMinor: recentList.reduce((s, d) => s + d.amount.minor, 0) },
      rand: rng(parseInt(sha256(req.id).slice(0, 8), 16)),
    };
    const ev = evaluate(req, ctx);
    const d: Decision = { ...ev, layers: { ...ev.layers, ledger: { seq: 0, hash: "" } } };
    if (d.outcome === "approve") d.postings = postingsFor(d, this.ownerOf(d.agentId));
    this.decisionsList.push(d);
    this.requests.set(d.id, req);
    if (agentNow && !seeding) agentNow.lastSeenAt = d.at;
    return d;
  }

  // ───────────────────────────── ledger ─────────────────────────────

  private decisionEvent(d: Decision): PendingEvent {
    const event: LedgerEventType = d.outcome === "approve" ? "decision.approved" : d.outcome === "review" ? "decision.review" : "decision.blocked";
    return {
      at: d.at,
      event,
      subject: `${money(d.amount)} · ${d.agentId}`,
      actor: "strata",
      ref: { kind: "decision", id: d.id },
      decisionId: d.id,
      data: { amount: d.amount, merchant: d.merchantId, outcome: d.outcome, reasons: d.reasonCodes, mandate: d.mandateId ? `${d.mandateId}@v${d.mandateVersion}` : null, score: d.layers.risk.score },
    };
  }

  private reviewEvent(d: Decision, ok: boolean, by: string, at: number, reason?: string): PendingEvent {
    return {
      at,
      event: ok ? "review.approved" : "review.declined",
      subject: `${money(d.amount)} · ${d.agentId}`,
      actor: by,
      ref: { kind: "decision", id: d.id },
      decisionId: d.id,
      data: { decision: d.id, reason: reason ?? null },
    };
  }

  /** Append one entry: digest = SHA-256(payload), hash = SHA-256(prevHash ‖ digest). */
  private append(e: PendingEvent): LedgerEntry {
    const prev = this.entries[this.entries.length - 1];
    const seq = prev ? prev.seq + 1 : 1001;
    const payload = canonicalJson({ seq, at: e.at, event: e.event, subject: e.subject, actor: e.actor, ref: e.ref, data: e.data });
    const digest = sha256(payload);
    const prevHash = prev ? prev.hash : GENESIS;
    const entry: LedgerEntry = { seq, at: e.at, event: e.event, subject: e.subject, actor: e.actor, ref: e.ref, payload, digest, prevHash, hash: chainHash(prevHash, digest) };
    this.entries.push(entry);
    if (e.decisionId) {
      const d = this.decisionsList.find((x) => x.id === e.decisionId);
      if (d && e.event.startsWith("decision.")) d.layers.ledger = { seq, hash: entry.hash };
      if (d && e.event.startsWith("review.") && d.review && d.review.status !== "pending") d.review.seq = seq;
    }
    return entry;
  }

  private changed() {
    for (const l of this.listeners) l();
  }

  private record(e: Omit<PendingEvent, "at"> & { at?: number }) {
    const entry = this.append({ ...e, at: e.at ?? this.now() });
    this.changed();
    return entry;
  }

  // ───────────────────────────── ConsoleApi ─────────────────────────────

  subscribe(listener: () => void) {
    this.listeners.add(listener);
    return () => {
      this.listeners.delete(listener);
    };
  }

  async overview(range: OverviewRange): Promise<Overview> {
    const now = this.now();
    const since = range === "today" ? startOfDay(now) : startOfDay(now - (range === "7d" ? 6 : 29) * DAY);
    const ds = this.decisionsList.filter((d) => d.at >= since);
    const pending = this.decisionsList.filter((d) => d.review?.status === "pending");
    const moved = ds.filter((d) => d.postings.length > 0);
    const buckets: Overview["byBucket"] = [];
    if (range === "today") {
      // All 24 hours, so the chart keeps its shape at any time of day
      for (let h = 0; h < 24; h++) buckets.push({ label: `${String(h).padStart(2, "0")}:00`, start: since + h * HOUR, end: since + (h + 1) * HOUR, approve: 0, review: 0, block: 0 });
      for (const d of ds) bump(buckets[new Date(d.at).getHours()], d.outcome);
    } else {
      const days = range === "7d" ? 7 : 30;
      for (let i = 0; i < days; i++) {
        const start = startOfDay(since + i * DAY + 2 * HOUR); // +2h keeps clock changes on the right day
        buckets.push({ label: new Date(start).toLocaleDateString("en-GB", { day: "numeric", month: "short" }), start, end: startOfDay(start + 26 * HOUR), approve: 0, review: 0, block: 0 });
      }
      for (const d of ds) bump(buckets.find((b) => d.at >= b.start && d.at < b.end), d.outcome);
    }
    const trend: Overview["trend"] = [];
    for (let i = 6; i >= 0; i--) {
      const start = startOfDay(now - i * DAY);
      const end = startOfDay(start + 26 * HOUR);
      const day = this.decisionsList.filter((d) => d.at >= start && d.at < end);
      trend.push({
        day: new Date(start).toLocaleDateString("en-GB", { weekday: "short" }),
        checked: day.length,
        approved: day.filter((d) => d.postings.length > 0).length,
        waiting: day.filter((d) => d.outcome === "review").length,
        blocked: day.filter((d) => d.outcome === "block" || d.review?.status === "declined").length,
      });
    }
    const counts = new Map<string, number>();
    for (const d of ds) if (d.outcome !== "approve") for (const c of d.reasonCodes) if (c !== "SCORING_DEGRADED") counts.set(c, (counts.get(c) ?? 0) + 1);
    return clone({
      range,
      checked: ds.length,
      approved: ds.filter((d) => d.postings.length > 0).length,
      waiting: pending.length,
      blocked: ds.filter((d) => d.outcome === "block" || d.review?.status === "declined").length,
      paidOut: moved.reduce((s, d) => addMoney(s, d.amount), zero),
      byBucket: buckets,
      trend,
      topControls: [...counts.entries()].sort((a, b) => b[1] - a[1]).slice(0, 6).map(([code, count]) => ({ code: code as Decision["reasonCodes"][number], label: REASON_LABEL[code as Decision["reasonCodes"][number]], count })),
      recentlyBlocked: [...this.decisionsList].reverse().filter((d) => d.outcome === "block").slice(0, 5),
    });
  }

  async decisions(f: DecisionFilter): Promise<Decision[]> {
    const q = f.q?.trim().toLowerCase();
    const merchants = new Map(this.merchantsList.map((m) => [m.id, m.name.toLowerCase()]));
    const out = this.decisionsList.filter((d) => {
      if (f.since && d.at < f.since) return false;
      if (f.until && d.at >= f.until) return false;
      if (f.outcome === "approved" && d.postings.length === 0) return false;
      if (f.outcome === "review" && d.review?.status !== "pending") return false;
      if (f.outcome === "blocked" && d.outcome !== "block") return false;
      if (f.outcome === "declined" && d.review?.status !== "declined") return false;
      if (f.agentId && d.agentId !== f.agentId && !(f.agentId === "unregistered" && !d.agentRegistered)) return false;
      if (f.reason && !d.reasonCodes.includes(f.reason)) return false;
      if (q) {
        const hay = `${d.id} ${d.agentId} ${merchants.get(d.merchantId)} ${(d.amount.minor / 100).toFixed(2)} ${d.reasonCodes.join(" ")}`.toLowerCase();
        if (!hay.includes(q.replace(/[£,]/g, ""))) return false;
      }
      return true;
    });
    return clone(out.reverse());
  }

  async decision(id: string) {
    const d = this.decisionsList.find((x) => x.id === id);
    return d ? clone(d) : null;
  }

  async reviewQueue() {
    return clone(this.decisionsList.filter((d) => d.review?.status === "pending").reverse());
  }

  private resolve(id: string, actor: string, ok: boolean, reason?: DeclineReason): Decision {
    const d = this.decisionsList.find((x) => x.id === id);
    if (!d || d.review?.status !== "pending") throw new Error("This payment isn't waiting for review any more.");
    const at = this.now();
    d.review = { status: ok ? "approved" : "declined", by: actor, at, reason, seq: 0 };
    if (ok) d.postings = postingsFor(d, this.ownerOf(d.agentId));
    this.append(this.reviewEvent(d, ok, actor, at, reason));
    this.changed();
    return clone(d);
  }
  async approve(id: string, actor: string) {
    return this.resolve(id, actor, true);
  }
  async decline(id: string, actor: string, reason: DeclineReason) {
    return this.resolve(id, actor, false, reason);
  }

  async dispute(decisionId: string): Promise<Dispute | null> {
    const d = this.decisionsList.find((x) => x.id === decisionId);
    if (!d) return null;
    const entry = this.entries.find((e) => e.seq === d.layers.ledger.seq)!;
    const m = this.mandatesList.find((x) => x.id === d.mandateId);
    const merchant = this.merchantsList.find((x) => x.id === d.merchantId)!;
    const at = (ms: number) => d.at + ms;
    const steps: DisputeStep[] = [
      { at: at(0), title: `${d.agentId} asked to pay ${merchant.name} ${money(d.amount)}`, detail: `For ${d.chain?.cart.items.map((i) => i.name.toLowerCase()).join(" and ") ?? merchant.category}, at checkout ${d.checkoutHost}.` },
    ];
    for (const c of d.checks) {
      if (c.status === "pass" && !["Request signature", "Mandate", "Per-payment cap"].includes(c.control)) continue;
      const verb = c.status === "pass" ? "passed" : c.status === "review" ? "asked for a person" : "failed";
      steps.push({ at: at(2), title: `${c.control} ${verb}`, detail: c.detail + "." });
    }
    if (!d.agentRegistered) steps.push({ at: at(1), title: "Nothing else was checked", detail: "Without a registered key, the request can't be tied to anyone, so it fails closed." });
    if (m && d.mandateVersion) steps.push({ at: at(3), title: `The rule in force was "${m.name}", version ${d.mandateVersion}`, detail: `Today it reads: ${mandateSentence(m, this.merchantsList)}` });
    if (d.layers.risk.status !== "skipped") steps.push({ at: at(9), title: d.layers.risk.degraded ? "Risk model unavailable: rules only" : `Risk score ${d.layers.risk.score?.toFixed(2)}`, detail: d.layers.risk.summary + "." });
    steps.push({ at: at(12), title: `Decision: ${d.outcome === "approve" ? "approve" : d.outcome === "review" ? "hold for a person" : "block"}, written to the ledger`, detail: `Entry #${entry.seq}, chained to #${entry.seq - 1}, before the payment rail was called.` });
    if (d.review && d.review.status !== "pending") steps.push({ at: d.review.at, title: `${d.review.by} ${d.review.status} it`, detail: `Recorded as its own ledger entry #${d.review.seq}${d.review.reason ? `, reason: ${d.review.reason}` : ""}.` });
    else if (d.review) steps.push({ at: this.now(), title: "Still waiting for a person", detail: "Nothing moves until someone approves it in the review queue." });
    steps.push(
      d.postings.length
        ? { at: (d.review && d.review.status === "approved" ? d.review.at : d.at) + 14, title: "Money moved", detail: `${money(d.amount)} out of ${d.postings[0].account}, ${money(d.amount)} into ${d.postings[1].account}. The two postings sum to zero.` }
        : { at: at(14), title: "No money moved", detail: "There are no postings for this payment." },
    );
    return clone({ decision: d, steps, entry, chain: this.computeChain() });
  }

  async merchants() {
    // Strip the mock-only `typical` range: it's not part of the API
    return clone(this.merchantsList.map((m) => ({ id: m.id, name: m.name, category: m.category, host: m.host })));
  }

  async mandates() {
    const now = this.now();
    return clone(this.mandatesList.map((m) => this.withUsage(m, now)));
  }
  private withUsage(m: Mandate, now: number): Mandate {
    const u = this.usageAt(m.id, now);
    return { ...m, usage: { today: { minor: u.day, currency: "GBP" }, month: { minor: u.month, currency: "GBP" } } };
  }
  async mandate(id: string) {
    const m = this.mandatesList.find((x) => x.id === id);
    return m ? clone(this.withUsage(m, this.now())) : null;
  }
  async mandateHistory(id: string) {
    return clone(this.history.get(id) ?? []);
  }

  async saveMandate(id: string | null, input: MandateInput, actor: string): Promise<Mandate> {
    const now = this.now();
    if (id) {
      const m = this.mandatesList.find((x) => x.id === id);
      if (!m) throw new Error("That mandate no longer exists.");
      const change = describeChange(m, input, this.merchantsList) || "No changes";
      Object.assign(m, input, { version: m.version + 1, updatedAt: now, updatedBy: actor });
      this.history.get(id)!.unshift({ version: m.version, at: now, by: actor, change });
      this.record({ event: "mandate.updated", subject: `${m.name} · ${change}`, actor, ref: { kind: "mandate", id }, data: { version: m.version, input } });
      return clone(this.withUsage(m, now));
    }
    const m: Mandate = { ...input, id: this.id("mnd", 6), status: "active", version: 1, updatedAt: now, updatedBy: actor, usage: { today: zero, month: zero } };
    this.mandatesList.push(m);
    this.history.set(m.id, [{ version: 1, at: now, by: actor, change: "Created" }]);
    this.record({ event: "mandate.created", subject: `${m.name} · Created`, actor, ref: { kind: "mandate", id: m.id }, data: { version: 1, input } });
    return clone(m);
  }

  async setMandateStatus(id: string, status: Mandate["status"], actor: string) {
    const m = this.mandatesList.find((x) => x.id === id);
    if (!m) throw new Error("That mandate no longer exists.");
    if (m.status !== status) {
      m.status = status;
      m.version += 1;
      m.updatedAt = this.now();
      m.updatedBy = actor;
      const change = status === "paused" ? "Paused" : "Resumed";
      this.history.get(id)!.unshift({ version: m.version, at: m.updatedAt, by: actor, change });
      this.record({ event: status === "paused" ? "mandate.paused" : "mandate.resumed", subject: `${m.name} · ${change}`, actor, ref: { kind: "mandate", id }, data: { version: m.version } });
    }
    return clone(this.withUsage(m, this.now()));
  }

  async replayMandate(id: string | null, input: MandateInput, days: number): Promise<ReplayResult> {
    const since = this.now() - days * DAY;
    const current = id ? this.mandatesList.find((m) => m.id === id) ?? null : null;
    const cats = new Set([...input.categories, ...(current?.categories ?? [])]);
    const reqs = this.decisionsList
      .filter((d) => d.at >= since && d.agentId === input.agentId && d.source === "agent")
      .map((d) => this.requests.get(d.id)!)
      .filter((r) => cats.has(this.merchantsList.find((m) => m.id === r.merchantId)!.category));
    const draft: Mandate = { ...(current ?? { id: "mnd_draft", status: "active", version: 0, updatedAt: 0, updatedBy: "", usage: { today: zero, month: zero } }), ...input, status: "active" };
    const run = (m: Mandate) => this.simulate(reqs, m);
    const after = run(draft);
    const before = current ? run({ ...current, status: "active" }) : null;
    const tally = (xs: Outcome[]) => xs.reduce((t, o) => ((t[o] += 1), t), { approve: 0, review: 0, block: 0 } as Record<Outcome, number>);
    const changed = before
      ? reqs
          .map((r, i) => ({ at: r.at, merchant: this.merchantsList.find((m) => m.id === r.merchantId)!.name, amount: r.amount, before: before[i], after: after[i] }))
          .filter((c) => c.before !== c.after)
          .reverse()
      : [];
    return { days, requests: reqs.length, draft: tally(after), current: before ? tally(before) : null, changed };
  }

  /** Re-run requests against one mandate, tracking its own day and month totals. */
  private simulate(reqs: PaymentRequest[], m: Mandate): Outcome[] {
    const agent = this.agentsList.find((a) => a.id === m.agentId)!;
    const spent = new Map<string, number>();
    return reqs.map((req) => {
      const dk = `d${startOfDay(req.at)}`;
      const mk = `m${startOfMonth(req.at)}`;
      const ctx: EngineContext = {
        agent: { ...agent, status: "active" },
        hasActiveKey: true,
        mandates: [m],
        merchant: this.merchantsList.find((x) => x.id === req.merchantId)!,
        usage: () => ({ day: spent.get(dk) ?? 0, month: spent.get(mk) ?? 0 }),
        recent: { count: 0, sumMinor: 0 },
        rand: rng(parseInt(sha256(req.id).slice(0, 8), 16)),
      };
      const o = evaluate(req, ctx).outcome;
      if (o === "approve") {
        spent.set(dk, (spent.get(dk) ?? 0) + req.amount.minor);
        spent.set(mk, (spent.get(mk) ?? 0) + req.amount.minor);
      }
      return o;
    });
  }

  async agents() {
    return clone(this.agentsList);
  }

  async agentActivity(agentId: string, days: number): Promise<AgentDay[]> {
    const now = this.now();
    const out: AgentDay[] = [];
    for (let i = days - 1; i >= 0; i--) {
      const start = startOfDay(now - i * DAY);
      const end = startOfDay(start + 26 * HOUR);
      const ds = this.decisionsList.filter((d) => d.agentId === agentId && d.at >= start && d.at < end);
      out.push({
        day: new Date(start).toLocaleDateString("en-GB", { day: "numeric", month: "short" }),
        start,
        spent: { minor: ds.filter((d) => d.postings.length > 0).reduce((s, d) => s + d.amount.minor, 0), currency: "GBP" },
        approve: ds.filter((d) => d.outcome === "approve").length,
        review: ds.filter((d) => d.outcome === "review").length,
        block: ds.filter((d) => d.outcome === "block").length,
      });
    }
    return out;
  }

  async registerAgent(input: { id: string; description: string; owner: string }, actor: string) {
    if (!/^agt_[a-z0-9-]{3,40}$/.test(input.id)) throw new Error("Use agt_ followed by 3–40 lowercase letters, digits or dashes.");
    if (this.agentsList.some((a) => a.id === input.id)) throw new Error("An agent with that ID already exists.");
    const now = this.now();
    const agent: Agent = { ...input, status: "active", createdAt: now, createdBy: actor, lastSeenAt: null };
    this.agentsList.push(agent);
    this.append({ at: now, event: "agent.registered", subject: agent.id, actor, ref: { kind: "agent", id: agent.id }, data: { owner: agent.owner } });
    const created = this.issueKey(agent.id, "live", actor);
    this.changed();
    return { agent: clone(agent), created };
  }

  async setAgentStatus(id: string, status: Agent["status"], actor: string) {
    const a = this.agentsList.find((x) => x.id === id);
    if (!a) throw new Error("That agent no longer exists.");
    if (a.status !== status) {
      a.status = status;
      a.pausedBy = status === "paused" ? actor : undefined;
      this.record({ event: status === "paused" ? "agent.paused" : "agent.resumed", subject: id, actor, ref: { kind: "agent", id }, data: {} });
    }
    return clone(a);
  }

  async keys(agentId?: string) {
    return clone(this.keysList.filter((k) => !agentId || k.agentId === agentId).sort((a, b) => b.createdAt - a.createdAt));
  }

  private issueKey(agentId: string, mode: ApiKey["mode"], actor: string): CreatedKey {
    // Shown once. In the real control plane only a hash of the secret is stored.
    const secret = `sk_${mode}_${this.id("x", 30).slice(2)}`;
    const key: ApiKey = { id: this.id("key", 8), agentId, mode, last4: secret.slice(-4), createdAt: this.now(), lastUsedAt: null, revokedAt: null };
    this.keysList.push(key);
    this.append({ at: key.createdAt, event: "key.created", subject: `sk_${mode}_••••${key.last4} · ${agentId}`, actor, ref: { kind: "key", id: key.id }, data: { agent: agentId, mode } });
    return { key: clone(key), secret };
  }

  async createKey(agentId: string, mode: ApiKey["mode"], actor: string) {
    const created = this.issueKey(agentId, mode, actor);
    this.changed();
    return created;
  }

  async revokeKey(keyId: string, actor: string) {
    const k = this.keysList.find((x) => x.id === keyId);
    if (!k) throw new Error("That key no longer exists.");
    if (!k.revokedAt) {
      k.revokedAt = this.now();
      this.record({ event: "key.revoked", subject: `sk_${k.mode}_••••${k.last4} · ${k.agentId}`, actor, ref: { kind: "key", id: k.id }, data: {} });
    }
    return clone(k);
  }

  async ledger(f: { events?: LedgerEventType[]; since?: number }) {
    return clone(this.entries.filter((e) => (!f.since || e.at >= f.since) && (!f.events || f.events.includes(e.event))).reverse());
  }

  private computeChain(): ChainStatus {
    let prev = GENESIS;
    const checkedAt = this.now();
    for (const e of this.entries) {
      const digest = sha256(e.payload);
      if (digest !== e.digest || e.prevHash !== prev || chainHash(prev, e.digest) !== e.hash) return { ok: false, entries: this.entries.length, checkedAt, firstBroken: e.seq };
      prev = e.hash;
    }
    return { ok: true, entries: this.entries.length, checkedAt };
  }

  async verifyChain() {
    this.verification = this.computeChain();
    this.changed();
    return clone(this.verification);
  }

  async lastVerification() {
    return clone(this.verification);
  }

  async workspace() {
    return clone(this.ws);
  }
  async renameWorkspace(name: string, actor: string) {
    if (!name.trim()) throw new Error("Enter a name.");
    this.ws.name = name.trim();
    void actor;
    this.changed();
    return clone(this.ws);
  }
  async members() {
    return clone(this.membersList);
  }
  async invites() {
    return clone(this.invitesList);
  }
  async invite(email: string, role: Invite["role"], actor: string) {
    if (!/^[^@\s]+@[^@\s]+\.[^@\s]+$/.test(email)) throw new Error("Enter a valid email address.");
    if (this.membersList.some((m) => m.email === email) || this.invitesList.some((i) => i.email === email)) throw new Error("That person is already a member or invited.");
    const inv: Invite = { id: this.id("inv", 6), email, role, sentAt: this.now() };
    this.invitesList.unshift(inv);
    this.record({ event: "member.invited", subject: `${email} as ${role}`, actor, ref: { kind: "member", id: inv.id }, data: { role } });
    return clone(inv);
  }
  async revokeInvite(id: string, actor: string) {
    void actor;
    this.invitesList = this.invitesList.filter((i) => i.id !== id);
    this.changed();
  }
  async setRole(memberId: string, role: Role, actor: string) {
    const m = this.membersList.find((x) => x.id === memberId);
    if (!m) throw new Error("That person is no longer a member.");
    if (m.role === "owner" && this.membersList.filter((x) => x.role === "owner").length === 1 && role !== "owner") throw new Error("A workspace needs at least one owner.");
    m.role = role;
    this.record({ event: "member.role_changed", subject: `${m.email} → ${role}`, actor, ref: { kind: "member", id: m.id }, data: { role } });
    return clone(m);
  }
  async removeMember(memberId: string, actor: string) {
    const m = this.membersList.find((x) => x.id === memberId);
    if (!m) return;
    if (m.role === "owner") throw new Error("Owners can't be removed. Change their role first.");
    this.membersList = this.membersList.filter((x) => x.id !== memberId);
    this.record({ event: "member.removed", subject: m.email, actor, ref: { kind: "member", id: m.id }, data: {} });
  }
  async webhooks(): Promise<Webhook[]> {
    return [
      { id: "wh_1", url: "https://hooks.strata-demo.example/decisions", events: ["decision.review", "decision.blocked"], enabled: true },
      { id: "wh_2", url: "https://hooks.strata-demo.example/ledger", events: ["mandate.updated", "key.created", "key.revoked"], enabled: false },
    ];
  }

  // ───────────────────────────── demo ─────────────────────────────

  async scenarios(): Promise<Scenario[]> {
    return clone(SCENARIOS);
  }

  async runScenario(n: number): Promise<Decision> {
    let last: Decision | null = null;
    const merchant = (id: string) => this.merchantsList.find((m) => m.id === id)!;
    for (const step of scenarioRequests(n, merchant)) {
      const req: PaymentRequest = { ...step, id: this.id("dec"), at: this.now() + (last ? 1 : 0), source: "attack-lab", scenario: n };
      last = this.decide(req);
      this.append(this.decisionEvent(last));
      // The split-purchase attack keeps paying until something stops it
      if (last.outcome !== "approve") break;
    }
    this.changed();
    return clone(last!);
  }

  async simulateTraffic(): Promise<Decision> {
    const pool = AGENTS.filter((a) => a.perDay >= 1);
    const a = pool[Math.floor(Math.random() * pool.length)];
    const m = this.merchantsList.find((x) => x.id === a.merchants[Math.floor(Math.random() * a.merchants.length)])!;
    let pounds = m.typical[0] + Math.random() * (m.typical[1] - m.typical[0]) * 0.8;
    const flags: Flags = {};
    const r = Math.random();
    if (r < 0.05) pounds = m.typical[1] * 3;
    else if (r < 0.08) flags.contextChange = true;
    else if (r < 0.1) flags.replay = true;
    const req: PaymentRequest = { id: this.id("dec"), at: this.now(), agentId: a.id, merchantId: m.id, checkoutHost: m.host, amount: gbp(Math.round(pounds * 100) / 100), flags, source: "agent" };
    const d = this.decide(req);
    this.append(this.decisionEvent(d));
    this.changed();
    return clone(d);
  }

  async testPayment(agentId: string): Promise<Decision> {
    const m = this.mandatesList.find((x) => x.agentId === agentId && x.status === "active");
    if (!m) throw new Error("This agent has no active mandate yet.");
    const merchant = this.merchantsList.find((x) => (m.merchantIds ? m.merchantIds.includes(x.id) : m.categories.includes(x.category)))!;
    const cap = Math.min(m.limits.perPayment.minor, m.review.enabled ? m.review.above.minor : m.limits.perPayment.minor);
    const amount = { minor: Math.max(100, Math.min(Math.round(cap * 0.4), Math.round(merchant.typical[0] * 100 * 1.2))), currency: "GBP" as const };
    const req: PaymentRequest = { id: this.id("dec"), at: this.now(), agentId, merchantId: merchant.id, checkoutHost: merchant.host, amount, flags: {}, source: "onboarding" };
    const d = this.decide(req);
    this.append(this.decisionEvent(d));
    this.changed();
    return clone(d);
  }

  async tamperWithEntry(): Promise<number> {
    if (this.tampered) return this.tampered.seq;
    // Edit an old decision's amount in place, without touching its hashes:
    // what a quiet UPDATE in the database would do.
    const target = this.entries.slice(Math.floor(this.entries.length * 0.6)).find((e) => e.event === "decision.approved")!;
    this.tampered = { seq: target.seq, payload: target.payload };
    target.payload = target.payload.replace(/"minor":(\d+)/, (_m, v: string) => `"minor":${Math.round(Number(v) / 10)}`);
    this.changed();
    return target.seq;
  }

  async restoreTamperedEntry() {
    if (!this.tampered) return;
    const t = this.tampered;
    const e = this.entries.find((x) => x.seq === t.seq);
    if (e) e.payload = t.payload;
    this.tampered = null;
    this.verification = this.computeChain();
    this.changed();
  }

  async reset() {
    this.build();
    this.changed();
  }
}

function bump(b: Overview["byBucket"][number] | undefined, o: Outcome) {
  if (b) b[o] += 1;
}

/** One line describing what changed between two versions of a mandate. */
export function describeChange(a: MandateInput, b: MandateInput, merchants: Merchant[]): string {
  const out: string[] = [];
  const lim = (k: keyof MandateInput["limits"], label: string) => {
    if (a.limits[k].minor !== b.limits[k].minor) out.push(`${label} ${moneyShort(a.limits[k])} → ${moneyShort(b.limits[k])}`);
  };
  if (a.name !== b.name) out.push(`Renamed to "${b.name}"`);
  lim("perPayment", "Per-payment cap");
  lim("perDay", "Daily limit");
  lim("perMonth", "Monthly limit");
  const addC = b.categories.filter((c) => !a.categories.includes(c));
  const remC = a.categories.filter((c) => !b.categories.includes(c));
  if (addC.length) out.push(`Added ${addC.join(", ")}`);
  if (remC.length) out.push(`Removed ${remC.join(", ")}`);
  const name = (id: string) => merchants.find((m) => m.id === id)?.name ?? id;
  if ((a.merchantIds === null) !== (b.merchantIds === null)) out.push(b.merchantIds ? "Limited to named merchants" : "Any merchant in these categories");
  else if (a.merchantIds && b.merchantIds) {
    const add = b.merchantIds.filter((x) => !a.merchantIds!.includes(x)).map(name);
    const rem = a.merchantIds.filter((x) => !b.merchantIds!.includes(x)).map(name);
    if (add.length) out.push(`Added ${add.join(", ")}`);
    if (rem.length) out.push(`Removed ${rem.join(", ")}`);
  }
  if (a.days.join() !== b.days.join() || JSON.stringify(a.hours) !== JSON.stringify(b.hours)) out.push("Changed when it applies");
  if (a.review.enabled !== b.review.enabled) out.push(b.review.enabled ? `Review over ${moneyShort(b.review.above)}` : "No review");
  else if (b.review.enabled && a.review.above.minor !== b.review.above.minor) out.push(`Review threshold ${moneyShort(a.review.above)} → ${moneyShort(b.review.above)}`);
  return out.join("; ");
}

export type { Money };
