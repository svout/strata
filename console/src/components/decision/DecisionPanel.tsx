"use client";

import Link from "next/link";
import { Allowed } from "@/components/ui/Allowed";
import Button from "@mui/material/Button";
import clsx from "clsx";
import type { Decision, Merchant } from "@/api/types";
import { OutcomeChip, Overline } from "@/components/brand";
import { Icon } from "@/components/ui/icons";
import { Kbd } from "@/components/ui";
import { Tag, reasonTone } from "@/components/ui/Tag";
import { useToast } from "@/components/ui/Toasts";
import { clock, dayMonth, money } from "@/lib/format";
import { LayerTrace, layersOf } from "./LayerTrace";
import { useReview } from "./ReviewActions";

const BUDGET = { signature: 2, mandate: 3, policy: 4, risk: 8, audit: 5 } as const;
const TIMING_LABEL: Record<keyof typeof BUDGET, string> = { signature: "Signature", mandate: "Mandate", policy: "Policy", risk: "Risk", audit: "Audit write" };

/**
 * P4 detail drawer: why one payment went the way it did. Everything shown
 * here comes from the decision record; the console adds no reasoning of its
 * own (console/CLAUDE.md).
 */
export default function DecisionPanel({ decision: d, merchant, onClose }: { decision: Decision; merchant?: Merchant; onClose: () => void }) {
  const review = useReview();
  const toast = useToast();
  const pending = d.review?.status === "pending";
  const t = d.timingsMs;
  const total = t.signature + t.mandate + t.policy + t.risk + t.audit;

  return (
    <aside aria-label={`Payment ${d.id}`} className="flex h-full w-full flex-col bg-elevated">
      <div className="flex items-start justify-between gap-3 border-b border-line-soft p-6 pb-4">
        <div className="flex min-w-0 flex-col gap-1">
          <span className="truncate font-mono text-[12px] text-fg-muted">
            {d.id} · {dayMonth(d.at)} {clock(d.at)}
          </span>
          <span className="font-mono text-[30px] leading-tight font-medium">{money(d.amount)}</span>
          <span className="text-[14px]">
            <Link href={d.agentRegistered ? `/agents?id=${d.agentId}` : "/agents"} className="font-mono text-[13px] hover:underline">
              {d.agentId}
            </Link>{" "}
            → {merchant?.name ?? d.merchantId}
          </span>
        </div>
        <button type="button" aria-label="Close" onClick={onClose} className="grid size-8 shrink-0 place-items-center rounded-md hover:bg-neutral-tint">
          <Icon name="close" size={16} />
        </button>
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto p-6">
        {/* 1 · The answer: outcome and one sentence */}
        <div className={clsx("flex flex-col gap-2 rounded-lg border border-line bg-elevated p-4", d.outcome === "approve" || d.review?.status === "approved" ? "shadow-[inset_3px_0_0_var(--color-approve)]" : d.outcome === "block" || d.review?.status === "declined" ? "shadow-[inset_3px_0_0_var(--color-block)]" : "shadow-[inset_3px_0_0_var(--color-review)]")}>
          <div className="flex flex-wrap items-center gap-2">
            <OutcomeChip decision={d} size="md" />
            {d.source === "attack-lab" && <Tag tone="demo" size="xs">Attack · scenario {d.scenario}</Tag>}
            {d.layers.risk.degraded && <Tag tone="outline" size="xs">Rules-only scoring</Tag>}
          </div>
          <p className="text-[15px] leading-snug">{d.explanation}</p>
        </div>

        {/* 2 · How it was decided: the three layers */}
        <section className="flex flex-col gap-2.5">
          <Overline>How it was decided</Overline>
          <LayerTrace layers={layersOf(d)} />
        </section>

        {/* 3 · Key facts, as tags */}
        <section className="flex flex-col gap-2">
          <Overline>At a glance</Overline>
          <dl className="grid grid-cols-[110px_minmax(0,1fr)] items-center gap-x-3 gap-y-2 text-[13px]">
            <dt className="text-fg-muted">Decided by</dt>
            <dd className="flex flex-wrap gap-1.5">
              <Tag tone={reasonTone(d.reasonCodes[0])}>{d.outcome === "approve" ? "All checks passed" : d.controlFired}</Tag>
            </dd>
            <dt className="text-fg-muted">Mandate</dt>
            <dd>
              {d.mandateId ? (
                <Link href={`/mandates/${d.mandateId}`} className="hover:underline">
                  <Tag tone="control">
                    {d.mandateId} · v{d.mandateVersion}
                  </Tag>
                </Link>
              ) : (
                <Tag tone="outline">None</Tag>
              )}
            </dd>
            <dt className="text-fg-muted">Risk score</dt>
            <dd>
              {d.layers.risk.score == null ? (
                <Tag tone="outline">Not scored</Tag>
              ) : (
                <Tag tone={d.layers.risk.score >= 0.65 ? "review" : "neutral"} mono>
                  {d.layers.risk.score.toFixed(2)} · {d.layers.risk.score >= 0.65 ? "high" : "normal"}
                </Tag>
              )}
            </dd>
            <dt className="text-fg-muted">Money moved</dt>
            <dd>{d.postings.length ? <Tag tone="approve">Yes · {money(d.amount)}</Tag> : <Tag tone="outline">{pending ? "Not yet" : "No · £0"}</Tag>}</dd>
            <dt className="text-fg-muted">Ledger</dt>
            <dd>
              <Link href={`/ledger?seq=${d.layers.ledger.seq}`} className="hover:underline">
                <Tag tone="record" mono>
                  #{d.layers.ledger.seq}
                </Tag>
              </Link>
            </dd>
          </dl>
        </section>

        {/* 4 · Everything else, folded away */}
        <section className="flex flex-col">
          <Overline className="mb-1">Details</Overline>
          <Fold title="Every check, in order" count={d.checks.length}>
            <ol className="flex flex-col gap-1.5 text-[13px]">
              {d.checks.map((c, i) => (
                <li key={i} className="flex items-start gap-2.5">
                  <span
                    aria-label={c.status}
                    className={clsx(
                      "mt-0.5 grid size-[18px] shrink-0 place-items-center rounded-full font-mono text-[11px] font-semibold",
                      c.status === "pass" ? "bg-approve-tint text-approve-fg" : c.status === "review" ? "bg-review-tint text-review-fg" : "bg-block-tint text-block-fg",
                    )}
                  >
                    {c.status === "pass" ? "✓" : c.status === "review" ? "!" : "✕"}
                  </span>
                  <span>
                    <b className="font-semibold">{c.control}</b> <span className="text-fg-muted">· {c.detail}</span>
                  </span>
                </li>
              ))}
            </ol>
            <div className="mt-2.5 flex flex-wrap gap-1.5">
              {d.reasonCodes.map((c) => (
                <Tag key={c} tone="outline" mono size="xs">
                  {c}
                </Tag>
              ))}
            </div>
          </Fold>

          {d.layers.risk.status !== "skipped" && !d.layers.risk.degraded && (
            <Fold title="Risk breakdown">
              <div className="flex flex-col gap-2">
                {d.layers.risk.contributions.map((c) => (
                  <div key={c.code} className="grid grid-cols-[minmax(0,1fr)_110px_44px] items-center gap-2.5 text-[13px]">
                    <span>{c.label}</span>
                    <span className="h-1.5 rounded-full bg-neutral-tint">
                      <span className="block h-full rounded-full bg-review" style={{ width: `${Math.min(100, (c.value / 0.7) * 100)}%` }} />
                    </span>
                    <span className="text-right font-mono">+{c.value.toFixed(2)}</span>
                  </div>
                ))}
                <p className="text-[12px] text-fg-muted">Held for a person at 0.65 or above · {d.layers.risk.model}</p>
              </div>
            </Fold>
          )}

          {d.postings.length > 0 && (
            <Fold title="Money movements" count={d.postings.length}>
              <table className="w-full text-[12.5px]">
                <tbody>
                  {d.postings.map((p) => (
                    <tr key={p.account} className="border-t border-line-soft first:border-0">
                      <td className="py-1.5 font-mono">{p.account}</td>
                      <td className="py-1.5">
                        <Tag tone="outline" size="xs">
                          {p.direction}
                        </Tag>
                      </td>
                      <td className="py-1.5 text-right font-mono">{money(p.amount)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
              <p className="mt-1.5 text-[12px] text-fg-muted">Always sums to {money({ minor: 0, currency: "GBP" })}.</p>
            </Fold>
          )}

          {d.chain && (
            <Fold title="Mandate chain">
              <div className="flex flex-col text-[12.5px]">
                <ChainCard title="Intent" id={d.chain.intent.id}>
                  Signed by {d.chain.intent.signedBy}. {d.chain.intent.scope}
                </ChainCard>
                <span aria-hidden className="ml-5 h-3 border-l-2 border-dotted border-line" />
                <ChainCard title="Cart" id={d.chain.cart.id} bad={d.reasonCodes.includes("CART_EXCEEDS_INTENT")}>
                  {d.chain.cart.items.map((i) => (
                    <span key={i.name} className="flex justify-between gap-3">
                      <span>{i.name}</span>
                      <span className="font-mono">{money(i.amount)}</span>
                    </span>
                  ))}
                </ChainCard>
                <span aria-hidden className="ml-5 h-3 border-l-2 border-dotted border-line" />
                <ChainCard title="Payment" id={d.chain.payment.id} bad={d.chain.payment.replayed}>
                  {d.chain.payment.rail} · {d.chain.payment.instrument}
                  {d.chain.payment.replayed && " · nonce already used"}
                </ChainCard>
              </div>
            </Fold>
          )}

          <Fold title="Time added" count={`${total.toFixed(1)} ms`}>
            <div className="grid grid-cols-[86px_minmax(0,1fr)_64px] items-center gap-x-3 gap-y-1.5 text-[12.5px]">
              {(Object.keys(BUDGET) as (keyof typeof BUDGET)[]).map((k) => (
                <Timing key={k} label={TIMING_LABEL[k]} value={t[k]} budget={BUDGET[k]} />
              ))}
              <Timing label="Total" value={total} budget={25} strong />
            </div>
            <p className="mt-1.5 text-[12px] text-fg-muted">Sample timings · budget 25 ms at p99</p>
          </Fold>
        </section>

        <nav aria-label="Related" className="flex flex-wrap gap-2">
          <Button variant="outline" size="small" href={`/disputes/${d.id}`}>
            Dispute view
          </Button>
          {d.agentRegistered && (
            <Button variant="ghost" size="small" href={`/agents?id=${d.agentId}`}>
              Agent
            </Button>
          )}
        </nav>
      </div>

      <div className="flex flex-wrap gap-2.5 border-t border-line-soft bg-elevated p-4">
        {pending ? (
          <Allowed action="review">
            {(ok) => (
              <span className="flex flex-1 gap-2.5">
                <Button variant="primary" size="large" className="flex-1" disabled={!ok} onClick={() => review.approve(d)}>
                  Approve <Kbd>A</Kbd>
                </Button>
                <Button variant="outline" size="large" className="flex-1" disabled={!ok} onClick={() => review.decline(d)}>
                  Decline <Kbd>D</Kbd>
                </Button>
              </span>
            )}
          </Allowed>
        ) : (
          <Button
            variant="outline"
            onClick={() =>
              navigator.clipboard.writeText(d.id).then(
                () => toast("Payment ID copied.", "info"),
                () => toast("Your browser blocked copying.", "review"),
              )
            }
          >
            <Icon name="copy" size={16} className="mr-1.5" /> Copy ID
          </Button>
        )}
      </div>
      {review.dialog}
    </aside>
  );
}

function ChainCard({ title, id, bad, children }: { title: string; id: string; bad?: boolean; children: React.ReactNode }) {
  return (
    <div className={clsx("flex flex-col gap-1 rounded-md border bg-surface px-3.5 py-2.5", bad ? "border-block" : "border-line")}>
      <span className="flex justify-between gap-2">
        <b className="font-semibold">{title} mandate</b>
        <span className="font-mono text-[11.5px] text-fg-muted">{id}</span>
      </span>
      {children}
    </div>
  );
}

function Timing({ label, value, budget, strong }: { label: string; value: number; budget: number; strong?: boolean }) {
  return (
    <>
      <span className={strong ? "font-semibold" : ""}>{label}</span>
      <span className="relative h-1.5 rounded-full bg-neutral-tint" title={`Budget ${budget} ms`}>
        <span className="absolute inset-y-0 left-0 rounded-full bg-brand" style={{ width: `${Math.min(100, (value / budget) * 100)}%` }} />
      </span>
      <span className={clsx("text-right font-mono", strong && "font-semibold")}>{value.toFixed(2)} ms</span>
    </>
  );
}

/** A collapsed detail section. Closed by default so the summary stays short. */
function Fold({ title, count, children }: { title: string; count?: number | string; children: React.ReactNode }) {
  return (
    <details className="group border-t border-line-soft py-1 [&_summary::-webkit-details-marker]:hidden">
      <summary className="flex cursor-pointer list-none items-center gap-2 rounded-md py-2 text-[13.5px] font-semibold hover:text-brand-on-subtle">
        <Icon name="chevronDown" size={15} className="-rotate-90 text-fg-muted transition-transform group-open:rotate-0" />
        {title}
        {count != null && <span className="ml-auto font-mono text-[12px] font-normal text-fg-muted">{count}</span>}
      </summary>
      <div className="pt-1 pb-3 pl-6">{children}</div>
    </details>
  );
}
