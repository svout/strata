"use client";

import Link from "next/link";
import clsx from "clsx";
import { Tag } from "@/components/ui/Tag";
import { windowText } from "@/api/mock/engine";
import { Allowed } from "@/components/ui/Allowed";
import { useState } from "react";
import Button from "@mui/material/Button";
import { EmptyState, ErrorNote, Meter, PageHeader, Segmented, Skeleton } from "@/components/ui";
import { useResource } from "@/lib/useResource";
import { mandateSentence } from "@/lib/mandateSentence";
import { moneyShort } from "@/lib/format";

type Tab = "active" | "paused" | "all";

/** P6 · Every spending rule and how much of it is used. Each card leads with the plain-words sentence. */
export default function MandatesPage() {
  const [tab, setTab] = useState<Tab>("active");
  const { data: mandates, error } = useResource("mandates", (a) => a.mandates());
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const list = (mandates ?? []).filter((m) => tab === "all" || m.status === tab);

  return (
    <div className="mx-auto flex max-w-[1280px] flex-col gap-[18px] px-5 py-7 md:px-8">
      <PageHeader
        page="mandates"
        actions={
          <Allowed action="editMandates">
            {(ok) => (
              <Button variant="primary" href={ok ? "/mandates/new" : undefined} disabled={!ok}>
                New mandate
              </Button>
            )}
          </Allowed>
        }
      />
      <Segmented<Tab>
        label="Status"
        value={tab}
        onChange={setTab}
        options={[
          { value: "active", label: "Active", count: mandates?.filter((m) => m.status === "active").length },
          { value: "paused", label: "Paused", count: mandates?.filter((m) => m.status === "paused").length },
          { value: "all", label: "All" },
        ]}
      />
      {error && <ErrorNote text={error} />}
      {!mandates ? (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">{[0, 1, 2].map((i) => <Skeleton key={i} className="h-[210px] rounded-lg" />)}</div>
      ) : list.length === 0 ? (
        <div className="rounded-lg border border-line">
          <EmptyState
            title={tab === "paused" ? "No paused mandates" : "No mandates yet"}
            body="An agent with no active mandate can't spend anything. Add one to let an agent pay for a set of things, within limits you choose."
            filled={0}
            action={<Allowed action="editMandates">{(ok) => <Button variant="primary" href={ok ? "/mandates/new" : undefined} disabled={!ok}>New mandate</Button>}</Allowed>}
          />
        </div>
      ) : (
        <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-3">
          {list.map((m) => {
            const paused = m.status === "paused";
            return (
              <Link
                key={m.id}
                href={`/mandates/${m.id}`}
                title={merchants ? mandateSentence(m, merchants) : undefined}
                className={clsx("flex flex-col gap-3.5 rounded-lg border border-line bg-elevated p-5 hover:border-sec-control", paused && "opacity-70")}
              >
                <span className="flex items-start justify-between gap-2">
                  <span className="flex min-w-0 flex-col gap-0.5">
                    <span className="text-[18px] font-semibold tracking-[-0.01em]">{m.name}</span>
                    <span className="truncate font-mono text-[12px] text-fg-muted">{m.agentId}</span>
                  </span>
                  {paused ? <Tag tone="outline">Paused</Tag> : <Tag tone="control">Active · v{m.version}</Tag>}
                </span>

                <span className="flex flex-col gap-2">
                  <Row label="Buys">
                    {m.categories.map((c) => (
                      <Tag key={c}>{c}</Tag>
                    ))}
                    {m.merchantIds && <Tag tone="outline">{m.merchantIds.length} merchants only</Tag>}
                  </Row>
                  <Row label="Limits">
                    <Tag mono>{moneyShort(m.limits.perPayment)} each</Tag>
                    <Tag mono>{moneyShort(m.limits.perDay)} / day</Tag>
                  </Row>
                  <Row label="Rules">
                    {m.review.enabled ? <Tag tone="review">Review over {moneyShort(m.review.above)}</Tag> : <Tag tone="outline">No review</Tag>}
                    {(m.days.length < 7 || m.hours) && <Tag tone="outline">{windowText(m)}</Tag>}
                  </Row>
                </span>

                <span className="mt-auto flex flex-col gap-1.5 border-t border-line-soft pt-3">
                  <span className="flex justify-between text-[12.5px] text-fg-muted">
                    <span>Used today</span>
                    <span className="font-mono text-fg">{paused ? "—" : `${moneyShort(m.usage.today)} of ${moneyShort(m.limits.perDay)}`}</span>
                  </span>
                  <Meter value={paused ? 0 : m.usage.today.minor} max={m.limits.perDay.minor} label={`${m.name} used today`} />
                </span>
              </Link>
            );
          })}
        </div>
      )}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <span className="grid grid-cols-[52px_minmax(0,1fr)] items-start gap-2">
      <span className="pt-0.5 text-[12px] text-fg-muted">{label}</span>
      <span className="flex flex-wrap gap-1.5">{children}</span>
    </span>
  );
}
