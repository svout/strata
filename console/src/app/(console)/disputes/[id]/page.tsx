"use client";

import Link from "next/link";
import { useParams } from "next/navigation";
import Button from "@mui/material/Button";
import { OutcomeChip, Overline } from "@/components/brand";
import { ErrorNote, PageHeader, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { useResource } from "@/lib/useResource";
import { clock, money } from "@/lib/format";

/**
 * Dispute view (P7 phase deliverable, P5 reconstruction): what happened to
 * one payment, readable by a non-engineer. Rendered verbatim from the
 * reconstruction the platform returns; nothing is re-derived here.
 */
export default function DisputePage() {
  const { id } = useParams<{ id: string }>();
  const toast = useToast();
  const { data, error } = useResource(`dispute:${id}`, (a) => a.dispute(id));
  const { data: merchants } = useResource("merchants", (a) => a.merchants());

  if (error) return <div className="p-8"><ErrorNote text={error} /></div>;
  if (data === null) return <div className="p-8"><ErrorNote text="There's no payment with that ID." /></div>;
  if (!data) return <div className="mx-auto flex max-w-[860px] flex-col gap-4 p-8"><Skeleton className="h-12" /><Skeleton className="h-96" /></div>;

  const d = data.decision;
  const merchant = merchants?.find((m) => m.id === d.merchantId)?.name ?? d.merchantId;
  const asText = () =>
    [
      `What happened to payment ${d.id}`,
      d.explanation,
      "",
      ...data.steps.map((st) => `${clock(st.at)}  ${st.title}\n          ${st.detail}`),
      "",
      `Ledger entry #${data.entry.seq} · hash ${data.entry.hash}`,
      data.chain.ok ? `Chain verified (${data.chain.entries} entries).` : `Chain broken at entry #${data.chain.firstBroken}.`,
    ].join("\n");

  return (
    <div className="mx-auto flex max-w-[860px] flex-col gap-5 px-5 py-7 md:px-8">
      <PageHeader
        page="dispute"
        title={`${money(d.amount)} to ${merchant}`}
        crumb={<Link href={`/decisions?id=${d.id}`} className="hover:underline">← Decisions</Link>}
        meta={<Tag tone="neutral" mono size="xs">{d.id}</Tag>}
        actions={
        <Button
          variant="outline"
          onClick={() =>
            navigator.clipboard.writeText(asText()).then(
              () => toast("Reconstruction copied as text.", "info"),
              () => toast("Your browser blocked copying.", "review"),
            )
          }
        >
          <Icon name="copy" size={15} className="mr-1.5" /> Copy as text
        </Button>
        }
      />

      <div className="flex flex-wrap items-center gap-3 rounded-md border border-line p-4">
        <OutcomeChip decision={d} size="md" />
        <p className="flex-1 text-[15px] font-medium">{d.explanation}</p>
      </div>

      <ol className="rounded-md border border-line">
        {data.steps.map((st, i) => (
          <li key={i} className="grid grid-cols-[80px_minmax(0,1fr)] gap-4 border-t border-line-soft px-5 py-3.5 first:border-0">
            <span className="pt-0.5 font-mono text-[12.5px] text-fg-muted">{clock(st.at)}</span>
            <div>
              <p className="font-semibold">{st.title}</p>
              <p className="mt-0.5 text-[13.5px] text-fg-muted">{st.detail}</p>
            </div>
          </li>
        ))}
      </ol>

      <section className="flex flex-col gap-3 rounded-md border border-line p-5">
        <Overline>Proof it hasn&apos;t been changed</Overline>
        {data.chain.ok ? (
          <p className="rounded-md bg-approve-tint p-3.5 text-[14px] text-approve-fg">
            The ledger chain verifies end to end ({data.chain.entries.toLocaleString("en-GB")} entries). Entry #{data.entry.seq} is exactly as it was written.
          </p>
        ) : (
          <p className="rounded-md bg-block-tint p-3.5 text-[14px] text-block-fg">
            The ledger chain is broken at entry #{data.chain.firstBroken}.{" "}
            {data.chain.firstBroken <= data.entry.seq ? "This entry comes after the break, so it can't be proven until the chain is restored." : "The break is after this entry, so this record is still proven."}
          </p>
        )}
        <dl className="grid grid-cols-[130px_minmax(0,1fr)] gap-x-3 gap-y-1.5 text-[12.5px]">
          <dt className="text-fg-muted">Entry</dt>
          <dd className="font-mono">
            <Link href={`/ledger?seq=${data.entry.seq}`} className="text-brand-on-subtle hover:underline">#{data.entry.seq}</Link>
          </dd>
          <dt className="text-fg-muted">Written</dt>
          <dd className="font-mono">{new Date(data.entry.at).toISOString()}</dd>
          <dt className="text-fg-muted">Payload digest</dt>
          <dd className="font-mono break-all">{data.entry.digest}</dd>
          <dt className="text-fg-muted">Previous hash</dt>
          <dd className="font-mono break-all">{data.entry.prevHash}</dd>
          <dt className="text-fg-muted">Entry hash</dt>
          <dd className="font-mono break-all">{data.entry.hash}</dd>
        </dl>
      </section>
    </div>
  );
}
