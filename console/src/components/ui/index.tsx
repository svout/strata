"use client";

import clsx from "clsx";
import Dialog from "@mui/material/Dialog";
import Button from "@mui/material/Button";
import { useId, useState, type ReactNode } from "react";
import Popover from "@mui/material/Popover";
import { PAGES, type PageKey } from "@/lib/sections";

/**
 * Page header: title, status tags, what this page is
 * for (one line), and a "?" with up to three tips. Pass `page` to take all
 * of that from src/lib/sections.ts; `title` overrides the title.
 */
export function PageHeader({ page, title, meta, actions, crumb }: { page?: PageKey; title?: ReactNode; meta?: ReactNode; actions?: ReactNode; crumb?: ReactNode }) {
  const p = page ? PAGES[page] : null;
  return (
    <div className="flex flex-wrap items-start justify-between gap-4">
      <div className="flex min-w-0 items-start">
        <div className="flex min-w-0 flex-col gap-1">
          {crumb && <div className="text-[13px] font-medium text-brand">{crumb}</div>}
          <div className="flex flex-wrap items-center gap-2">
            <h1 className="text-h1">{title ?? p?.title}</h1>
            {meta}
            {p && <PageHelp page={page!} />}
          </div>
          {p && <p className="text-[14px] text-fg-subtle">{p.purpose}</p>}
        </div>
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2.5 pt-1">{actions}</div>}
    </div>
  );
}

function PageHelp({ page }: { page: PageKey }) {
  const [anchor, setAnchor] = useState<HTMLElement | null>(null);
  const p = PAGES[page];
  const id = useId();
  return (
    <>
      <button
        type="button"
        aria-label={`How ${p.title} works`}
        aria-describedby={anchor ? id : undefined}
        onClick={(e) => setAnchor(e.currentTarget)}
        className="grid size-5 place-items-center rounded-full border border-line text-[11px] font-semibold text-fg-subtle hover:border-fg-muted hover:text-fg"
      >
        ?
      </button>
      <Popover
        open={!!anchor}
        anchorEl={anchor}
        onClose={() => setAnchor(null)}
        anchorOrigin={{ vertical: "bottom", horizontal: "left" }}
        slotProps={{ paper: { sx: { mt: 1, borderRadius: "8px", maxWidth: 340 } } }}
      >
        <div id={id} className="flex flex-col gap-2 p-4">
          <span className="text-[13px] font-semibold">{p.title}</span>
          <ul className="flex flex-col gap-1.5 text-[13.5px]">
            {p.tips.map((t) => (
              <li key={t} className="flex gap-2">
                <span aria-hidden className="mt-[7px] size-1 shrink-0 rounded-full bg-fg-subtle" />
                {t}
              </li>
            ))}
          </ul>
        </div>
      </Popover>
    </>
  );
}

/** Filter pills: active = ink, others = paper (P4, P6, P9 boards). */
export function Segmented<T extends string>({
  value,
  options,
  onChange,
  label,
  size = "md",
}: {
  value: T;
  options: { value: T; label: ReactNode; count?: number }[];
  onChange: (v: T) => void;
  label: string;
  size?: "sm" | "md";
}) {
  return (
    <div role="group" aria-label={label} className="flex flex-wrap gap-1.5">
      {options.map((o) => {
        const on = o.value === value;
        return (
          <button
            key={o.value}
            type="button"
            aria-pressed={on}
            onClick={() => onChange(o.value)}
            className={clsx(
              "inline-flex items-center gap-1.5 rounded-md border font-medium transition-colors",
              size === "md" ? "px-2.5 py-1 text-[13px]" : "px-2 py-0.5 text-xs",
              on ? "border-brand bg-brand-faint text-brand-on-subtle" : "border-line bg-elevated text-fg hover:bg-surface",
            )}
          >
            {o.label}
            {o.count != null && <span className={clsx("font-mono text-[11px]", on ? "opacity-80" : "text-fg-muted")}>{o.count.toLocaleString("en-GB")}</span>}
          </button>
        );
      })}
    </div>
  );
}

/** Today / 7 days / 30 days switch on Overview (P3). */
export function RangeTabs<T extends string>({ value, options, onChange, label }: { value: T; options: { value: T; label: string }[]; onChange: (v: T) => void; label: string }) {
  return (
    <div role="group" aria-label={label} className="flex gap-0.5 rounded-md bg-surface p-[3px] text-[13px]">
      {options.map((o) => (
        <button
          key={o.value}
          type="button"
          aria-pressed={o.value === value}
          onClick={() => onChange(o.value)}
          className={clsx("rounded-md px-3.5 py-[7px]", o.value === value ? "bg-elevated font-semibold shadow-[0_1px_2px_rgb(11_13_18/0.12)]" : "text-fg-muted hover:text-fg")}
        >
          {o.label}
        </button>
      ))}
    </div>
  );
}

