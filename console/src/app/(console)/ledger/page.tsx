"use client";

import Link from "next/link";
import { Allowed } from "@/components/ui/Allowed";
import { Suspense, useEffect, useMemo, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import Drawer from "@mui/material/Drawer";
import clsx from "clsx";
import type { LedgerEntry, LedgerEventType } from "@/api";
import { sha256 } from "@/api/mock/sha256";
import { Overline } from "@/components/brand";
import { Confirm, EmptyState, PageHeader, Segmented, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { api, useResource } from "@/lib/useResource";
import { useSession } from "@/lib/session";
import { IS_SAMPLE_DATA } from "@/api";
import { ago, clock, clockShort, dayMonth, shortHash } from "@/lib/format";

type Group = "all" | "decisions" | "reviews" | "mandates" | "access";
const GROUPS: Record<Exclude<Group, "all">, LedgerEventType[]> = {
  decisions: ["decision.approved", "decision.review", "decision.blocked"],
  reviews: ["review.approved", "review.declined"],
  mandates: ["mandate.created", "mandate.updated", "mandate.paused", "mandate.resumed"],
  access: ["agent.registered", "agent.paused", "agent.resumed", "key.created", "key.revoked", "member.invited", "member.role_changed", "member.removed"],
};
const SPANS = { "24h": 24, "7d": 24 * 7, "30d": 24 * 30, all: 0 } as const;
const EVENT_TONE = (e: LedgerEventType) =>
  e === "decision.blocked" || e === "review.declined" || e === "key.revoked" || e === "agent.paused"
    ? ("block" as const)
    : e === "decision.review"
      ? ("review" as const)
      : e === "decision.approved" || e === "review.approved"
        ? ("approve" as const)
        : e.startsWith("mandate.")
          ? ("control" as const)
          : ("neutral" as const);
const EVENT_COLOUR = (e: LedgerEventType) =>
  e === "decision.blocked" || e === "review.declined" || e === "key.revoked" || e === "agent.paused"
    ? "text-block-fg"
    : e === "decision.review"
      ? "text-review-fg"
      : e === "decision.approved" || e === "review.approved"
        ? "text-approve-fg"
        : e.startsWith("mandate.")
          ? "text-brand-on-subtle"
          : "text-fg";

export default function LedgerPage() {
  return (
    <Suspense fallback={null}>
      <Ledger />
    </Suspense>
  );
}

/** P9 · Prove what happened: one append-only, hash-chained log of everything. */
function Ledger() {
  const params = useSearchParams();
  const router = useRouter();
  const path = usePathname();
  const s = useSession();
  const toast = useToast();
  const [group, setGroup] = useState<Group>("all");
  const [span, setSpan] = useState<keyof typeof SPANS>("24h");
  const [shown, setShown] = useState(200);
  const [checking, setChecking] = useState(false);
  const [confirmTamper, setConfirmTamper] = useState(false);
  const openSeq = Number(params.get("seq")) || null;

  // Opening an older entry by link widens the window so the row is visible
  useEffect(() => {
    if (openSeq) setSpan("all");
  }, [openSeq]);

  const since = SPANS[span] ? Date.now() - SPANS[span] * 3600_000 : undefined;
  const { data: entries } = useResource(`ledger:${group}:${span}`, (a) => a.ledger({ events: group === "all" ? undefined : GROUPS[group], since }));
  const { data: chain } = useResource("chain", (a) => a.lastVerification());
  const open = useMemo(() => entries?.find((e) => e.seq === openSeq) ?? null, [entries, openSeq]);
  const setOpen = (seq: number | null) => router.replace(seq ? `${path}?seq=${seq}` : path, { scroll: false });

  const verify = async () => {
    setChecking(true);
    const r = await api().verifyChain();
    setChecking(false);
    toast(r.ok ? `Chain verified: ${r.entries.toLocaleString("en-GB")} entries.` : `Chain broken at entry #${r.firstBroken}.`, r.ok ? "approve" : "block");
  };

  const exportCsv = async () => {
    const all = await api().ledger({});
    const head = "seq,time,event,subject,actor,payload_digest,prev_hash,hash";
    const lines = [...all].reverse().map((e) => [e.seq, new Date(e.at).toISOString(), e.event, `"${e.subject.replace(/"/g, '""')}"`, e.actor, e.digest, e.prevHash, e.hash].join(","));
    const url = URL.createObjectURL(new Blob([[head, ...lines].join("\n")], { type: "text/csv" }));
    const a = document.createElement("a");
    a.href = url;
    a.download = `strata-ledger-${new Date().toISOString().slice(0, 10)}.csv`;
    a.click();
    URL.revokeObjectURL(url);
  };

  return (
    <div className="mx-auto flex max-w-[1320px] flex-col gap-4 px-5 py-7 md:px-8">
      <PageHeader
        page="ledger"
        meta={
          chain &&
          (chain.ok ? (
            <Tag tone="approve" title={`Checked ${clockShort(chain.checkedAt)}`}>
              <Icon name="check" size={12} /> Verified · {chain.entries.toLocaleString("en-GB")} entries
            </Tag>
          ) : (
            <button type="button" onClick={() => setOpen(chain.firstBroken)}>
              <Tag tone="block"><Icon name="close" size={12} /> Broken at #{chain.firstBroken}</Tag>
            </button>
          ))
        }
        actions={
          <>
            <Button variant="outline" size="small" onClick={verify} disabled={checking}>
              {checking ? "Checking…" : "Verify again"}
            </Button>
            <Button variant="ink" size="small" onClick={exportCsv}>
              Export CSV
            </Button>
          </>
        }
      />

      {chain && !chain.ok && (
        <div role="alert" className="flex flex-wrap items-center gap-3 rounded-md bg-block-tint p-4 text-[14px] text-block-fg">
          <span className="flex-1">
            <b>Entry #{chain.firstBroken} was changed after it was recorded.</b> Entries after it are unproven.
          </span>
          {IS_SAMPLE_DATA && s.can("runDemo") && (
            <Button variant="outline" size="small" onClick={() => api().restoreTamperedEntry().then(() => toast("Entry restored. The chain verifies again."))}>
              Restore entry (demo)
            </Button>
          )}
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2">
        <Segmented<Group>
          label="Event type"
          value={group}
          onChange={setGroup}
          options={[
            { value: "all", label: "All events" },
            { value: "decisions", label: "Decisions" },
            { value: "reviews", label: "Reviews" },
            { value: "mandates", label: "Mandate changes" },
            { value: "access", label: "Agents, keys & members" },
          ]}
        />
        <label htmlFor="l-span" className="sr-only">Time range</label>
        <select id="l-span" className="field ml-auto !h-[34px] !w-auto text-[13px]" value={span} onChange={(e) => setSpan(e.target.value as keyof typeof SPANS)}>
          <option value="24h">Last 24 hours</option>
          <option value="7d">Last 7 days</option>
          <option value="30d">Last 30 days</option>
          <option value="all">Everything</option>
        </select>
      </div>

      <div className="overflow-x-auto rounded-md border border-line">
        {!entries ? (
          <div className="flex flex-col gap-2 p-4">{Array.from({ length: 10 }, (_, i) => <Skeleton key={i} className="h-8" />)}</div>
        ) : entries.length === 0 ? (
          <EmptyState title="Nothing recorded in this window" body="Pick a longer time range or another event type." filled={0} />
        ) : (
          <table className="data-table font-mono">
            <thead>
              <tr>
                <th>#</th>
                <th>Time</th>
                <th>Event</th>
                <th>Subject</th>
                <th>Actor</th>
                <th>Hash</th>
                <th>Prev</th>
              </tr>
            </thead>
            <tbody>
              {entries.slice(0, shown).map((e) => {
                const broken = chain && !chain.ok && e.seq === chain.firstBroken;
                const after = chain && !chain.ok && e.seq > chain.firstBroken;
                return (
                  <tr key={e.seq} data-href aria-selected={e.seq === openSeq} onClick={() => setOpen(e.seq)} className={clsx("text-[12.5px]", broken && "[&>td]:!bg-block-tint", after && "opacity-60")}>
                    <td className="text-fg-muted">{e.seq}</td>
                    <td className="text-fg-muted">{span === "24h" ? clock(e.at) : `${dayMonth(e.at)} ${clockShort(e.at)}`}</td>
                    <td><Tag tone={EVENT_TONE(e.event)} mono size="xs">{e.event}</Tag></td>
                    <td className="max-w-[340px] truncate font-sans text-[13px]">{e.subject}</td>
                    <td className="text-fg-muted">{e.actor}</td>
                    <td className="text-brand-on-subtle">{shortHash(e.hash)}{broken && " ✕"}</td>
                    <td className="text-fg-subtle">{shortHash(e.prevHash)}</td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        )}
        {entries && entries.length > shown && (
          <div className="border-t border-line-soft p-3 text-center">
            <Button variant="outline" size="small" onClick={() => setShown((n) => n + 200)}>
              Show more ({(entries.length - shown).toLocaleString("en-GB")} older)
            </Button>
          </div>
        )}
      </div>

      {IS_SAMPLE_DATA && chain?.ok && (
        <div className="flex flex-wrap items-center gap-3 rounded-md border border-dashed border-mist p-4 text-[13px] text-fg-muted">
          <span className="flex-1">
            <Tag tone="demo" size="xs">Demo</Tag> <span className="ml-1.5">Edit an old entry behind Strata&apos;s back, then press Verify again.</span>
          </span>
          <Allowed action="runDemo">
            {(ok) => (
              <Button variant="outline" size="small" disabled={!ok} onClick={() => setConfirmTamper(true)}>
                Edit an entry
              </Button>
            )}
          </Allowed>
        </div>
      )}

      <Drawer anchor="right" open={!!open} onClose={() => setOpen(null)} slotProps={{ paper: { sx: { width: "min(100vw, 520px)" } } }}>
        {open && <EntryDetail entry={open} onClose={() => setOpen(null)} />}
      </Drawer>
      <Confirm
        open={confirmTamper}
        onClose={() => setConfirmTamper(false)}
        title="Edit a ledger entry behind Strata's back?"
        body="This changes one old approved payment's amount in place, the way a quiet database UPDATE would. Nothing warns you until someone verifies the chain."
        confirm="Edit the entry"
        danger
        onConfirm={async () => {
          const seq = await api().tamperWithEntry();
          toast(`Entry #${seq} was edited. The badge still says verified until someone checks.`, "review", { label: "Verify now", run: verify });
        }}
      />
    </div>
  );
}

/** The raw record, with its digest recomputed in front of the reader. */
function EntryDetail({ entry: e, onClose }: { entry: LedgerEntry; onClose: () => void }) {
  const recomputed = sha256(e.payload);
  const matches = recomputed === e.digest;
  let pretty = e.payload;
  try {
    pretty = JSON.stringify(JSON.parse(e.payload), null, 2);
  } catch {
    /* show as stored */
  }
  return (
    <div className="flex h-full flex-col">
      <div className="flex items-start justify-between gap-3 border-b border-line-soft p-6">
        <div className="flex flex-col gap-1">
          <span className="font-mono text-[12px] text-fg-muted">
            Entry #{e.seq} · {dayMonth(e.at)} {clock(e.at)} · {ago(e.at)}
          </span>
          <span className={clsx("font-mono text-[20px] font-medium", EVENT_COLOUR(e.event))}>{e.event}</span>
          <span className="text-[14px]">
            {e.subject} · by {e.actor}
          </span>
        </div>
        <button type="button" aria-label="Close" onClick={onClose} className="grid size-9 shrink-0 place-items-center rounded-full bg-surface">
          <Icon name="close" size={16} />
        </button>
      </div>
      <div className="flex min-h-0 flex-1 flex-col gap-5 overflow-y-auto p-6">
        <div className={clsx("rounded-md p-3.5 text-[13.5px]", matches ? "bg-approve-tint text-approve-fg" : "bg-block-tint text-block-fg")}>
          {matches ? "The payload still matches the digest written when this entry was recorded." : "The payload no longer matches its digest: this entry was changed after it was written."}
        </div>
        <section className="flex flex-col gap-2">
          <Overline>Chain</Overline>
          <dl className="grid grid-cols-[124px_minmax(0,1fr)] gap-x-3 gap-y-1.5 font-mono text-[12px] break-all">
            <dt className="font-sans text-fg-muted">Previous hash</dt>
            <dd>{e.prevHash}</dd>
            <dt className="font-sans text-fg-muted">Stored digest</dt>
            <dd>{e.digest}</dd>
            <dt className="font-sans text-fg-muted">Digest now</dt>
            <dd className={matches ? "" : "text-block-fg"}>{recomputed}</dd>
            <dt className="font-sans text-fg-muted">Entry hash</dt>
            <dd>{e.hash}</dd>
          </dl>
          <p className="text-[12px] text-fg-muted">digest = SHA-256(payload) · hash = SHA-256(previous hash ‖ digest)</p>
        </section>
        <section className="flex flex-col gap-2">
          <Overline>Payload (canonical JSON)</Overline>
          <pre className="overflow-x-auto rounded-md bg-ink p-4 font-mono text-[12px] leading-relaxed text-[#C9CBD1]">{pretty}</pre>
        </section>
        {e.ref.kind === "decision" && (
          <div className="flex flex-wrap gap-2">
            <Button variant="outline" size="small" href={`/decisions?id=${e.ref.id}`}>Open the decision</Button>
            <Button variant="ghost" size="small" href={`/disputes/${e.ref.id}`}>Dispute view</Button>
          </div>
        )}
        {e.ref.kind === "mandate" && <Link className="text-[13px] font-semibold text-brand-on-subtle" href={`/mandates/${e.ref.id}`}>Open the mandate →</Link>}
      </div>
    </div>
  );
}
