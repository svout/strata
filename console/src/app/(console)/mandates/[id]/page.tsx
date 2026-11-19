"use client";

import Link from "next/link";
import { Allowed } from "@/components/ui/Allowed";
import { Suspense, useEffect, useMemo, useState } from "react";
import { useParams, useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import Menu from "@mui/material/Menu";
import MenuItem from "@mui/material/MenuItem";
import Switch from "@mui/material/Switch";
import clsx from "clsx";
import type { Category, Mandate, MandateInput, ReplayResult, Weekday } from "@/api";
import { CATEGORIES } from "@/api/mock/catalogue";
import { SignalDot } from "@/components/brand";
import { Confirm, ErrorNote, PageHeader, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { api, useResource } from "@/lib/useResource";
import { useSession } from "@/lib/session";
import { mandateSentence } from "@/lib/mandateSentence";
import { dayMonth, gbp, money, moneyShort } from "@/lib/format";
import { CompareBars } from "@/components/charts";

const DAYS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];
const HOURS = Array.from({ length: 25 }, (_, h) => `${String(h).padStart(2, "0")}:00`);

/** Form state: amounts as typed (pounds), converted to pence on save. */
type Draft = Omit<MandateInput, "limits" | "review"> & { perPayment: string; perDay: string; perMonth: string; reviewOn: boolean; reviewAbove: string };

const toDraft = (m: MandateInput): Draft => ({
  name: m.name,
  agentId: m.agentId,
  categories: m.categories,
  merchantIds: m.merchantIds,
  days: m.days,
  hours: m.hours,
  perPayment: String(m.limits.perPayment.minor / 100),
  perDay: String(m.limits.perDay.minor / 100),
  perMonth: String(m.limits.perMonth.minor / 100),
  reviewOn: m.review.enabled,
  reviewAbove: String(m.review.above.minor / 100),
});
const pounds = (s: string) => (s.trim() === "" || isNaN(Number(s.replace(/[£,]/g, ""))) ? NaN : Number(s.replace(/[£,]/g, "")));
const toInput = (d: Draft): MandateInput => ({
  name: d.name.trim(),
  agentId: d.agentId,
  categories: d.categories,
  merchantIds: d.merchantIds,
  days: [...d.days].sort() as Weekday[],
  hours: d.hours,
  limits: { perPayment: gbp(pounds(d.perPayment) || 0), perDay: gbp(pounds(d.perDay) || 0), perMonth: gbp(pounds(d.perMonth) || 0) },
  review: { enabled: d.reviewOn, above: gbp(pounds(d.reviewAbove) || 0) },
});

function validate(d: Draft, takenNames: string[]) {
  const e: Partial<Record<"name" | "categories" | "merchants" | "perPayment" | "perDay" | "perMonth" | "days" | "hours" | "review", string>> = {};
  const pp = pounds(d.perPayment), pd = pounds(d.perDay), pm = pounds(d.perMonth), ra = pounds(d.reviewAbove);
  if (!d.name.trim()) e.name = "Give it a name people will recognise, like “Travel & accommodation”.";
  else if (takenNames.includes(d.name.trim().toLowerCase())) e.name = "Another mandate already uses this name.";
  if (!d.categories.length) e.categories = "Pick at least one category.";
  if (d.merchantIds && !d.merchantIds.length) e.merchants = "Pick at least one merchant, or allow any merchant in these categories.";
  if (!(pp > 0)) e.perPayment = "Enter an amount above £0.";
  if (!(pd > 0)) e.perDay = "Enter an amount above £0.";
  else if (pp > pd) e.perDay = "Can't be lower than the per-payment cap.";
  if (!(pm > 0)) e.perMonth = "Enter an amount above £0.";
  else if (pd > pm) e.perMonth = "Can't be lower than the daily limit.";
  if (!d.days.length) e.days = "Pick at least one day.";
  if (d.hours && d.hours.from >= d.hours.to) e.hours = "The start time must be before the end time.";
  if (d.reviewOn && !(ra >= 0)) e.review = "Enter £0 or more.";
  else if (d.reviewOn && ra >= pp) e.review = `Set this below the ${moneyShort(gbp(pp || 0))} cap, or nothing would ever be reviewed.`;
  return e;
}

export default function EditorPage() {
  return (
    <Suspense fallback={null}>
      <Editor />
    </Suspense>
  );
}

/** P7 · Write a rule without reading docs: five plain questions, live summary, impact check. */
function Editor() {
  const { id: rawId } = useParams<{ id: string }>();
  const isNew = rawId === "new";
  const params = useSearchParams();
  const router = useRouter();
  const toast = useToast();
  const s = useSession();
  const editable = s.can("editMandates");

  const { data: existing } = useResource(`mandate:${rawId}`, (a) => (isNew ? Promise.resolve(null) : a.mandate(rawId)));
  const { data: history } = useResource(`history:${rawId}`, (a) => (isNew ? Promise.resolve([]) : a.mandateHistory(rawId)));
  const { data: agents } = useResource("agents", (a) => a.agents());
  const { data: merchants } = useResource("merchants", (a) => a.merchants());
  const { data: all } = useResource("mandates", (a) => a.mandates());

  const [draft, setDraft] = useState<Draft | null>(null);
  const [touched, setTouched] = useState(false);
  const [replay, setReplay] = useState<ReplayResult | null>(null);
  const [catMenu, setCatMenu] = useState<HTMLElement | null>(null);
  const [confirmPause, setConfirmPause] = useState(false);
  const [saving, setSaving] = useState(false);

  // Initialise once the data arrives
  useEffect(() => {
    if (draft) return;
    if (isNew && agents) {
      const agentId = params.get("agent") ?? agents.find((a) => a.status === "active")?.id ?? agents[0]?.id ?? "";
      setDraft(toDraft({ name: "", agentId, categories: [], merchantIds: null, days: [0, 1, 2, 3, 4, 5, 6], hours: null, limits: { perPayment: gbp(250), perDay: gbp(1000), perMonth: gbp(5000) }, review: { enabled: true, above: gbp(200) } }));
    } else if (existing) setDraft(toDraft(existing));
  }, [isNew, agents, existing, draft, params]);

  const input = useMemo(() => (draft ? toInput(draft) : null), [draft]);
  const errors = useMemo(
    () => (draft ? validate(draft, (all ?? []).filter((m) => m.id !== rawId).map((m) => m.name.toLowerCase())) : {}),
    [draft, all, rawId],
  );
  const valid = Object.keys(errors).length === 0;

  // Re-run the impact check a moment after each change
  useEffect(() => {
    if (!input || !valid) return;
    const t = setTimeout(() => api().replayMandate(isNew ? null : rawId, input, 30).then(setReplay), 350);
    return () => clearTimeout(t);
  }, [input, valid, isNew, rawId]);

  if (!isNew && existing === null) return <div className="p-8"><ErrorNote text="That mandate doesn't exist. It may have been removed." /></div>;
  if (!draft || !merchants || !agents) return <div className="flex flex-col gap-4 p-8"><Skeleton className="h-10 w-80" /><Skeleton className="h-[520px]" /></div>;

  const set = (p: Partial<Draft>) => {
    setTouched(true);
    setDraft((d) => (d ? { ...d, ...p } : d));
  };
  const show = (k: keyof typeof errors) => (touched || !isNew ? errors[k] : undefined);
  const inCats = merchants.filter((m) => draft.categories.includes(m.category));
  const reviewer = "a person";

  const save = async () => {
    if (!input || !valid) {
      setTouched(true);
      return;
    }
    setSaving(true);
    try {
      const m = await api().saveMandate(isNew ? null : rawId, input, s.user.name);
      toast(isNew ? `Created "${m.name}". It applies from the next payment.` : `Saved "${m.name}" as version ${m.version}. It applies from the next payment.`);
      router.push("/mandates");
    } catch (e) {
      toast(e instanceof Error ? e.message : "Couldn't save.", "block");
      setSaving(false);
    }
  };

  const togglePause = async (m: Mandate) => {
    const next = m.status === "active" ? "paused" : "active";
    await api().setMandateStatus(m.id, next, s.user.name);
    toast(next === "paused" ? `"${m.name}" is paused. ${m.agentId} can't spend under it until it's resumed.` : `"${m.name}" is active again.`, next === "paused" ? "review" : "approve");
  };

  return (
    <div className="min-h-full bg-surface">
      <div className="mx-auto flex max-w-[1320px] flex-col gap-4 px-5 py-6 md:px-8">
        <PageHeader
          page="mandate"
          title={isNew ? "New mandate" : existing?.name}
          crumb={<Link href="/mandates" className="hover:underline">← Mandates</Link>}
          meta={
            <>
              {existing && (existing.status === "paused" ? <Tag tone="outline">Paused</Tag> : <Tag tone="control">Active · v{existing.version}</Tag>)}
              {!editable && <Tag tone="neutral" title="Owners, admins and members can edit">Read only</Tag>}
            </>
          }
          actions={
          <div className="flex flex-wrap gap-2.5">
            {existing && (
              <Allowed action="editMandates">
                {(ok) => (
                  <Button variant="ghost" disabled={!ok} onClick={() => (existing.status === "active" ? setConfirmPause(true) : togglePause(existing))}>
                    <Icon name={existing.status === "active" ? "pause" : "play"} size={15} className="mr-1.5" />
                    {existing.status === "active" ? "Pause" : "Resume"}
                  </Button>
                )}
              </Allowed>
            )}
            <Button variant="outline" href="/mandates">Cancel</Button>
            <Allowed action="editMandates">
              {(ok) => (
                <Button variant="primary" onClick={save} disabled={!ok || saving || (touched && !valid)}>
                  {isNew ? "Create mandate" : `Save as v${(existing?.version ?? 0) + 1}`}
                </Button>
              )}
            </Allowed>
          </div>
          }
        />

        <div className="grid gap-5 xl:grid-cols-[minmax(0,1fr)_360px]">
          <fieldset disabled={!editable} className="flex flex-col rounded-lg border border-line bg-elevated px-6 py-2">
            <Row n={1} title="Which agent?">
              <div className="grid gap-3 md:grid-cols-2">
                <div className="flex flex-col gap-1.5">
                  <label htmlFor="m-agent" className="text-[12px] text-fg-muted">Agent</label>
                  <select id="m-agent" className="field font-mono" value={draft.agentId} disabled={!isNew} onChange={(e) => set({ agentId: e.target.value })}>
                    {agents.map((a) => <option key={a.id} value={a.id}>{a.id}{a.status === "paused" ? " (paused)" : ""}</option>)}
                  </select>
                </div>
                <div className="flex flex-col gap-1.5">
                  <label htmlFor="m-name" className="text-[12px] text-fg-muted">Mandate name</label>
                  <input id="m-name" className="field" value={draft.name} aria-invalid={!!show("name")} placeholder="Travel & accommodation" onChange={(e) => set({ name: e.target.value })} />
                </div>
              </div>
              <Err text={show("name")} />
            </Row>

            <Row n={2} title="What & where?">
              <div className="flex flex-wrap gap-2 text-[13px]">
                {draft.categories.map((c) => (
                  <button key={c} type="button" onClick={() => set({ categories: draft.categories.filter((x) => x !== c), merchantIds: draft.merchantIds?.filter((id) => merchants.find((m) => m.id === id)?.category !== c) ?? null })} className="rounded-md border border-brand bg-brand-faint px-2.5 py-1 font-medium text-brand-on-subtle hover:bg-brand-subtle" aria-label={`Remove ${c}`}>
                    {cap(c)} ✕
                  </button>
                ))}
                <button type="button" onClick={(e) => setCatMenu(e.currentTarget)} className="rounded-full border-[1.5px] border-dashed border-mist px-3 py-1.5 text-fg-muted hover:text-fg">
                  + Add category
                </button>
                <Menu anchorEl={catMenu} open={!!catMenu} onClose={() => setCatMenu(null)}>
                  {CATEGORIES.filter((c) => !draft.categories.includes(c)).map((c) => (
                    <MenuItem key={c} onClick={() => { set({ categories: [...draft.categories, c as Category] }); setCatMenu(null); }}>{cap(c)}</MenuItem>
                  ))}
                </Menu>
              </div>
              <Err text={show("categories")} />
              <div className="flex flex-wrap items-center gap-2 text-[13px]">
                <button type="button" onClick={() => set({ merchantIds: null })} className={clsx("rounded-md border px-2.5 py-1", draft.merchantIds === null ? "border-brand bg-brand-faint text-brand-on-subtle" : "border-line bg-elevated hover:bg-surface")}>
                  Any merchant in these categories
                </button>
                <button type="button" onClick={() => set({ merchantIds: inCats.map((m) => m.id) })} className={clsx("rounded-md border px-2.5 py-1", draft.merchantIds !== null ? "border-brand bg-brand-faint text-brand-on-subtle" : "border-line bg-elevated hover:bg-surface")}>
                  Only specific merchants
                </button>
              </div>
              {draft.merchantIds !== null && (
                <div className="flex flex-col gap-1.5">
                  {inCats.length === 0 && <p className="text-[13px] text-fg-muted">Add a category to see its merchants.</p>}
                  {inCats.map((m) => (
                    <label key={m.id} className="flex items-center gap-2.5 text-[13.5px]">
                      <input type="checkbox" className="size-4 accent-[var(--color-brand)]" checked={draft.merchantIds!.includes(m.id)} onChange={(e) => set({ merchantIds: e.target.checked ? [...draft.merchantIds!, m.id] : draft.merchantIds!.filter((x) => x !== m.id) })} />
                      {m.name} <span className="font-mono text-[11.5px] text-fg-muted">{m.host}</span>
                    </label>
                  ))}
                  <Err text={show("merchants")} />
                </div>
              )}
            </Row>

            <Row n={3} title="How much?">
              <div className="grid gap-3 sm:grid-cols-3">
                <Money label="Per payment" value={draft.perPayment} error={show("perPayment")} onChange={(v) => set({ perPayment: v })} />
                <Money label="Per day" value={draft.perDay} error={show("perDay")} onChange={(v) => set({ perDay: v })} />
                <Money label="Per month" value={draft.perMonth} error={show("perMonth")} onChange={(v) => set({ perMonth: v })} />
              </div>
            </Row>

            <Row n={4} title="When?">
              <div className="flex flex-wrap items-center gap-1.5 text-[13px]" role="group" aria-label="Days">
                {DAYS.map((name, i) => {
                  const on = draft.days.includes(i as Weekday);
                  return (
                    <button key={name} type="button" aria-pressed={on} onClick={() => set({ days: on ? draft.days.filter((d) => d !== i) : ([...draft.days, i] as Weekday[]) })} className={clsx("rounded-md border px-2.5 py-1", on ? "border-brand bg-brand-faint text-brand-on-subtle" : "border-line bg-elevated text-fg-muted hover:bg-surface")}>
                      {name}
                    </button>
                  );
                })}
                <label htmlFor="m-time" className="sr-only">Time of day</label>
                <select id="m-time" className="field ml-2 !h-[34px] !w-auto text-[13px]" value={draft.hours ? "window" : "any"} onChange={(e) => set({ hours: e.target.value === "any" ? null : { from: "08:00", to: "19:00" } })}>
                  <option value="any">Any time</option>
                  <option value="window">Between set hours</option>
                </select>
                {draft.hours && (
                  <span className="flex items-center gap-1.5">
                    <label className="sr-only" htmlFor="m-from">From</label>
                    <select id="m-from" className="field !h-[34px] !w-auto font-mono text-[13px]" value={draft.hours.from} onChange={(e) => set({ hours: { ...draft.hours!, from: e.target.value } })}>
                      {HOURS.slice(0, 24).map((h) => <option key={h}>{h}</option>)}
                    </select>
                    –
                    <label className="sr-only" htmlFor="m-to">Until</label>
                    <select id="m-to" className="field !h-[34px] !w-auto font-mono text-[13px]" value={draft.hours.to} onChange={(e) => set({ hours: { ...draft.hours!, to: e.target.value } })}>
                      {HOURS.slice(1).map((h) => <option key={h}>{h}</option>)}
                    </select>
                  </span>
                )}
              </div>
              <Err text={show("days") ?? show("hours")} />
            </Row>

            <Row n={5} title="Ask a human?" last>
              <div className="flex flex-wrap items-center gap-2.5 text-[14px]">
                <Switch checked={draft.reviewOn} onChange={(e) => set({ reviewOn: e.target.checked })} slotProps={{ input: { "aria-label": "Send to review" } }} />
                {draft.reviewOn ? (
                  <>
                    Send to review when over
                    <span className="relative">
                      <span className="pointer-events-none absolute top-1/2 left-2.5 -translate-y-1/2 font-mono text-fg-muted">£</span>
                      <input aria-label="Review threshold" inputMode="decimal" className="field !w-[110px] pl-6 font-mono" value={draft.reviewAbove} aria-invalid={!!show("review")} onChange={(e) => set({ reviewAbove: e.target.value })} />
                    </span>
                    and wait for a person
                  </>
                ) : (
                  <span className="text-fg-muted">Payments inside the limits go through without a person. The risk score can still hold unusual ones.</span>
                )}
              </div>
              <Err text={show("review")} />
            </Row>
          </fieldset>

          <div className="flex flex-col gap-4">
            <div className="flex flex-col gap-2 rounded-lg border border-line bg-elevated p-5 shadow-[inset_3px_0_0_var(--color-brand)]">
              <span className="text-[12px] font-semibold text-fg-muted">In plain words</span>
              <p className="text-[15px] leading-[1.5] font-medium" aria-live="polite">{input ? mandateSentence(input, merchants, reviewer) : ""}</p>
              {!valid && touched && <p className="text-[13px] text-review-fg">{Object.keys(errors).length} thing{Object.keys(errors).length > 1 ? "s" : ""} to fix before saving.</p>}
            </div>

            <div className="flex flex-col gap-3 rounded-lg border border-line bg-elevated p-[22px]">
              <h2 className="text-[15px] font-semibold">If this rule had run the last 30 days</h2>
              {!valid ? (
                <p className="text-[13px] text-fg-muted">Fix the form to see the impact.</p>
              ) : !replay ? (
                <Skeleton className="h-20" />
              ) : (
                <>
                  <p className="text-[12.5px] text-fg-muted">Replayed against {replay.requests} real requests from {draft.agentId}.</p>
                  <CompareBars
                    label="Outcomes of the last 30 days under the current version and under this draft"
                    rows={[...(replay.current ? [{ label: `v${existing?.version ?? ""} now`, ...replay.current }] : []), { label: "Draft", ...replay.draft }]}
                  />
                  {(["approve", "review", "block"] as const).map((o) => (
                    <div key={o} className="flex justify-between text-[14px]">
                      <span className="flex items-center gap-2">
                        <SignalDot tone={o} /> {o === "approve" ? "Approved" : o === "review" ? "Sent to review" : "Blocked"}
                      </span>
                      <span className="font-mono">
                        {replay.draft[o]}
                        {replay.current && replay.current[o] !== replay.draft[o] && <span className="text-fg-muted"> (was {replay.current[o]})</span>}
                      </span>
                    </div>
                  ))}
                  {replay.current && (
                    <p className="border-t border-line-soft pt-2.5 text-[13px]">
                      {replay.changed.length === 0 ? "No payment would end differently from the current version." : `${replay.changed.length} payment${replay.changed.length > 1 ? "s" : ""} would end differently:`}
                    </p>
                  )}
                  {replay.changed.slice(0, 5).map((c, i) => (
                    <div key={i} className="flex flex-wrap items-center gap-1.5 text-[12.5px]">
                      <span className="font-mono text-fg-muted">{dayMonth(c.at)}</span>
                      <span className="flex-1 truncate">{c.merchant} · <span className="font-mono">{money(c.amount)}</span></span>
                      <SignalDot tone={c.before} /> → <SignalDot tone={c.after} />
                    </div>
                  ))}
                </>
              )}
            </div>

            {!isNew && (
              <div className="flex flex-col gap-2 rounded-lg border border-line bg-elevated p-[22px]">
                <h2 className="text-[15px] font-semibold">Version history</h2>
                <ol className="flex flex-col">
                  {history?.map((v) => (
                    <li key={v.version} className="grid grid-cols-[34px_1fr] gap-2 border-t border-line-soft py-2 text-[13px] first:border-0">
                      <span className="font-mono font-medium">v{v.version}</span>
                      <span>
                        {v.change}
                        <span className="block text-[12px] text-fg-muted">{dayMonth(v.at)} · {v.by}</span>
                      </span>
                    </li>
                  ))}
                </ol>
              </div>
            )}
          </div>
        </div>
      </div>
      {existing && (
        <Confirm
          open={confirmPause}
          onClose={() => setConfirmPause(false)}
          title={`Pause "${existing.name}"?`}
          body={`${existing.agentId} can't spend under this mandate until someone resumes it. Payments it tries are blocked, not queued.`}
          confirm="Pause mandate"
          danger
          onConfirm={() => togglePause(existing)}
        />
      )}
    </div>
  );
}

function Row({ n, title, children, last }: { n: number; title: string; children: React.ReactNode; last?: boolean }) {
  return (
    <div className={clsx("grid gap-4 py-4 md:grid-cols-[180px_minmax(0,1fr)]", !last && "border-b border-line-soft")}>
      <span className="pt-2 text-[14px] font-semibold">
        {n} · {title}
      </span>
      <div className="flex min-w-0 flex-col gap-2.5">{children}</div>
    </div>
  );
}

function Money({ label, value, error, onChange }: { label: string; value: string; error?: string; onChange: (v: string) => void }) {
  const id = `m-${label.toLowerCase().replace(/\s+/g, "-")}`;
  return (
    <div className="flex flex-col gap-1.5">
      <label htmlFor={id} className="text-[12px] text-fg-muted">{label}</label>
      <span className="relative">
        <span className="pointer-events-none absolute top-1/2 left-3 -translate-y-1/2 font-mono text-fg-muted">£</span>
        <input id={id} inputMode="decimal" className="field pl-7 font-mono" value={value} aria-invalid={!!error} onChange={(e) => onChange(e.target.value)} />
      </span>
      <Err text={error} />
    </div>
  );
}

function Err({ text }: { text?: string }) {
  return text ? <p className="text-[12.5px] font-medium text-block-fg">{text}</p> : null;
}

const cap = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);
