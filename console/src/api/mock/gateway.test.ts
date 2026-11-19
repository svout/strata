import { describe, expect, it } from "vitest";
import { MockGateway } from "./gateway";
import { canonicalJson, sha256 } from "./sha256";

const NOW = new Date("2026-11-13T21:00:00Z").getTime();
const make = () => new MockGateway(20261113, () => NOW);

describe("sha256", () => {
  it("matches the FIPS 180-4 test vectors", () => {
    expect(sha256("")).toBe("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855");
    expect(sha256("abc")).toBe("ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad");
    expect(sha256("abcdbcdecdefdefgefghfghighijhijkijkljklmklmnlmnomnopnopq")).toBe(
      "248d6a61d20638b8e5c026930c3e6039a33ce45964ff2167f6ecedd419db06c1",
    );
  });

  it("canonical JSON ignores key order", () => {
    expect(canonicalJson({ b: 1, a: [2, { d: 3, c: 4 }] })).toBe(canonicalJson({ a: [2, { c: 4, d: 3 }], b: 1 }));
  });
});

describe("mock gateway", () => {
  it("builds the same workspace from the same seed", async () => {
    const a = await make().decisions({});
    const b = await make().decisions({});
    expect(a.length).toBeGreaterThan(500);
    expect(a.map((d) => d.id)).toEqual(b.map((d) => d.id));
  });

  it("explains every decision and names the control that fired", async () => {
    for (const d of await make().decisions({})) {
      expect(d.reasonCodes.length).toBeGreaterThan(0);
      expect(d.explanation.length).toBeGreaterThan(10);
      expect(d.controlFired).toBeTruthy();
      if (d.outcome !== "approve") expect(d.checks.some((c) => c.status !== "pass") || d.layers.risk.status === "review").toBe(true);
    }
  });

  it("moves money only when approved, and postings always sum to zero", async () => {
    for (const d of await make().decisions({})) {
      const moved = d.outcome === "approve" || (d.review && d.review.status === "approved");
      expect(d.postings.length > 0).toBe(!!moved);
      expect(d.postings.reduce((s, p) => s + p.amount.minor, 0)).toBe(0);
    }
  });

  it("writes every decision to the chain, and the chain verifies", async () => {
    const g = make();
    const ds = await g.decisions({});
    expect(ds.every((d) => d.layers.ledger.seq > 1000 && d.layers.ledger.hash.length === 64)).toBe(true);
    expect((await g.verifyChain()).ok).toBe(true);
  });

  it("catches an entry edited after it was written, and names it", async () => {
    const g = make();
    const seq = await g.tamperWithEntry();
    const v = await g.verifyChain();
    expect(v.ok).toBe(false);
    expect(v.ok === false && v.firstBroken).toBe(seq);
    await g.restoreTamperedEntry();
    expect((await g.verifyChain()).ok).toBe(true);
  });

  it("ends all eight threat-model scenarios as expected", async () => {
    const g = make();
    for (const s of await g.scenarios()) {
      const d = await g.runScenario(s.n);
      expect({ n: s.n, outcome: d.outcome }).toEqual({ n: s.n, outcome: s.expected });
    }
  });

  it("records a human approval as its own ledger entry and moves the money", async () => {
    const g = make();
    const [first] = await g.reviewQueue();
    expect(first).toBeDefined();
    const before = (await g.ledger({})).length;
    const d = await g.approve(first.id, "Ivan Kovalchuk");
    expect(d.review).toMatchObject({ status: "approved", by: "Ivan Kovalchuk" });
    expect(d.postings).toHaveLength(2);
    const ledger = await g.ledger({});
    expect(ledger).toHaveLength(before + 1);
    expect(ledger[0]).toMatchObject({ event: "review.approved", actor: "Ivan Kovalchuk" });
    await expect(g.approve(first.id, "Ivan Kovalchuk")).rejects.toThrow();
  });

  it("blocks a paused agent's next payment", async () => {
    const g = make();
    await g.setAgentStatus("agt_travel-booker", "paused", "Ivan Kovalchuk");
    const d = await g.runScenario(8);
    expect(d.outcome).toBe("block");
    expect(d.reasonCodes).toContain("AGENT_PAUSED");
  });

  it("bumps the mandate version and logs what changed", async () => {
    const g = make();
    const m = (await g.mandate("mnd_travel"))!;
    const saved = await g.saveMandate(m.id, { ...m, limits: { ...m.limits, perPayment: { minor: 90000, currency: "GBP" } } }, "Ivan Kovalchuk");
    expect(saved.version).toBe(m.version + 1);
    const [latest] = await g.mandateHistory(m.id);
    expect(latest.change).toContain("Per-payment cap £800 → £900");
  });

  it("replays a draft against the agent's real history", async () => {
    const g = make();
    const m = (await g.mandate("mnd_travel"))!;
    const tighter = await g.replayMandate(m.id, { ...m, limits: { ...m.limits, perPayment: { minor: 20000, currency: "GBP" } } }, 30);
    expect(tighter.requests).toBeGreaterThan(50);
    expect(tighter.draft.block).toBeGreaterThan(tighter.current!.block);
    expect(tighter.changed.length).toBeGreaterThan(0);
  });

  it("keeps at least one owner", async () => {
    await expect(make().setRole("usr_ivan", "admin", "Ivan Kovalchuk")).rejects.toThrow(/owner/);
  });

  it("gives every chart bucket a time window that holds exactly its decisions", async () => {
    const g = make();
    const o = await g.overview("7d");
    expect(o.byBucket).toHaveLength(7);
    for (const b of o.byBucket) {
      const ds = await g.decisions({ since: b.start, until: b.end });
      expect(ds.length).toBe(b.approve + b.review + b.block);
    }
    expect(o.trend).toHaveLength(7);
  });

  it("sums an agent's daily spend from approved payments only", async () => {
    const g = make();
    const days = await g.agentActivity("agt_travel-booker", 14);
    expect(days).toHaveLength(14);
    const last = days[days.length - 1];
    const ds = await g.decisions({ agentId: "agt_travel-booker", since: last.start });
    expect(last.spent.minor).toBe(ds.filter((d) => d.postings.length).reduce((s, d) => s + d.amount.minor, 0));
  });
});
