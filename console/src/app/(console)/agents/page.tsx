"use client";

import Link from "next/link";
import { Allowed } from "@/components/ui/Allowed";
import { Suspense, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import Menu from "@mui/material/Menu";
import MenuItem from "@mui/material/MenuItem";
import clsx from "clsx";
import type { Agent, ApiKey, CreatedKey } from "@/api";
import { DecisionChip, OutcomeChip } from "@/components/brand";
import { Confirm, ErrorNote, Field, Modal, PageHeader, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { Icon } from "@/components/ui/icons";
import { useToast } from "@/components/ui/Toasts";
import { api, useResource } from "@/lib/useResource";
import { useSession } from "@/lib/session";
import { ago, dateLong, money } from "@/lib/format";
import { MoneyBars, OutcomeBars } from "@/components/charts";

export default function AgentsPage() {
  return (
    <Suspense fallback={null}>
      <Agents />
    </Suspense>
  );
}

/** P8 · Know every agent that can spend, and switch one off instantly. */
function Agents() {
  const params = useSearchParams();
  const router = useRouter();
  const path = usePathname();
  const toast = useToast();
  const { data: agents, error } = useResource("agents", (a) => a.agents());
  const { data: mandates } = useResource("mandates", (a) => a.mandates());
  const { data: queue } = useResource("queue", (a) => a.reviewQueue());
  const { data: unknown } = useResource("unknown", (a) => a.decisions({ agentId: "unregistered", since: Date.now() - 30 * 24 * 3600_000 }));
  const [registering, setRegistering] = useState(false);
  const [secret, setSecret] = useState<{ title: string; created: CreatedKey } | null>(null);

  const selectedId = params.get("id") ?? agents?.[0]?.id;
  const selected = agents?.find((a) => a.id === selectedId);
  const select = (id: string) => router.replace(`${path}?id=${id}`, { scroll: false });

  const meta = (a: Agent) => {
    if (a.status === "paused") return { tag: "Paused", tone: "outline" as const, sub: a.pausedBy ? `by ${a.pausedBy}` : "" };
    const n = mandates?.filter((m) => m.agentId === a.id && m.status === "active").length ?? 0;
    const waiting = queue?.filter((d) => d.agentId === a.id).length ?? 0;
    if (n === 0) return { tag: "No mandate", tone: "block" as const, sub: "can't spend" };
    if (waiting) return { tag: `${waiting} waiting`, tone: "review" as const, sub: `${n} mandate${n > 1 ? "s" : ""}` };
    return { tag: "Active", tone: "approve" as const, sub: ago(a.lastSeenAt) };
  };

  return (
    <div className="flex h-full min-h-0 flex-col">
      <div className="border-b border-line bg-elevated px-5 py-5 md:px-8">
        <PageHeader
          page="agents"
          meta={agents ? <Tag tone="neutral">{agents.length} registered</Tag> : undefined}
          actions={
            <Allowed action="manageAgents">
              {(ok) => (
                <Button variant="primary" disabled={!ok} onClick={() => setRegistering(true)}>
                  Register agent
                </Button>
              )}
            </Allowed>
          }
        />
      </div>
      <div className="flex min-h-0 flex-1 flex-col lg:flex-row">
      <div className="flex shrink-0 flex-col gap-2 border-line bg-elevated px-4 py-4 lg:w-[300px] lg:overflow-y-auto lg:border-r">
        {error && <ErrorNote text={error} />}
        {!agents ? (
          [0, 1, 2, 3].map((i) => <Skeleton key={i} className="h-16" />)
        ) : (
          <ul className="flex flex-col gap-1">
            {agents.map((a) => {
              const m = meta(a);
              const on = a.id === selectedId;
              return (
                <li key={a.id}>
                  <button
                    type="button"
                    onClick={() => select(a.id)}
                    aria-current={on}
                    className={clsx("flex w-full flex-col gap-1 rounded-md p-3 text-left", on ? "bg-brand-faint shadow-[inset_3px_0_0_var(--color-brand)]" : "hover:bg-surface")}
                  >
                    <span className="truncate font-mono text-[13.5px] font-medium">{a.id}</span>
                    <span className="flex items-center gap-2">
                      <Tag tone={m.tone} size="xs">{m.tag}</Tag>
                      {m.sub && <span className="truncate text-[12px] text-fg-muted">{m.sub}</span>}
                    </span>
                  </button>
                </li>
              );
            })}
            <li>
              <Link href="/decisions?agent=unregistered" className="flex flex-col gap-1 rounded-md p-3 hover:bg-surface">
                <span className="font-mono text-[13.5px] font-medium">agt_unknown</span>
                <span className="flex items-center gap-2">
                  <Tag tone="block" size="xs">Unregistered</Tag>
                  <span className="text-[12px] text-fg-muted">{unknown?.length ?? "…"} blocked · 30d →</span>
                </span>
              </Link>
            </li>
          </ul>
        )}
      </div>

      <main className="min-w-0 flex-1 overflow-y-auto bg-surface px-5 py-6 md:px-8">
        {selected ? (
          <AgentDetail key={selected.id} agent={selected} onCreated={(c) => setSecret({ title: `New ${c.key.mode} key for ${selected.id}`, created: c })} />
        ) : agents ? (
          <ErrorNote text={`There's no registered agent called ${selectedId}. Pick one from the list.`} />
        ) : (
          <Skeleton className="h-64" />
        )}
      </main>
      </div>

      <RegisterDialog
        open={registering}
        onClose={() => setRegistering(false)}
        onDone={(agent, created) => {
          setRegistering(false);
          select(agent.id);
          setSecret({ title: `${agent.id} is registered`, created });
          toast(`${agent.id} registered. It can't spend until it has a mandate.`, "info");
        }}
      />
      <SecretDialog value={secret} onClose={() => setSecret(null)} />
    </div>
  );
}

function AgentDetail({ agent: a, onCreated }: { agent: Agent; onCreated: (c: CreatedKey) => void }) {
  const s = useSession();
  const toast = useToast();
  const since = new Date().setHours(0, 0, 0, 0);
  const { data: keys } = useResource(`keys:${a.id}`, (x) => x.keys(a.id));
  const { data: today } = useResource(`agent-today:${a.id}`, (x) => x.decisions({ agentId: a.id, since }));
  const { data: activity } = useResource(`agent-activity:${a.id}`, (x) => x.agentActivity(a.id, 14));
  const { data: recent } = useResource(`agent-recent:${a.id}`, (x) => x.decisions({ agentId: a.id }).then((d) => d.slice(0, 6)));
  const { data: mandates } = useResource("mandates", (x) => x.mandates());
  const { data: merchants } = useResource("merchants", (x) => x.merchants());
  const [confirm, setConfirm] = useState<null | "pause" | ApiKey>(null);
  const [keyMenu, setKeyMenu] = useState<HTMLElement | null>(null);
  const mine = mandates?.filter((m) => m.agentId === a.id) ?? [];
  const spent = (today ?? []).filter((d) => d.postings.length).reduce((t, d) => t + d.amount.minor, 0);

  const setStatus = async (status: Agent["status"]) => {
    await api().setAgentStatus(a.id, status, s.user.name);
    toast(status === "paused" ? `${a.id} is paused. Its next payment will be blocked.` : `${a.id} is active again.`, status === "paused" ? "block" : "approve", status === "paused" ? { label: "Undo", run: () => api().setAgentStatus(a.id, "active", s.user.name) } : undefined);
  };
  const createKey = async (mode: ApiKey["mode"]) => {
    setKeyMenu(null);
    onCreated(await api().createKey(a.id, mode, s.user.name));
  };

  return (
    <div className="flex flex-col gap-[18px]">
      <div className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-1.5">
          <h2 className="font-mono text-[24px] font-medium">{a.id}</h2>
          <p className="text-[14px] text-fg-muted">
            {a.description} · {a.owner} · created {dateLong(a.createdAt)} by {a.createdBy}
          </p>
        </div>
        <div className="flex items-center gap-2.5">
          {a.status === "active" ? <DecisionChip tone="approve" size="md">Active</DecisionChip> : <DecisionChip tone="neutral" size="md">Paused</DecisionChip>}
          <Allowed action="manageAgents">
            {(ok) =>
              a.status === "active" ? (
                <Button variant="outline" disabled={!ok} className="!border-block !text-block-fg hover:!bg-block-tint" onClick={() => setConfirm("pause")}>
                  Pause agent
                </Button>
              ) : (
                <Button variant="primary" disabled={!ok} onClick={() => setStatus("active")}>
                  Resume agent
                </Button>
              )
            }
          </Allowed>
        </div>
      </div>

      <div className="grid gap-3.5 md:grid-cols-3">
        <Stat label="Spent today" value={today ? money({ minor: spent, currency: "GBP" }) : "…"} mono />
        <Stat label="Decisions today" value={today ? String(today.length) : "…"} mono />
        <div className="flex flex-col gap-1 rounded-md bg-elevated p-4">
          <span className="text-[12.5px] text-fg-muted">Mandates</span>
          {mine.length ? (
            <span className="flex flex-wrap gap-x-2 text-[15px] font-semibold">
              {mine.map((m, i) => (
                <Link key={m.id} href={`/mandates/${m.id}`} className="text-brand-on-subtle hover:underline">
                  {m.name}{m.status === "paused" ? " (paused)" : ""}{i < mine.length - 1 ? "," : ""}
                </Link>
              ))}
            </span>
          ) : (
            <Link href={`/mandates/new?agent=${a.id}`} className="text-[15px] font-semibold text-brand-on-subtle">Add a mandate →</Link>
          )}
        </div>
      </div>

      <section className="grid gap-3.5 lg:grid-cols-2">
        <div className="flex flex-col gap-2 rounded-lg border border-line bg-elevated p-5">
          <h3 className="text-[16px] font-semibold">Spent, last 14 days</h3>
          {activity ? (
            <MoneyBars label={`${a.id} spend per day, last 14 days`} labels={activity.map((d) => d.day)} values={activity.map((d) => d.spent.minor)} />
          ) : (
            <Skeleton className="h-[200px]" />
          )}
        </div>
        <div className="flex flex-col gap-2 rounded-lg border border-line bg-elevated p-5">
          <h3 className="text-[16px] font-semibold">Decisions, last 14 days</h3>
          {activity ? (
            <OutcomeBars height={200} label={`${a.id} decisions per day by outcome, last 14 days`} buckets={activity.map((d) => ({ label: d.day, approve: d.approve, review: d.review, block: d.block }))} />
          ) : (
            <Skeleton className="h-[200px]" />
          )}
        </div>
      </section>

      <section className="flex flex-col gap-3 rounded-lg border border-line bg-elevated p-5">
        <div className="flex items-center justify-between">
          <h3 className="text-[16px] font-semibold">API keys</h3>
          <Allowed action="manageKeys">
            {(ok) => (
              <Button variant="outline" size="small" disabled={!ok} onClick={(e) => setKeyMenu(e.currentTarget)}>
                <Icon name="key" size={15} className="mr-1.5" /> Create key
              </Button>
            )}
          </Allowed>
          {s.can("manageKeys") && (
            <>
              <Menu anchorEl={keyMenu} open={!!keyMenu} onClose={() => setKeyMenu(null)}>
                <MenuItem onClick={() => createKey("live")}>Live key</MenuItem>
                <MenuItem onClick={() => createKey("test")}>Test key</MenuItem>
              </Menu>
            </>
          )}
        </div>
        <div className="overflow-x-auto">
          <table className="data-table">
            <thead>
              <tr>
                <th>Key</th>
                <th>Mode</th>
                <th>Created</th>
                <th>Last used</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {keys?.map((k) => (
                <tr key={k.id} className={clsx(k.revokedAt && "opacity-55")}>
                  <td className="font-mono">sk_{k.mode}_••••{k.last4}</td>
                  <td className={k.mode === "test" ? "text-fg-muted" : ""}>{k.mode === "live" ? "Live" : "Test"}</td>
                  <td>{dateLong(k.createdAt)}</td>
                  <td>{k.revokedAt ? `Revoked ${ago(k.revokedAt)}` : ago(k.lastUsedAt)}</td>
                  <td className="text-right">
                    {!k.revokedAt && (
                      <Allowed action="manageKeys">
                        {(ok) => (
                          <button type="button" disabled={!ok} className="font-semibold text-block-fg hover:underline disabled:cursor-not-allowed disabled:text-fg-subtle disabled:no-underline" onClick={() => setConfirm(k)}>
                            Revoke
                          </button>
                        )}
                      </Allowed>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>

      <section className="flex flex-col gap-2.5 rounded-lg border border-line bg-elevated p-5">
        <div className="flex items-center justify-between">
          <h3 className="text-[16px] font-semibold">Recent decisions</h3>
          <Link href={`/decisions?agent=${a.id}`} className="text-[13px] font-semibold text-brand-on-subtle">Open in Decisions →</Link>
        </div>
        {recent?.length === 0 && <p className="text-[13px] text-fg-muted">No payments yet.</p>}
        {recent?.map((d) => (
          <Link key={d.id} href={`/decisions?id=${d.id}`} className="flex items-center justify-between gap-3 border-t border-line-soft pt-2.5 text-[13.5px] first:border-0 first:pt-0 hover:underline">
            <span className="font-mono">
              {money(d.amount)} · {merchants?.find((m) => m.id === d.merchantId)?.name}
            </span>
            <OutcomeChip decision={d} />
          </Link>
        ))}
      </section>

      <Confirm
        open={confirm === "pause"}
        onClose={() => setConfirm(null)}
        title={`Pause ${a.id}?`}
        body="Its next payment is blocked straight away, whatever its mandates say. You can resume it at any time."
        confirm="Pause agent"
        danger
        onConfirm={() => setStatus("paused")}
      />
      <Confirm
        open={!!confirm && confirm !== "pause"}
        onClose={() => setConfirm(null)}
        title="Revoke this key?"
        body={`Requests signed with it are rejected immediately, as if ${a.id} were unknown. If it's the only live key, the agent stops being able to pay.`}
        confirm="Revoke key"
        danger
        onConfirm={async () => {
          if (confirm && confirm !== "pause") {
            await api().revokeKey(confirm.id, s.user.name);
            toast(`Key ••••${confirm.last4} revoked.`, "block");
          }
        }}
      />
    </div>
  );
}

function Stat({ label, value, mono }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="flex flex-col gap-1 rounded-md bg-elevated p-4">
      <span className="text-[12.5px] text-fg-muted">{label}</span>
      <span className={clsx("text-[22px]", mono && "font-mono")}>{value}</span>
    </div>
  );
}

function RegisterDialog({ open, onClose, onDone }: { open: boolean; onClose: () => void; onDone: (a: Agent, c: CreatedKey) => void }) {
  const s = useSession();
  const [id, setId] = useState("agt_");
  const [description, setDescription] = useState("");
  const [owner, setOwner] = useState("Finance");
  const [error, setError] = useState<string | null>(null);
  return (
    <Modal
      open={open}
      onClose={onClose}
      title="Register an agent"
      body="It gets a live key straight away, and can't spend until it has a mandate."
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>Cancel</Button>
          <Button
            variant="primary"
            onClick={async () => {
              try {
                const r = await api().registerAgent({ id: id.trim(), description: description.trim() || "No description yet", owner }, s.user.name);
                setError(null);
                setId("agt_");
                setDescription("");
                onDone(r.agent, r.created);
              } catch (e) {
                setError(e instanceof Error ? e.message : "Couldn't register.");
              }
            }}
          >
            Register and create key
          </Button>
        </>
      }
    >
      <Field label="Agent ID" error={error} hint="agt_ then lowercase letters, digits or dashes.">
        {(fid) => <input id={fid} className="field font-mono" value={id} aria-invalid={!!error} onChange={(e) => setId(e.target.value)} />}
      </Field>
      <Field label="What it does">{(fid) => <input id={fid} className="field" value={description} placeholder="Pays approved supplier invoices" onChange={(e) => setDescription(e.target.value)} />}</Field>
      <Field label="Owner">
        {(fid) => (
          <select id={fid} className="field" value={owner} onChange={(e) => setOwner(e.target.value)}>
            {["Finance", "Sales ops", "Engineering", "Support", "Marketing"].map((o) => <option key={o}>{o}</option>)}
          </select>
        )}
      </Field>
    </Modal>
  );
}

function SecretDialog({ value, onClose }: { value: { title: string; created: CreatedKey } | null; onClose: () => void }) {
  const toast = useToast();
  return (
    <Modal
      open={!!value}
      onClose={onClose}
      title={value?.title ?? ""}
      body="Copy this key now. You won't see it again: after this only the last 4 characters are shown."
      actions={
        <>
          <Button
            variant="outline"
            onClick={() =>
              value &&
              navigator.clipboard.writeText(value.created.secret).then(
                () => toast("Key copied.", "info"),
                () => toast("Your browser blocked copying. Select the key and copy it by hand.", "review"),
              )
            }
          >
            <Icon name="copy" size={15} className="mr-1.5" /> Copy key
          </Button>
          <Button variant="primary" onClick={onClose}>I&apos;ve saved it</Button>
        </>
      }
    >
      <code className="block rounded-md border border-dashed border-mist bg-surface p-3.5 font-mono text-[13px] break-all select-all">{value?.created.secret}</code>
    </Modal>
  );
}
