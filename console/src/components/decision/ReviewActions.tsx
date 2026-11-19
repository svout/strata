"use client";

import { useState } from "react";
import Button from "@mui/material/Button";
import type { Decision, DeclineReason } from "@/api/types";
import { Modal } from "@/components/ui";
import { useToast } from "@/components/ui/Toasts";
import { useSession } from "@/lib/session";
import { api } from "@/lib/useResource";
import { money } from "@/lib/format";

const REASONS: { value: DeclineReason; label: string }[] = [
  { value: "wrong merchant", label: "Wrong merchant" },
  { value: "too expensive", label: "Too expensive" },
  { value: "not needed", label: "Not needed" },
  { value: "other", label: "Other" },
];

/**
 * Approve / decline for one held payment. Declining asks why (P5 board): the
 * reason is saved to the ledger with the reviewer's name.
 */
export function useReview() {
  const s = useSession();
  const toast = useToast();
  const [declining, setDeclining] = useState<Decision | null>(null);

  const approve = async (d: Decision) => {
    if (!s.can("review")) return;
    try {
      const r = await api().approve(d.id, s.user.name);
      toast(`Approved ${money(d.amount)} for ${d.agentId}. Ledger entry #${r.review && "seq" in r.review ? r.review.seq : "—"}.`, "approve");
    } catch (e) {
      toast(e instanceof Error ? e.message : "Couldn't approve.", "block");
    }
  };

  const dialog = (
    <DeclineDialog
      decision={declining}
      onClose={() => setDeclining(null)}
      onDecline={async (reason) => {
        const d = declining!;
        setDeclining(null);
        try {
          const r = await api().decline(d.id, s.user.name, reason);
          toast(`Declined ${money(d.amount)}: ${reason}. Ledger entry #${r.review && "seq" in r.review ? r.review.seq : "—"}.`, "block");
        } catch (e) {
          toast(e instanceof Error ? e.message : "Couldn't decline.", "block");
        }
      }}
    />
  );

  return { approve, decline: (d: Decision) => s.can("review") && setDeclining(d), dialog, allowed: s.can("review") };
}

function DeclineDialog({ decision, onClose, onDecline }: { decision: Decision | null; onClose: () => void; onDecline: (r: DeclineReason) => void }) {
  const [reason, setReason] = useState<DeclineReason>("not needed");
  return (
    <Modal
      open={!!decision}
      onClose={onClose}
      title={`Decline ${decision ? money(decision.amount) : ""}?`}
      body="No money moves. Your name and the reason go on the ledger."
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button variant="primary" className="!bg-block hover:!bg-block-fg" onClick={() => onDecline(reason)}>
            Decline payment
          </Button>
        </>
      }
    >
      <fieldset className="flex flex-col gap-2">
        <legend className="mb-2 text-[13px] font-semibold">Why?</legend>
        {REASONS.map((r) => (
          <label key={r.value} className="flex cursor-pointer items-center gap-2.5 rounded-lg border border-line px-3 py-2.5 text-[14px] has-[:checked]:border-brand has-[:checked]:bg-brand-faint">
            <input type="radio" name="decline-reason" value={r.value} checked={reason === r.value} onChange={() => setReason(r.value)} className="accent-[var(--color-brand)]" />
            {r.label}
          </label>
        ))}
      </fieldset>
    </Modal>
  );
}
