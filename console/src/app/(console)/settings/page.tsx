"use client";

import Link from "next/link";
import { Suspense, useEffect, useState } from "react";
import { usePathname, useRouter, useSearchParams } from "next/navigation";
import Button from "@mui/material/Button";
import clsx from "clsx";
import type { Invite, Member, Role } from "@/api";
import { DecisionChip } from "@/components/brand";
import { Confirm, Field, PageHeader, Skeleton } from "@/components/ui";
import { Tag } from "@/components/ui/Tag";
import { useToast } from "@/components/ui/Toasts";
import { api, useResource } from "@/lib/useResource";
import { useSession } from "@/lib/session";
import { ROLES, ROLE_INFO } from "@/lib/permissions";
import { Allowed, RoleMatrix } from "@/components/ui/Allowed";
import { ago, dateLong } from "@/lib/format";

type Tab = "workspace" | "members" | "keys" | "webhooks";
const TABS: { value: Tab; label: string }[] = [
  { value: "workspace", label: "Workspace" },
  { value: "members", label: "Members" },
  { value: "keys", label: "API keys" },
  { value: "webhooks", label: "Webhooks" },
];

export default function SettingsPage() {
  return (
    <Suspense fallback={null}>
      <Settings />
    </Suspense>
  );
}

/** P10 · Invite people and give them the right power. */
function Settings() {
  const params = useSearchParams();
  const router = useRouter();
  const path = usePathname();
  const tab = (params.get("tab") as Tab) ?? "members";
  const s = useSession();
  return (
    <div className="mx-auto flex max-w-[1280px] flex-col gap-[18px] px-5 py-7 md:px-8">
      <PageHeader page="settings" meta={!s.can("manageMembers") ? <Tag tone="neutral" title="Only owners and admins can change settings">Read only</Tag> : undefined} />
      <div role="tablist" aria-label="Settings sections" className="flex gap-6 overflow-x-auto border-b border-line text-[14px]">
        {TABS.map((t) => (
          <button
            key={t.value}
            role="tab"
            aria-selected={tab === t.value}
            onClick={() => router.replace(`${path}?tab=${t.value}`, { scroll: false })}
            className={clsx("py-2.5 whitespace-nowrap", tab === t.value ? "font-semibold shadow-[inset_0_-2px_0_var(--color-brand)]" : "text-fg-muted hover:text-fg")}
          >
            {t.label}
          </button>
        ))}
      </div>
      {tab === "workspace" && <WorkspaceTab />}
      {tab === "members" && <MembersTab />}
      {tab === "keys" && <KeysTab />}
      {tab === "webhooks" && <WebhooksTab />}
    </div>
  );
}

function WorkspaceTab() {
  const s = useSession();
  const toast = useToast();
  const { data: ws } = useResource("workspace", (a) => a.workspace());
  const [name, setName] = useState("");
  useEffect(() => {
    if (ws) setName(ws.name);
  }, [ws]);
  if (!ws) return <Skeleton className="h-40" />;
  return (
    <div className="flex max-w-[640px] flex-col gap-5">
      <div className="flex flex-wrap items-end gap-3">
        <Field label="Workspace name" className="min-w-[260px] flex-1">
          {(id) => <input id={id} className="field" value={name} disabled={!s.can("renameWorkspace")} onChange={(e) => setName(e.target.value)} />}
        </Field>
        <Button variant="outline" disabled={!s.can("renameWorkspace") || name.trim() === ws.name} onClick={() => api().renameWorkspace(name, s.user.name).then(() => toast("Workspace renamed."), (e: Error) => toast(e.message, "block"))}>
          Save name
        </Button>
      </div>
      <dl className="grid grid-cols-[140px_1fr] gap-y-2 text-[14px]">
        <dt className="text-fg-muted">Workspace ID</dt>
        <dd className="font-mono">{ws.id}</dd>
        <dt className="text-fg-muted">Time zone</dt>
        <dd>{ws.timeZone} (limits reset at midnight here)</dd>
        <dt className="text-fg-muted">Currency</dt>
        <dd>GBP only (SPEC-002)</dd>
      </dl>
    </div>
  );
}