export function Card({ children, className, as: Tag = "section" }: { children: ReactNode; className?: string; as?: "section" | "div" | "article" }) {
  return <Tag className={clsx("rounded-md border border-line bg-elevated", className)}>{children}</Tag>;
}

export function CardTitle({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <div className="flex items-center justify-between gap-3">
      <h2 className="text-[15px] font-semibold">{children}</h2>
      {action}
    </div>
  );
}

/** Usage meter: cobalt, amber above 80%, red at 100%. */
export function Meter({ value, max, label }: { value: number; max: number; label: string }) {
  const pct = max > 0 ? Math.min(100, (value / max) * 100) : 0;
  const tone = pct >= 100 ? "bg-block" : pct >= 80 ? "bg-review" : "bg-brand";
  const track = pct >= 80 && pct < 100 ? "bg-review-tint" : "bg-brand-faint";
  return (
    <div role="meter" aria-label={label} aria-valuemin={0} aria-valuemax={max} aria-valuenow={value} className={clsx("h-2 overflow-hidden rounded-full", track)}>
      <div className={clsx("h-full rounded-full", tone)} style={{ width: `${pct}%` }} />
    </div>
  );
}

/** Empty state (MoodPlatform): outline pills fill in, one primary action, no illustration. */
export function EmptyState({ title, body, action, filled = 1 }: { title: string; body?: ReactNode; action?: ReactNode; filled?: 0 | 1 | 2 | 3 }) {
  const pill = (i: number, x: number, y: number) =>
    i < filled ? (
      <rect key={i} x={x} y={y} width="24" height="9" rx="4.5" fill="var(--color-brand)" />
    ) : (
      <rect key={i} x={x} y={y} width="24" height="9" rx="4.5" fill="none" stroke="var(--color-mist)" strokeWidth="1.5" strokeDasharray="3 2" />
    );
  return (
    <div className="flex flex-col items-start gap-2.5 p-8">
      <svg width="56" height="56" viewBox="0 0 48 48" aria-hidden>
        {[pill(2, 18, 7), pill(1, 12, 19.5), pill(0, 6, 32)]}
      </svg>
      <h2 className="text-[17px] font-semibold">{title}</h2>
      {body && <p className="max-w-[52ch] text-[13.5px] text-fg-muted">{body}</p>}
      {action}
    </div>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="rounded-md border border-line bg-surface px-1.5 py-0.5 font-mono text-[11px] text-fg-muted">{children}</kbd>;
}

export function Field({ label, hint, error, children, className }: { label: string; hint?: ReactNode; error?: string | null; children: (id: string) => ReactNode; className?: string }) {
  const id = useId();
  return (
    <div className={clsx("flex flex-col gap-1.5", className)}>
      <label htmlFor={id} className="text-[13px] font-semibold">
        {label}
      </label>
      {children(id)}
      {error ? <p className="text-[12.5px] font-medium text-block-fg">{error}</p> : hint ? <p className="text-[12.5px] text-fg-muted">{hint}</p> : null}
    </div>
  );
}

export function Modal({ open, onClose, title, body, children, actions, width = 480 }: { open: boolean; onClose: () => void; title: string; body?: ReactNode; children?: ReactNode; actions: ReactNode; width?: number }) {
  const id = useId();
  return (
    <Dialog open={open} onClose={onClose} aria-labelledby={id} slotProps={{ paper: { sx: { width, maxWidth: "calc(100% - 32px)", m: 2 } } }}>
      <div className="flex flex-col gap-4 p-6">
        <div className="flex flex-col gap-1.5">
          <h2 id={id} className="text-[20px] font-semibold tracking-[-0.01em]">
            {title}
          </h2>
          {body && <div className="text-[14px] text-fg-muted">{body}</div>}
        </div>
        {children}
        <div className="flex flex-wrap justify-end gap-2">{actions}</div>
      </div>
    </Dialog>
  );
}

export function Confirm({ open, title, body, confirm, danger, onConfirm, onClose }: { open: boolean; title: string; body: ReactNode; confirm: string; danger?: boolean; onConfirm: () => void; onClose: () => void }) {
  return (
    <Modal
      open={open}
      onClose={onClose}
      title={title}
      body={body}
      actions={
        <>
          <Button variant="ghost" onClick={onClose}>
            Cancel
          </Button>
          <Button
            variant="primary"
            className={danger ? "!bg-block hover:!bg-block-fg" : undefined}
            onClick={() => {
              onConfirm();
              onClose();
            }}
          >
            {confirm}
          </Button>
        </>
      }
    />
  );
}

export function Skeleton({ className }: { className?: string }) {
  return <div aria-hidden className={clsx("animate-pulse rounded-lg bg-neutral-tint", className)} />;
}

export function ErrorNote({ text }: { text: string }) {
  return (
    <div role="alert" className="rounded-md bg-block-tint p-4 text-[14px] text-block-fg">
      {text}
    </div>
  );
}
