"use client";

import { useRef, useState } from "react";
import { Allowed } from "@/components/ui/Allowed";
import Button from "@mui/material/Button";
import clsx from "clsx";
import type { Decision, Outcome } from "@/api";
import { DecisionChip, Overline } from "@/components/brand";
import { LayerTrace, layersOf, type LayerView } from "@/components/decision/LayerTrace";
import { PageHeader, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { api, useResource } from "@/lib/useResource";
import { money } from "@/lib/format";

const OUT: Record<Outcome, { tone: "approve" | "review" | "block"; label: string }> = {
  approve: { tone: "approve", label: "Allow" },
  review: { tone: "review", label: "Step up" },
  block: { tone: "block", label: "Block" },
};
const IDLE: LayerView[] = [
  { name: "Mandate", tone: "idle", summary: "" },
  { name: "Risk", tone: "idle", summary: "" },
  { name: "Ledger", tone: "idle", summary: "" },
];
const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));

/**
 * Attack replay (P7 phase deliverable): run a threat-model scenario and
 * watch it be stopped. Runs go into Decisions and the ledger like real
 * traffic, marked as attack-lab. The real harness (make attack) replaces
 * the mock here once the gateway exists.
 */
export default function AttackLabPage() {
  const toast = useToast();
  const { data: scenarios } = useResource("scenarios", (a) => a.scenarios());
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const [running, setRunning] = useState<number | null>(null);
  const [step, setStep] = useState(-1);
  const [shown, setShown] = useState<Decision | null>(null);
  const [results, setResults] = useState<Record<number, Outcome>>({});
  const busy = useRef(false);

  const run = async (n: number) => {
    const d = await api().runScenario(n);
    setRunning(n);
    setShown(d);
    for (let i = 0; i < 3; i++) {
      setStep(i);
      await wait(matchMedia("(prefers-reduced-motion: reduce)").matches ? 0 : 520);
    }
    setStep(-1);
    setRunning(null);
    setResults((r) => ({ ...r, [n]: d.outcome }));
    return d;
  };

  const runOne = async (n: number) => {
    if (busy.current) return;
    busy.current = true;
    try {
      await run(n);
    } catch (e) {
      setRunning(null);
      toast(e instanceof Error ? e.message : "The scenario couldn't run.", "block");
    } finally {
      busy.current = false;
    }
  };

  const runAll = async () => {
    if (busy.current || !scenarios) return;
    busy.current = true;
    setResults({});
    let matched = 0;
    try {
      for (const sc of scenarios) {
        const d = await run(sc.n);
        if (d.outcome === sc.expected) matched++;
        await wait(250);
      }
    } catch (e) {
      setRunning(null);
      toast(e instanceof Error ? e.message : "A scenario couldn't run.", "block");
      return;
    } finally {
      busy.current = false;
    }
    toast(`${matched} of ${scenarios.length} scenarios ended as the threat model expects.`, matched === scenarios.length ? "approve" : "review");
  };

  const done = Object.keys(results).length;
  const matched = scenarios?.filter((sc) => results[sc.n] === sc.expected).length ?? 0;

  return (
    <div className="mx-auto flex max-w-[1280px] flex-col gap-5 px-5 py-7 md:px-8">
      <PageHeader
        page="attacks"
        meta={<Tag tone="demo">Sample data</Tag>}
        actions={
          <Allowed action="runDemo">
            {(ok) => (
              <Button variant="primary" onClick={runAll} disabled={!ok || running != null}>
                <Icon name="play" size={15} className="mr-1.5" /> Run all eight
              </Button>
            )}
          </Allowed>
        }
      />

      <section aria-live="polite" className="grid items-center gap-6 rounded-lg border border-line p-6 lg:grid-cols-[minmax(0,1fr)_minmax(0,1.1fr)]">
        <div className="flex flex-col gap-3">
          <Overline>{running != null ? `Scenario ${running} · running` : shown ? `Scenario ${shown.scenario} · last run` : "Pick a scenario"}</Overline>
          {shown ? (
            <>
              <div className="flex flex-col gap-1 rounded-md border border-line bg-surface p-4 text-[13px]">
                <span className="font-mono text-[12px] text-fg-muted">POST /v1/payments · {shown.id}</span>
                <b className="text-[15px] font-semibold">
                  {shown.agentId} → {merchants?.find((m) => m.id === shown.merchantId)?.name}
                </b>
                <span className="font-mono text-[24px] font-medium">{money(shown.amount)}</span>
                <span className="font-mono text-[12px] text-fg-muted">checkout {shown.checkoutHost}</span>
              </div>
              <p className="min-h-[48px] text-[15px] font-medium">{running != null ? "Checking…" : shown.explanation}</p>
              {running == null && (
                <div className="flex flex-wrap gap-2">
                  <Button variant="outline" size="small" href={`/decisions?id=${shown.id}`}>Open full trace</Button>
                  <Button variant="ghost" size="small" href={`/disputes/${shown.id}`}>Dispute view</Button>
                </div>
              )}
            </>
          ) : (
            <p className="text-[14px] text-fg-muted">Run a scenario to watch the checks.</p>
          )}
        </div>
        <LayerTrace layers={shown ? layersOf(shown) : IDLE} upTo={running != null ? step : shown ? undefined : -1} />
      </section>

      {done > 0 && (
        <div className={clsx("rounded-md p-3.5 text-[14px]", matched === done ? "bg-approve-tint text-approve-fg" : "bg-review-tint text-review-fg")}>
          <b>{matched} of {done}</b> scenario{done > 1 ? "s" : ""} ended as the threat model expects.
        </div>
      )}

      <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
        {!scenarios
          ? Array.from({ length: 8 }, (_, i) => <Skeleton key={i} className="h-44 rounded-md" />)
          : scenarios.map((sc) => {
              const got = results[sc.n];
              return (
                <article key={sc.n} className={clsx("flex flex-col gap-2.5 rounded-md border border-line p-4", running === sc.n && "outline-2 outline-brand")}>
                  <span className="flex items-center justify-between gap-2">
                    <Overline>#{sc.n}</Overline>
                    <Tag tone="outline" size="xs">{sc.attacker}</Tag>
                  </span>
                  <h2 className="text-[15px] font-semibold tracking-[-0.01em]">{sc.title}</h2>
                  <Tag tone="control" size="xs" className="self-start !whitespace-normal" title="Control under test">{sc.control}</Tag>
                  <div className="mt-auto flex flex-wrap items-center gap-1.5 text-[12.5px]">
                    <span className="text-fg-muted">Expected</span>
                    <DecisionChip tone={OUT[sc.expected].tone}>{OUT[sc.expected].label}</DecisionChip>
                    {got && (
                      <>
                        <span className="text-fg-muted">Got</span>
                        <DecisionChip tone={OUT[got].tone}>{OUT[got].label}</DecisionChip>
                        <span aria-label={got === sc.expected ? "matches" : "doesn't match"} className={got === sc.expected ? "text-approve-fg" : "text-block-fg"}>
                          {got === sc.expected ? "✓" : "✕"}
                        </span>
                      </>
                    )}
                  </div>
                  <Allowed action="runDemo">
                    {(ok) => (
                      <Button variant="outline" size="small" className="self-start" disabled={!ok || running != null} onClick={() => runOne(sc.n)}>
                        <Icon name="play" size={13} className="mr-1" /> Run
                      </Button>
                    )}
                  </Allowed>
                </article>
              );
            })}
      </div>
    </div>
  );
}
