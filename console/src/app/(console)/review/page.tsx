"use client";

import Link from "next/link";
import { Allowed } from "@/components/ui/Allowed";
import { useEffect, useState } from "react";
import Button from "@mui/material/Button";
import clsx from "clsx";
import { OutcomeChip } from "@/components/brand";
import { useReview } from "@/components/decision/ReviewActions";
import { EmptyState, ErrorNote, PageHeader, Skeleton } from "@/components/ui";
import { Tag, reasonTone } from "@/components/ui/Tag";
import { REASON_LABEL } from "@/api/reasons";
import { useHotkeys } from "@/lib/useHotkeys";
import { useResource } from "@/lib/useResource";
import { ago, money } from "@/lib/format";

/** P5 · Clear flagged payments fast and confidently. One card per payment, reasons first, two big buttons. */
export default function ReviewPage() {
  const { data: queue, error } = useResource("queue", (a) => a.reviewQueue());
  const { data: decided } = useResource("decided", (a) => a.decisions({ since: Date.now() - 7 * 24 * 3600_000 }).then((ds) => ds.filter((d) => d.review && d.review.status !== "pending").sort((x, y) => (y.review && "at" in y.review ? y.review.at : 0) - (x.review && "at" in x.review ? x.review.at : 0)).slice(0, 6)));
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const review = useReview();
  const [sel, setSel] = useState(0);
  const items = queue ?? [];
  useEffect(() => setSel((i) => Math.min(i, Math.max(0, items.length - 1))), [items.length]);

  useHotkeys({
    j: () => setSel((i) => Math.min(items.length - 1, i + 1)),
    k: () => setSel((i) => Math.max(0, i - 1)),
    a: () => items[sel] && review.approve(items[sel]),
    d: () => items[sel] && review.decline(items[sel]),
  });
  const merchantName = (id: string) => merchants?.find((m) => m.id === id)?.name ?? id;

  return (
    <div className="min-h-full bg-surface">
      <div className="mx-auto flex max-w-[1280px] flex-col gap-6 px-5 py-7 md:px-8 xl:flex-row">
        <div className="flex min-w-0 flex-1 flex-col gap-4">
          <PageHeader page="review" meta={queue && queue.length > 0 ? <Tag tone="review">{queue.length} waiting</Tag> : undefined} />
          {error && <ErrorNote text={error} />}
          {!queue ? (
            [0, 1, 2].map((i) => <Skeleton key={i} className="h-[120px] rounded-lg" />)
          ) : queue.length === 0 ? (
            <div className="rounded-lg border border-line bg-elevated">
              <EmptyState
                filled={3}
                title="All clear. Nothing is waiting for you."
                body="Payments that a mandate or the risk score hands back to a person show up here."
                action={
                  <Link href="/decisions" className="text-[14px] font-semibold text-brand-on-subtle">
                    See all decisions →
                  </Link>
                }
              />
            </div>
          ) : (
            queue.map((d, i) => (
              <article
                key={d.id}
                onClick={() => setSel(i)}
                aria-label={`${money(d.amount)} from ${d.agentId}`}
                className={clsx(
                  "flex flex-col gap-5 rounded-lg bg-elevated p-[22px] md:flex-row md:items-center md:gap-6",
                  i === sel ? "outline-2 outline-brand" : "border border-line",
                )}
              >
                <div className="flex shrink-0 flex-col gap-1.5 md:w-[230px]">
                  <span className="font-mono text-[26px] font-medium">{money(d.amount)}</span>
                  <span className="font-mono text-[13px]">{d.agentId}</span>
                  <span className="text-[13px] text-fg-muted">
                    {merchantName(d.merchantId)} · {ago(d.at)}
                  </span>
                </div>
                <div className="flex min-w-0 flex-1 flex-col gap-2">
                  <div className="flex flex-wrap items-center gap-1.5">
                    {d.reasonCodes
                      .filter((c) => c !== "SCORING_DEGRADED")
                      .map((c) => (
                        <Tag key={c} tone={reasonTone(c)}>
                          {REASON_LABEL[c]}
                        </Tag>
                      ))}
                    {d.layers.risk.score != null && (
                      <Tag tone={d.layers.risk.score >= 0.65 ? "review" : "neutral"} mono>
                        risk {d.layers.risk.score.toFixed(2)}
                      </Tag>
                    )}
                  </div>
                  <p className="line-clamp-2 text-[13px] text-fg-muted">{d.explanation}</p>
                  <Link href={`/decisions?id=${d.id}`} className="self-start text-[12.5px] font-semibold text-brand-on-subtle hover:underline" onClick={(e) => e.stopPropagation()}>
                    Full trace →
                  </Link>
                </div>
                <Allowed action="review">
                  {(ok) => (
                    <div className="flex shrink-0 gap-2 md:w-[150px] md:flex-col">
                      <Button variant="primary" size="large" className="flex-1" disabled={!ok} onClick={(e) => { e.stopPropagation(); review.approve(d); }}>
                        Approve
                      </Button>
                      <Button variant="outline" size="large" className="flex-1" disabled={!ok} onClick={(e) => { e.stopPropagation(); review.decline(d); }}>
                        Decline
                      </Button>
                    </div>
                  )}
                </Allowed>
              </article>
            ))
          )}
        </div>

        <aside className="flex shrink-0 flex-col gap-3.5 xl:w-[300px]">
          <div className="flex flex-col gap-2.5 rounded-lg border border-line bg-elevated p-5">
            <h2 className="text-[15px] font-semibold">Recently decided</h2>
            {decided?.length ? (
              decided.map((d) => (
                <Link key={d.id} href={`/decisions?id=${d.id}`} className="flex flex-col gap-1 border-t border-line-soft pt-2.5 text-[13px] first:border-0 first:pt-0">
                  <span className="flex items-center justify-between gap-2">
                    <span className="font-mono">{money(d.amount)}</span>
                    <OutcomeChip decision={d} />
                  </span>
                  <span className="text-fg-muted">
                    {d.review && "by" in d.review ? `${d.review.by} · ${ago(d.review.at)}${d.review.reason ? ` · ${d.review.reason}` : ""}` : ""}
                  </span>
                </Link>
              ))
            ) : (
              <p className="text-[13px] text-fg-muted">No reviews in the last 7 days.</p>
            )}
          </div>

        </aside>
      </div>
      {review.dialog}
    </div>
  );
}
