"use client";

import Link from "next/link";
import { Tag, reasonTone } from "@/components/ui/Tag";
import { useState } from "react";
import { useRouter } from "next/navigation";
import clsx from "clsx";
import type { OverviewRange } from "@/api";
import { SignalDot } from "@/components/brand";
import { Card, CardTitle, ErrorNote, Meter, PageHeader, RangeTabs, Skeleton } from "@/components/ui";
import { useResource } from "@/lib/useResource";
import { ago, money, moneyShort } from "@/lib/format";
import { OutcomeBars, RankBars, Spark } from "@/components/charts";

/** P3 · "Is anything wrong right now?" in five seconds. Reviews waiting are the loudest thing on the page. */
export default function OverviewPage() {
  const [range, setRange] = useState<OverviewRange>("today");
  const router = useRouter();
  const { data, error } = useResource(`overview:${range}`, (a) => a.overview(range));
  const { data: mandates } = useResource("mandates", (a) => a.mandates());
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const { data: chain } = useResource("chain", (a) => a.lastVerification());
  const merchantName = (id: string) => merchants?.find((m) => m.id === id)?.name ?? id;

  return (
    <div className="mx-auto flex max-w-[1280px] flex-col gap-5 px-5 py-7 md:px-8">
      <PageHeader
        page="overview"
        actions={
          <RangeTabs
            label="Time range"
            value={range}
            onChange={setRange}
            options={[
              { value: "today", label: "Today" },
              { value: "7d", label: "7 days" },
              { value: "30d", label: "30 days" },
            ]}
          />
        }
      />
      {error && <ErrorNote text={error} />}
      {chain && !chain.ok && (
        <Link href="/ledger" className="flex items-center gap-3 rounded-md bg-block-tint p-4 text-[15px] font-semibold text-block-fg">
          <SignalDot tone="block" /> The ledger chain is broken at entry #{chain.firstBroken}. Open the ledger →
        </Link>
      )}

      <div className="grid grid-cols-2 gap-3.5 lg:grid-cols-4">
        <Tile label="Payments checked" value={data?.checked} spark={data && { data: data.trend.map((t) => t.checked), labels: data.trend.map((t) => t.day), tone: "muted" }} href="/decisions" />
        <Tile
          label="Approved"
          dot="approve"
          value={data?.approved}
          sub={data ? `${money(data.paidOut)} paid out` : undefined}
          spark={data && { data: data.trend.map((t) => t.approved), labels: data.trend.map((t) => t.day), tone: "approved" }}
          href="/decisions?outcome=approved"
        />
        <Link
          href="/review"
          className={clsx(
            "flex flex-col gap-1.5 rounded-md p-[18px]",
            data && data.waiting > 0 ? "border border-review shadow-[inset_0_3px_0_var(--color-review)]" : "border border-line hover:border-fg-muted",
          )}
        >
          <span className={clsx("flex items-center gap-1.5 text-[13px]", data?.waiting ? "text-review-fg" : "text-fg-muted")}>
            <SignalDot tone="review" />
            Waiting for review →
          </span>
          {data ? <span className="font-mono text-[30px] font-medium">{data.waiting}</span> : <Skeleton className="h-9 w-16" />}
          <span className="text-[12.5px] text-fg-muted">{data?.waiting ? "Needs a person" : "All clear"}</span>
        </Link>
        <Tile
          label="Blocked"
          dot="block"
          value={data?.blocked}
          sub="No money moved"
          spark={data && { data: data.trend.map((t) => t.blocked), labels: data.trend.map((t) => t.day), tone: "block" }}
          href="/decisions?outcome=blocked"
        />
      </div>

      <div className="grid gap-3.5 xl:grid-cols-[1.6fr_1fr]">
        <Card className="flex flex-col gap-3.5 p-[18px]">
          <CardTitle
            action={
              <span className="flex gap-3 text-[12px] text-fg-muted">
                <span className="flex items-center gap-1.5"><i className="size-2 rounded-full bg-cobalt-300" /> approved</span>
                <span className="flex items-center gap-1.5 text-review-fg"><i className="size-2 rounded-full bg-review" /> review</span>
                <span className="flex items-center gap-1.5 text-block-fg"><i className="size-2 rounded-full bg-block" /> blocked</span>
              </span>
            }
          >
            Decisions by {range === "today" ? "hour" : "day"}
          </CardTitle>
          {data ? (
            <>
              <OutcomeBars
                buckets={data.byBucket}
                label={`Decisions by ${range === "today" ? "hour" : "day"}: approved, review and blocked`}
                onSelect={(i) => {
                  const b = data.byBucket[i];
                  router.push(`/decisions?since=${b.start}&until=${b.end}`);
                }}
              />

            </>
          ) : (
            <Skeleton className="h-[240px]" />
          )}
        </Card>

        <div className="flex flex-col gap-3.5">
          <Card className="flex flex-col gap-3 p-[18px]">
            <CardTitle action={<Link href="/mandates" className="text-[13px] font-semibold text-brand-on-subtle">All mandates →</Link>}>Mandate usage today</CardTitle>
            {mandates
              ? mandates
                  .filter((m) => m.status === "active")
                  .map((m) => (
                    <Link key={m.id} href={`/mandates/${m.id}`} className="flex flex-col gap-1 rounded-md">
                      <span className="flex justify-between gap-3 text-[13px]">
                        <span className="truncate">{m.name}</span>
                        <span className="shrink-0 font-mono">
                          {moneyShort(m.usage.today)} / {moneyShort(m.limits.perDay)}
                        </span>
                      </span>
                      <Meter value={m.usage.today.minor} max={m.limits.perDay.minor} label={`${m.name} used today`} />
                    </Link>
                  ))
              : <Skeleton className="h-28" />}
          </Card>

          <Card className="flex flex-1 flex-col gap-2.5 p-[18px]">
            <CardTitle action={<Link href="/decisions?outcome=blocked" className="text-[13px] font-semibold text-brand-on-subtle">All decisions →</Link>}>Recently blocked</CardTitle>
            {data?.recentlyBlocked.map((d) => (
              <Link key={d.id} href={`/decisions?id=${d.id}`} className="flex items-center justify-between gap-3 rounded-md text-[13px] hover:bg-surface">
                <span className="flex min-w-0 flex-col">
                  <span className="truncate font-mono">{d.agentId}</span>
                  <span className="truncate text-[12px] text-fg-muted">
                    {merchantName(d.merchantId)} · {ago(d.at)}
                  </span>
                </span>
                <Tag tone={reasonTone(d.reasonCodes[0])} size="xs">
                  {d.controlFired}
                </Tag>
              </Link>
            ))}
          </Card>
        </div>
      </div>

      {data && data.topControls.length > 0 && (
        <Card className="flex flex-col gap-3 p-[18px]">
          <CardTitle>What stopped or held payments</CardTitle>
          <RankBars
            label="Number of payments each control stopped or held"
            items={data.topControls.map((c) => ({ label: c.label, value: c.count }))}
            onSelect={(i) => router.push(`/decisions?reason=${data.topControls[i].code}`)}
          />
        </Card>
      )}
    </div>
  );
}

