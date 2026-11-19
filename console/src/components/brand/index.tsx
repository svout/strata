import clsx from "clsx";
import type { Decision } from "@/api/types";

/**
 * Brand elements, kept identical to strata-marketing/src/components/elements
 * (Logo, DecisionChip, Overline) so the console and the website read as one
 * product. If one changes, change the other.
 */

export function LogoMark({ size = 28, tile = "#2B59FF", bars = "#FFFFFF" }: { size?: number; tile?: string; bars?: string }) {
  return (
    <svg width={size} height={size} viewBox="0 0 48 48" aria-hidden focusable="false">
      <rect width="48" height="48" rx="12.5" fill={tile} />
      <g transform="translate(7.68 7.68) scale(0.68)" fill={bars}>
        <rect x="18" y="7" width="24" height="9" rx="4.5" />
        <rect x="12" y="19.5" width="24" height="9" rx="4.5" />
        <rect x="6" y="32" width="24" height="9" rx="4.5" />
      </g>
    </svg>
  );
}

export function Logo({ size = 28, className }: { size?: number; className?: string }) {
  return (
    <span className={clsx("inline-flex items-center gap-2", className)}>
      <LogoMark size={size} />
      <span className="leading-none font-semibold tracking-[-0.05em] text-fg" style={{ fontSize: Math.round(size * 0.72) }}>
        strata
      </span>
    </span>
  );
}

export type ChipTone = "approve" | "review" | "block" | "recorded" | "neutral";
const CHIP: Record<ChipTone, { chip: string; dot: string }> = {
  approve: { chip: "bg-approve-tint text-approve-fg", dot: "bg-approve" },
  review: { chip: "bg-review-tint text-review-fg", dot: "bg-review" },
  block: { chip: "bg-block-tint text-block-fg", dot: "bg-block" },
  recorded: { chip: "bg-recorded-tint text-recorded-fg", dot: "bg-brand" },
  neutral: { chip: "bg-neutral-tint text-fg-muted", dot: "bg-mist" },
};

/** Pill, tinted background, dark text, a dot carrying the signal colour. The label always says the outcome in words. */
export function DecisionChip({ tone, children, size = "sm", className }: { tone: ChipTone; children: React.ReactNode; size?: "sm" | "md"; className?: string }) {
  return (
    <span
      className={clsx(
        "inline-flex items-center gap-1.5 rounded-sm font-medium whitespace-nowrap",
        size === "sm" ? "px-2.5 py-[3px] text-xs" : "px-3 py-1 text-label",
        CHIP[tone].chip,
        className,
      )}
    >
      <span aria-hidden className={clsx("size-[7px] shrink-0 rounded-full", CHIP[tone].dot)} />
      {children}
    </span>
  );
}

export function SignalDot({ tone, className }: { tone: ChipTone; className?: string }) {
  return <span aria-hidden className={clsx("inline-block size-2 shrink-0 rounded-full", CHIP[tone].dot, className)} />;
}

/** What a decision ended as, including what a person did with it. */
export function decisionState(d: Pick<Decision, "outcome" | "review">): { tone: ChipTone; label: string } {
  if (d.outcome === "approve") return { tone: "approve", label: "Approved" };
  if (d.outcome === "block") return { tone: "block", label: "Blocked" };
  if (d.review?.status === "approved") return { tone: "approve", label: "Approved by a person" };
  if (d.review?.status === "declined") return { tone: "block", label: "Declined" };
  return { tone: "review", label: "Waiting for review" };
}

export function OutcomeChip({ decision, size }: { decision: Pick<Decision, "outcome" | "review">; size?: "sm" | "md" }) {
  const s = decisionState(decision);
  return (
    <DecisionChip tone={s.tone} size={size}>
      {s.label}
    </DecisionChip>
  );
}

/** mono/overline: Plex Mono, caps, wide tracking. */
export function Overline({ children, className }: { children: React.ReactNode; className?: string }) {
  return <p className={clsx("text-[12px] font-semibold text-fg-muted", className)}>{children}</p>;
}
