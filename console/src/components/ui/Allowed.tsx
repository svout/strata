"use client";

import type { ReactNode } from "react";
import Tooltip from "@mui/material/Tooltip";
import clsx from "clsx";
import type { Role } from "@/api/types";
import { ACTIONS, ROLES, ROLE_INFO, can, deniedReason, type Action } from "@/lib/permissions";
import { useSession } from "@/lib/session";

/**
 * Wrap a control that needs a permission. When the current role can't use
 * it, the control is rendered disabled and a tooltip says who can.
 *
 *   <Allowed action="manageAgents">{(ok) => <Button disabled={!ok}>Pause agent</Button>}</Allowed>
 */
export function Allowed({ action, children }: { action: Action; children: (allowed: boolean) => ReactNode }) {
  const { role } = useSession();
  const ok = can(role, action);
  if (ok) return <>{children(true)}</>;
  return (
    <Tooltip title={deniedReason(action, role)} arrow>
      {/* Disabled buttons don't fire hover events, so the tooltip hangs on a wrapper */}
      <span className="inline-flex" tabIndex={0} aria-label={deniedReason(action, role)}>
        {children(false)}
      </span>
    </Tooltip>
  );
}

/** The full permissions table, with the current role highlighted. */
export function RoleMatrix({ compact }: { compact?: boolean }) {
  const { role } = useSession();
  const actions = (Object.keys(ACTIONS) as Action[]).filter((a) => a !== "view");
  return (
    <div className="overflow-x-auto">
      <table className="w-full border-collapse text-[13px]">
        <caption className="sr-only">What each role can do</caption>
        <thead>
          <tr>
            <th className="py-2 pr-3 text-left text-[12px] font-semibold text-fg-muted">Can…</th>
            {ROLES.map((r) => (
              <th key={r} scope="col" className={clsx("px-2 py-2 text-center text-[12.5px] font-semibold", r === role && "rounded-t-lg bg-brand-subtle text-brand-on-subtle")}>
                {ROLE_INFO[r].label}
                {r === role && <span className="block text-[10.5px] font-normal">you</span>}
              </th>
            ))}
          </tr>
        </thead>
        <tbody>
          <Row label={ACTIONS.view.label} values={ROLES.map(() => true)} current={role} />
          {actions.map((a) => (
            <Row key={a} label={ACTIONS[a].label} values={ROLES.map((r) => can(r, a))} current={role} />
          ))}
        </tbody>
      </table>
      {!compact && <p className="mt-3 text-[12px] text-fg-muted">Agents don&apos;t have roles. What an agent may spend is set by its mandates; whether it can sign requests at all is set by its keys.</p>}
    </div>
  );
}

function Row({ label, values, current }: { label: string; values: boolean[]; current: Role }) {
  return (
    <tr className="border-t border-line-soft">
      <th scope="row" className="py-2 pr-3 text-left font-normal">{label}</th>
      {values.map((v, i) => (
        <td key={ROLES[i]} className={clsx("px-2 py-2 text-center", ROLES[i] === current && "bg-brand-subtle")}>
          {v ? <span aria-label="Yes" className="font-semibold text-approve-fg">✓</span> : <span aria-label="No" className="text-fg-subtle">—</span>}
        </td>
      ))}
    </tr>
  );
}