function Tile({ label, value, sub, dot, href, spark }: { label: string; value?: number; sub?: string; dot?: "approve" | "block"; href?: string; spark?: { data: number[]; labels: string[]; tone: "approved" | "block" | "muted" } | undefined }) {
  const body = (
    <>
      <span className="flex items-center gap-1.5 text-[13px] text-fg-muted">
        {dot && <SignalDot tone={dot} />}
        {label}
      </span>
      <span className="flex items-end justify-between gap-3">
        {value == null ? <Skeleton className="h-9 w-24" /> : <span className="font-mono text-[30px] leading-none font-medium">{value.toLocaleString("en-GB")}</span>}
        {spark && (
          <span className="w-[92px] shrink-0">
            <Spark data={spark.data} labels={spark.labels} tone={spark.tone} label={label} />
          </span>
        )}
      </span>
      {sub && <span className="text-[12.5px] text-fg-muted">{sub}</span>}
      {spark && <span className="sr-only">Small chart: the last 7 days.</span>}
    </>
  );
  const cls = "flex flex-col gap-1.5 rounded-md border border-line p-[18px]";
  return href ? (
    <Link href={href} className={clsx(cls, "hover:border-fg-muted")}>
      {body}
    </Link>
  ) : (
    <div className={cls}>{body}</div>
  );
}