function MembersTab() {
  const s = useSession();
  const toast = useToast();
  const { data: members } = useResource("members", (a) => a.members());
  const { data: invites } = useResource("invites", (a) => a.invites());
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<Invite["role"]>("member");
  const [error, setError] = useState<string | null>(null);
  const [removing, setRemoving] = useState<Member | null>(null);
  const allowed = s.can("manageMembers");

  const send = async () => {
    try {
      await api().invite(email.trim(), role, s.user.name);
      toast(`Invitation sent to ${email.trim()} as ${ROLE_INFO[role].label}.`);
      setEmail("");
      setError(null);
    } catch (e) {
      setError(e instanceof Error ? e.message : "Couldn't send.");
    }
  };

  return (
    <div className="flex flex-col gap-8">
      <div className="flex flex-col gap-3.5">
        <form
          className="flex flex-wrap gap-2.5"
          onSubmit={(e) => {
            e.preventDefault();
            send();
          }}
        >
          <label htmlFor="inv-email" className="sr-only">Invite by email</label>
          <input id="inv-email" type="email" className="field min-w-[220px] flex-1" placeholder="name@example.com" value={email} disabled={!allowed} aria-invalid={!!error} onChange={(e) => setEmail(e.target.value)} />
          <label htmlFor="inv-role" className="sr-only">Role</label>
          <select id="inv-role" className="field !w-auto" value={role} disabled={!allowed} onChange={(e) => setRole(e.target.value as Invite["role"])}>
            {(["admin", "member", "viewer"] as const).map((r) => <option key={r} value={r}>{ROLE_INFO[r].label}</option>)}
          </select>
          <Allowed action="manageMembers">
            {(ok) => (
              <Button type="submit" variant="primary" disabled={!ok || !email.trim()}>
                Send invite
              </Button>
            )}
          </Allowed>
        </form>
        {error ? (
          <p className="text-[12.5px] font-medium text-block-fg">{error}</p>
        ) : (
          allowed && (
            <p className="text-[12.5px] text-fg-muted">
              <b className="font-semibold text-fg">{ROLE_INFO[role].label}:</b> {ROLE_INFO[role].text}
            </p>
          )
        )}

        <div className="overflow-x-auto rounded-md border border-line">
          <table className="data-table">
            <thead>
              <tr>
                <th>Name</th>
                <th>Email</th>
                <th>Role</th>
                <th>Last active</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {members?.map((m) => (
                <tr key={m.id}>
                  <td className="!h-[52px]">
                    <span className="flex items-center gap-2.5">
                      <span className="grid size-7 place-items-center rounded-full bg-neutral-tint text-[11px] font-semibold">{m.initials}</span>
                      {m.name}
                      {m.name === s.user.name && <span className="text-fg-muted">(you)</span>}
                    </span>
                  </td>
                  <td className="text-fg-muted">{m.email}</td>
                  <td>
                    {allowed && m.role !== "owner" ? (
                      <>
                        <label htmlFor={`role-${m.id}`} className="sr-only">Role for {m.name}</label>
                        <select
                          id={`role-${m.id}`}
                          className="field !h-8 !w-auto text-[13px]"
                          value={m.role}
                          onChange={(e) => api().setRole(m.id, e.target.value as Role, s.user.name).then(() => toast(`${m.name} is now ${ROLE_INFO[e.target.value as Role].label}.`), (er: Error) => toast(er.message, "block"))}
                        >
                          {(["admin", "member", "viewer"] as const).map((r) => <option key={r} value={r}>{ROLE_INFO[r].label}</option>)}
                        </select>
                      </>
                    ) : (
                      <span className="rounded-lg border border-line px-2.5 py-1 text-[13px]">{ROLE_INFO[m.role].label}</span>
                    )}
                  </td>
                  <td className="text-[13px] text-fg-muted">{m.name === s.user.name ? "now" : ago(m.lastActiveAt)}</td>
                  <td className="text-right">
                    {allowed && m.role !== "owner" && (
                      <button type="button" className="text-[13px] font-semibold text-block-fg hover:underline" onClick={() => setRemoving(m)}>Remove</button>
                    )}
                  </td>
                </tr>
              ))}
              {invites?.map((i) => (
                <tr key={i.id}>
                  <td className="!h-[52px] text-fg-muted">
                    <span className="flex items-center gap-2.5">
                      <span className="grid size-7 place-items-center rounded-full border border-dashed border-mist text-[11px]">·</span>
                      Invite pending
                    </span>
                  </td>
                  <td className="text-fg-muted">{i.email}</td>
                  <td><DecisionChip tone="review">{ROLE_INFO[i.role].label}</DecisionChip></td>
                  <td className="text-[13px] text-fg-muted">sent {ago(i.sentAt)}</td>
                  <td className="text-right">
                    {allowed && (
                      <button type="button" className="text-[13px] font-semibold text-fg-muted hover:underline" onClick={() => api().revokeInvite(i.id, s.user.name).then(() => toast(`Invitation to ${i.email} revoked.`))}>Revoke</button>
                    )}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </div>

      <section aria-labelledby="roles-h" className="flex flex-col gap-4">
        <div className="flex flex-col gap-1">
          <h2 id="roles-h" className="text-[18px] font-semibold tracking-[-0.01em]">Roles</h2>
          <p className="max-w-[72ch] text-[13.5px] text-fg-muted">
            For people only. Agents are limited by mandates and keys.
          </p>
        </div>
        <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
          {ROLES.map((r) => {
            const n = members?.filter((m) => m.role === r).length ?? 0;
            return (
              <div key={r} className={clsx("flex flex-col gap-1.5 rounded-md border p-4", r === s.role ? "border-brand bg-brand-faint" : "border-line")}>
                <span className="flex items-center justify-between gap-2">
                  <span className="text-[15px] font-semibold">{ROLE_INFO[r].label}</span>
                  <span className="text-[12px] text-fg-muted">
                    {n} {n === 1 ? "person" : "people"}
                    {r === s.role && " · you"}
                  </span>
                </span>
                <span className="text-[13px] leading-normal">{ROLE_INFO[r].text}</span>
                <span className="text-[12.5px] leading-normal text-fg-muted">{ROLE_INFO[r].who}</span>
              </div>
            );
          })}
        </div>
        <div className="rounded-md border border-line p-4">
          <RoleMatrix compact />
        </div>
      </section>

      <Confirm
        open={!!removing}
        onClose={() => setRemoving(null)}
        title={`Remove ${removing?.name}?`}
        body="They lose access straight away. Their past approvals stay on the ledger."
        confirm="Remove"
        danger
        onConfirm={() => removing && api().removeMember(removing.id, s.user.name).then(() => toast(`${removing.name} removed.`), (e: Error) => toast(e.message, "block"))}
      />
    </div>
  );
}

function KeysTab() {
  const { data: keys } = useResource("all-keys", (a) => a.keys());
  if (!keys) return <Skeleton className="h-40" />;
  return (
    <div className="flex flex-col gap-3">
      <p className="text-[13.5px] text-fg-muted">All agent keys. Manage them on each agent&apos;s page.</p>
      <div className="overflow-x-auto rounded-md border border-line">
        <table className="data-table">
          <thead>
            <tr>
              <th>Key</th>
              <th>Agent</th>
              <th>Mode</th>
              <th>Created</th>
              <th>Last used</th>
              <th>Status</th>
            </tr>
          </thead>
          <tbody>
            {keys.map((k) => (
              <tr key={k.id}>
                <td className="font-mono">sk_{k.mode}_••••{k.last4}</td>
                <td>
                  <Link className="font-mono text-[13px] hover:underline" href={`/agents?id=${k.agentId}`}>{k.agentId}</Link>
                </td>
                <td>{k.mode === "live" ? "Live" : "Test"}</td>
                <td>{dateLong(k.createdAt)}</td>
                <td>{ago(k.lastUsedAt)}</td>
                <td>{k.revokedAt ? <DecisionChip tone="neutral">Revoked</DecisionChip> : <DecisionChip tone="approve">Active</DecisionChip>}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function WebhooksTab() {
  const { data: hooks } = useResource("webhooks", (a) => a.webhooks());
  return (
    <div className="flex flex-col gap-3">
      <p className="text-[13.5px] text-fg-muted">URLs Strata calls when something happens. HMAC-signed, retried.</p>
      {hooks?.map((h) => (
        <div key={h.id} className="flex flex-wrap items-center gap-3 rounded-md border border-line p-4">
          <span className="min-w-0 flex-1 truncate font-mono text-[13px]">{h.url}</span>
          <span className="flex flex-wrap gap-1.5">
            {h.events.map((e) => <span key={e} className="rounded-md bg-surface px-2 py-0.5 font-mono text-[11.5px] text-fg-muted">{e}</span>)}
          </span>
          {h.enabled ? <DecisionChip tone="approve">On</DecisionChip> : <DecisionChip tone="neutral">Off</DecisionChip>}
        </div>
      ))}
    </div>
  );
}
