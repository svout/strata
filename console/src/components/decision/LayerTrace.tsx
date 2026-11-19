import clsx from "clsx";
import type { Decision } from "@/api/types";
import { shortHash } from "@/lib/format";

/**
 * Every decision shows its three layers in order, Mandate → Risk → Ledger,
 * as a plain checklist: a coloured dot and verb per layer.
 */
type Tone = "pass" | "review" | "fail" | "skipped" | "ledger" | "idle";
/** Dot colour and verb colour per state; the row itself stays plain. */
const TONE: Record<Tone, { dot: string; verb: string }> = {
  pass: { dot: "bg-approve", verb: "text-approve-fg" },
  review: { dot: "bg-review", verb: "text-review-fg" },
  fail: { dot: "bg-block", verb: "text-block-fg" },
  skipped: { dot: "border border-fg-subtle bg-transparent", verb: "text-fg-subtle" },
  ledger: { dot: "bg-brand", verb: "text-brand-on-subtle" },
  idle: { dot: "border border-dashed border-fg-subtle bg-transparent", verb: "text-fg-subtle" },
};
const VERB: Record<Tone, string> = { pass: "pass", review: "flagged", fail: "failed", skipped: "not reached", ledger: "recorded", idle: "waiting" };

export type LayerView = { name: string; tone: Tone; summary: string; mono?: boolean };

export function layersOf(d: Decision): LayerView[] {
  return [
    { name: "Mandate", tone: d.layers.mandate.status, summary: d.layers.mandate.summary },
    { name: "Risk", tone: d.layers.risk.status, summary: d.layers.risk.summary },
    { name: "Ledger", tone: "ledger", summary: d.layers.ledger.seq ? `#${d.layers.ledger.seq} · ${shortHash(d.layers.ledger.hash)}` : "Writing…", mono: true },
  ];
}

/** `upTo` animates a run in the attack lab: layers after it show as waiting. */
export function LayerTrace({ layers, upTo, compact }: { layers: LayerView[]; upTo?: number; compact?: boolean }) {
  return (
    <ol aria-label="How it was decided" className="flex flex-col divide-y divide-line-soft rounded-lg border border-line bg-elevated">
      {layers.map((l, i) => {
        const tone: Tone = upTo != null && i > upTo ? "idle" : l.tone;
        return (
          <li key={l.name} className={clsx("flex items-start gap-3 transition-colors", compact ? "px-3 py-2" : "px-3.5 py-2.5", upTo === i && "bg-brand-faint")}>
            <span aria-hidden className={clsx("mt-[6px] size-2 shrink-0 rounded-full", TONE[tone].dot)} />
            <span className="flex min-w-0 flex-col">
              <span className="text-[13.5px] font-semibold">
                {l.name} · <span className={clsx("font-medium", TONE[tone].verb)}>{VERB[tone]}</span>
              </span>
              {tone !== "idle" && <span className={clsx("text-[12.5px] text-fg-muted", l.mono && "font-mono text-[12px]")}>{l.summary}</span>}
            </span>
          </li>
        );
      })}
    </ol>
  );
}
