"use client";

import { Suspense, useMemo, useRef, useState } from "react";
import { Tag, reasonTone } from "@/components/ui/Tag";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import clsx from "clsx";
import type { Decision, DecisionFilter, ReasonCode } from "@/api";
import { OutcomeChip } from "@/components/brand";
import DecisionPanel from "@/components/decision/DecisionPanel";
import { useReview } from "@/components/decision/ReviewActions";
import { EmptyState, ErrorNote, PageHeader, Segmented, Skeleton } from "@/components/ui";
import { Icon } from "@/components/ui/icons";
import { useHotkeys } from "@/lib/useHotkeys";
import { useResource } from "@/lib/useResource";
import { clock, clockShort, dayMonth, money } from "@/lib/format";
import { REASON_LABEL } from "@/api/reasons";

type OutcomeTab = "all" | NonNullable<DecisionFilter["outcome"]>;
const PAGE = 100;

export default function DecisionsPage() {
  return (
    <Suspense fallback={null}>
      <Decisions />
    </Suspense>
  );
}

/** P4 · Find any payment and see exactly why. */
function Decisions() {
  const params = useSearchParams();
  const router = useRouter();
  const path = usePathname();
  const outcome = (params.get("outcome") ?? "all") as OutcomeTab;
  const agent = params.get("agent") ?? "";
  const reason = (params.get("reason") ?? "") as ReasonCode | "";
  const selected = params.get("id");
  // A time window arrives from clicking a chart bar on Overview
  const since = Number(params.get("since")) || undefined;
  const until = Number(params.get("until")) || undefined;
  const [q, setQ] = useState("");
  const [shown, setShown] = useState(PAGE);
  const tableRef = useRef<HTMLDivElement>(null);

  const setParam = (k: string, v: string | null) => {
    const p = new URLSearchParams(params.toString());
    if (v) p.set(k, v);
    else p.delete(k);
    router.replace(`${path}?${p.toString()}`, { scroll: false });
  };

  // Outcome is filtered here so the tabs can show counts for the other filters
  const { data: all, error } = useResource(`decisions:${agent}:${reason}:${q}:${since}:${until}`, (a) =>
    a.decisions({ agentId: agent || undefined, reason: reason || undefined, q: q || undefined, since, until }),
  );
  const { data: agents } = useResource("agents", (a) => a.agents());
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const { data: current } = useResource(`decision:${selected}`, (a) => (selected ? a.decision(selected) : Promise.resolve(null)));
  const merchantName = (id: string) => merchants?.find((m) => m.id === id)?.name ?? id;

  const counts = useMemo(() => {
    const c = { all: 0, approved: 0, review: 0, blocked: 0, declined: 0 };
    for (const d of all ?? []) {
      c.all++;
      if (d.postings.length) c.approved++;
      if (d.review?.status === "pending") c.review++;
      if (d.outcome === "block") c.blocked++;
      if (d.review?.status === "declined") c.declined++;
    }
    return c;
  }, [all]);

  const rows = useMemo(
    () =>
      (all ?? []).filter((d) =>
        outcome === "all"
          ? true
          : outcome === "approved"
            ? d.postings.length > 0
            : outcome === "review"
              ? d.review?.status === "pending"
              : outcome === "blocked"
                ? d.outcome === "block"
                : d.review?.status === "declined",
      ),
    [all, outcome],
  );
  const visible = rows.slice(0, shown);

  const review = useReview();
  const move = (step: number) => {
    if (!visible.length) return;
    const i = visible.findIndex((d) => d.id === selected);
    const next = visible[Math.max(0, Math.min(visible.length - 1, i + step))];
    setParam("id", next.id);
    requestAnimationFrame(() => tableRef.current?.querySelector(`[data-id="${next.id}"]`)?.scrollIntoView({ block: "nearest" }));
  };
  useHotkeys({
    j: () => move(1),
    k: () => move(-1),
    escape: () => setParam("id", null),
    a: () => current?.review?.status === "pending" && review.approve(current),
    d: () => current?.review?.status === "pending" && review.decline(current),
  });

  const exportCsv = () => {
    const head = "id,time,agent,merchant,amount_gbp,outcome,review,control_fired,reason_codes,ledger_seq";
    const lines = rows.map((d) =>
      [d.id, new Date(d.at).toISOString(), d.agentId, `"${merchantName(d.merchantId)}"`, (d.amount.minor / 100).toFixed(2), d.outcome, d.review?.status ?? "", `"${d.controlFired}"`, d.reasonCodes.join("|"), d.layers.ledger.seq].join(","),
    );
    const url = URL.createObjectURL(new Blob([[head, ...lines].join("\n")], { type: "text/csv" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `strata-decisions-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="flex h-full min-h-0">
      <div className="flex min-w-0 flex-1 flex-col gap-4 overflow-y-auto px-5 py-7 md:px-7" ref={tableRef}>
        <PageHeader
          page="decisions"
          actions={
            <Button variant="outline" size="small" onClick={exportCsv} disabled={!rows.length}>
              <Icon name="download" size={15} className="mr-1.5" /> Export
            </Button>
          }
        />
        <div className="flex flex-wrap items-center gap-2">
          <Segmented<OutcomeTab>
            label="Outcome"
            value={outcome}
            onChange={(v) => setParam("outcome", v === "all" ? null : v)}
            options={[
              { value: "all", label: "All", count: counts.all },
              { value: "approved", label: "Approved", count: counts.approved },
              { value: "review", label: "Review", count: counts.review },
              { value: "blocked", label: "Blocked", count: counts.blocked },
              { value: "declined", label: "Declined", count: counts.declined },
            ]}
          />
          <label className="sr-only" htmlFor="f-agent">Agent</label>
          <select id="f-agent" className="field !h-[34px] !w-auto text-[13px]" value={agent} onChange={(e) => setParam("agent", e.target.value || null)}>
            <option value="">Agent: any</option>
            {agents?.map((a) => (
              <option key={a.id} value={a.id}>{a.id}</option>
            ))}
            <option value="unregistered">Unregistered keys</option>
          </select>
          <label className="sr-only" htmlFor="f-reason">Reason</label>
          <select id="f-reason" className="field !h-[34px] !w-auto text-[13px]" value={reason} onChange={(e) => setParam("reason", e.target.value || null)}>
            <option value="">Reason: any</option>
            {(Object.keys(REASON_LABEL) as ReasonCode[]).map((c) => (
              <option key={c} value={c}>{REASON_LABEL[c]}</option>
            ))}
          </select>
          <div className="relative ml-auto min-w-[220px] flex-1 md:max-w-[280px] md:flex-none">
            <label className="sr-only" htmlFor="f-q">Search</label>
            <input id="f-q" className="field !h-[34px] pr-8 text-[13px]" placeholder="Search amount, agent, ID" value={q} onChange={(e) => { setQ(e.target.value); setShown(PAGE); }} />
            <Icon name="search" size={15} className="absolute top-1/2 right-2.5 -translate-y-1/2 text-fg-muted" />
          </div>
        </div>
        {since && until && (
          <div className="flex flex-wrap items-center gap-2 text-[13px]">
            <span className="rounded-full bg-brand-subtle px-3 py-1 font-semibold text-brand-on-subtle">
              {dayMonth(since)} {clockShort(since)} – {until - since <= 3600_000 ? clockShort(until) : `${dayMonth(until - 1)}`}
            </span>
            <button
              type="button"
              className="font-semibold text-fg-muted hover:text-fg"
              onClick={() => {
                const p = new URLSearchParams(params.toString());
                p.delete("since");
                p.delete("until");
                router.replace(`${path}?${p.toString()}`, { scroll: false });
              }}
            >
              Clear time window ✕
            </button>
          </div>
        )}

        {error && <ErrorNote text={error} />}
        <div className="overflow-x-auto rounded-md border border-line">
          {!all ? (
            <div className="flex flex-col gap-2 p-4">{Array.from({ length: 8 }, (_, i) => <Skeleton key={i} className="h-9" />)}</div>
          ) : rows.length === 0 ? (
            <EmptyState title="No payments match" body="Try another outcome, agent or reason, or clear the search." filled={0} />
          ) : (
            <table className="data-table">
              <thead>
                <tr>
                  <th>Time</th>
                  <th>Agent</th>
                  <th>Merchant</th>
                  <th className="!text-right">Amount</th>
                  <th>Decision</th>
                  <th>Reason</th>
                </tr>
              </thead>
              <tbody>
                {visible.map((d) => (
                  <Row key={d.id} d={d} merchant={merchantName(d.merchantId)} selected={d.id === selected} onOpen={() => setParam("id", d.id)} />
                ))}
              </tbody>
            </table>
          )}
          {rows.length > shown && (
            <div className="border-t border-line-soft p-3 text-center">
              <Button variant="outline" size="small" onClick={() => setShown((n) => n + PAGE)}>
                Show {Math.min(PAGE, rows.length - shown)} more of {(rows.length - shown).toLocaleString("en-GB")}
              </Button>
            </div>
          )}
        </div>
      </div>

      {selected && current && (
        <>
          <button aria-label="Close details" className="fixed inset-0 z-30 bg-ink/30 xl:hidden" onClick={() => setParam("id", null)} />
          <div className="fixed inset-y-0 right-0 z-40 w-[min(100vw,440px)] border-l border-line shadow-[var(--shadow-drawer)] xl:static xl:z-auto xl:w-[440px] xl:shrink-0">
            <DecisionPanel decision={current} merchant={merchants?.find((m) => m.id === current.merchantId)} onClose={() => setParam("id", null)} />
          </div>
        </>
      )}
      {review.dialog}
    </div>
  );
}

function Row({ d, merchant, selected, onOpen }: { d: Decision; merchant: string; selected: boolean; onOpen: () => void }) {
  return (
    <tr data-href data-id={d.id} aria-selected={selected} onClick={onOpen} className={clsx(Date.now() - d.at < 8000 && "animate-fresh")}>
      <td className="font-mono text-[12.5px] text-fg-muted">
        <span className="hidden 2xl:inline">{dayMonth(d.at)} </span>
        {clock(d.at)}
      </td>
      <td className="font-mono text-[12.5px]">
        <button type="button" className="text-left hover:underline focus-visible:underline" onClick={(e) => { e.stopPropagation(); onOpen(); }}>
          {d.agentId}
        </button>
        {d.source === "attack-lab" && <span className="ml-1.5 rounded bg-surface px-1.5 py-0.5 text-[10.5px] text-fg-muted">attack</span>}
      </td>
      <td>{merchant}</td>
      <td className="text-right font-mono">{money(d.amount)}</td>
      <td>
        <OutcomeChip decision={d} />
      </td>
      <td className="max-w-[240px]">
        {d.outcome === "approve" ? (
          <span className="text-[12.5px] text-fg-subtle">—</span>
        ) : (
          <Tag tone={reasonTone(d.reasonCodes[0])} size="xs">
            {REASON_LABEL[d.reasonCodes[0]]}
          </Tag>
        )}
      </td>
    </tr>
  );
}
