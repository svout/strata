import clsx from "clsx";
import type { ReactNode } from "react";
import type { ReasonCode } from "@/api/types";
import type { SectionKey } from "@/lib/sections";

/**
 * Short labels that carry meaning by colour and word together, so a screen
 * can be scanned without reading sentences. Colour is never the only cue:
 * the word always says it too.
 *
 * - approve / review / block: outcomes only (signal colours)
 * - section keys: what part of the product something belongs to
 * - brand: the one thing to look at; neutral: facts (limits, IDs, counts)
 */
export type TagTone = "neutral" | "brand" | "approve" | "review" | "block" | "outline" | SectionKey;

const TONE: Record<TagTone, string> = {
  neutral: "bg-neutral-tint text-fg",
  outline: "border border-line text-fg-muted",
  brand: "bg-brand-subtle text-brand-on-subtle",
  approve: "bg-approve-tint text-approve-fg",
  review: "bg-review-tint text-review-fg",
  block: "bg-block-tint text-block-fg",
  monitor: "bg-neutral-tint text-fg-muted",
  control: "bg-brand-faint text-brand-on-subtle",
  record: "bg-neutral-tint text-fg-muted",
  admin: "bg-neutral-tint text-fg-muted",
  demo: "border border-dashed border-line text-fg-muted",
};

export function Tag({ tone = "neutral", children, mono, size = "sm", title, className }: { tone?: TagTone; children: ReactNode; mono?: boolean; size?: "xs" | "sm"; title?: string; className?: string }) {
  return (
    <span
      title={title}
      className={clsx(
        "inline-flex max-w-full items-center gap-1 rounded-sm font-medium whitespace-nowrap",
        size === "xs" ? "px-1.5 py-px text-[11.5px]" : "px-1.5 py-0.5 text-[12px]",
        mono && "font-mono font-medium",
        TONE[tone],
        className,
      )}
    >
      <span className="flex min-w-0 items-center gap-1 overflow-hidden text-ellipsis">{children}</span>
    </span>
  );
}

/** Which tone a reason code gets: block reasons red, review reasons amber, the rest neutral. */
const BLOCKING: ReasonCode[] = [
  "UNKNOWN_AGENT_KEY",
  "SIGNATURE_INVALID",
  "AGENT_PAUSED",
  "NO_MANDATE",
  "NONCE_REPLAYED",
  "HOST_MISMATCH",
  "CATEGORY_OUT_OF_SCOPE",
  "MERCHANT_NOT_IN_SCOPE",
  "CART_EXCEEDS_INTENT",
  "PER_PAYMENT_CAP",
  "DAILY_LIMIT",
  "MONTHLY_LIMIT",
  "OUTSIDE_TIME_WINDOW",
];
export const reasonTone = (c: ReasonCode): TagTone => (c === "WITHIN_MANDATE" ? "approve" : c === "SCORING_DEGRADED" ? "outline" : BLOCKING.includes(c) ? "block" : "review");
