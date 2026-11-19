"use client";

import { useEffect, useMemo, useState } from "react";
import { useRouter } from "next/navigation";
import Dialog from "@mui/material/Dialog";
import clsx from "clsx";
import { api } from "@/lib/useResource";
import { money } from "@/lib/format";
import type { Agent, Decision, Mandate } from "@/api";

type Item = { kind: string; label: string; href: string };

const PAGES: Item[] = [
  ["Overview", "/overview"],
  ["Decisions", "/decisions"],
  ["Review queue", "/review"],
  ["Mandates", "/mandates"],
  ["New mandate", "/mandates/new"],
  ["Agents", "/agents"],
  ["Ledger", "/ledger"],
  ["Settings", "/settings"],
  ["Attack lab", "/attack-lab"],
].map(([label, href]) => ({ kind: "Page", label, href }));

/** ⌘K: jump to a page, agent, mandate or payment (ID, merchant or amount). */
export default function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const router = useRouter();
  const [q, setQ] = useState("");
  const [idx, setIdx] = useState(0);
  const [agents, setAgents] = useState<Agent[]>([]);
  const [mandates, setMandates] = useState<Mandate[]>([]);
  const [payments, setPayments] = useState<Decision[]>([]);

  useEffect(() => {
    if (!open) return;
    setQ("");
    setIdx(0);
    api().agents().then(setAgents);
    api().mandates().then(setMandates);
  }, [open]);

  useEffect(() => {
    if (!open || q.trim().length < 2) {
      setPayments([]);
      return;
    }
    let alive = true;
    api()
      .decisions({ q })
      .then((d) => alive && setPayments(d.slice(0, 8)));
    return () => {
      alive = false;
    };
  }, [q, open]);

  const items = useMemo(() => {
    const needle = q.trim().toLowerCase();
    const all: Item[] = [
      ...PAGES,
      ...agents.map((a) => ({ kind: "Agent", label: `${a.id} · ${a.description}`, href: `/agents?id=${a.id}` })),
      ...mandates.map((m) => ({ kind: "Mandate", label: `${m.name} · ${m.agentId}`, href: `/mandates/${m.id}` })),
    ];
    const matched = needle ? all.filter((i) => i.label.toLowerCase().includes(needle)) : PAGES;
    return [...matched, ...payments.map((d) => ({ kind: "Payment", label: `${d.id} · ${d.agentId} · ${money(d.amount)}`, href: `/decisions?id=${d.id}` }))].slice(0, 14);
  }, [q, agents, mandates, payments]);

  const go = (i: Item | undefined) => {
    if (!i) return;
    onClose();
    router.push(i.href);
  };

  return (
    <Dialog
      open={open}
      onClose={onClose}
      aria-label="Search"
      slotProps={{ paper: { sx: { width: 620, maxWidth: "calc(100% - 32px)", alignSelf: "flex-start", mt: "12vh", borderRadius: "16px" } } }}
    >
      <input
        autoFocus
        aria-label="Search payments, agents, mandates and pages"
        placeholder="Search payments, agents, mandates, pages…"
        value={q}
        onChange={(e) => {
          setQ(e.target.value);
          setIdx(0);
        }}
        onKeyDown={(e) => {
          if (e.key === "ArrowDown" || e.key === "ArrowUp") {
            e.preventDefault();
            setIdx((i) => (i + (e.key === "ArrowDown" ? 1 : -1) + items.length) % Math.max(1, items.length));
          }
          if (e.key === "Enter") go(items[idx]);
        }}
        className="h-[54px] w-full border-b border-line bg-transparent px-5 text-[16px] outline-none"
      />
      <ul role="listbox" aria-label="Results" className="max-h-[56vh] overflow-auto p-1.5">
        {items.length === 0 && <li className="p-4 text-fg-muted">Nothing matches “{q}”.</li>}
        {items.map((it, i) => (
          <li key={`${it.kind}-${it.href}`} role="option" aria-selected={i === idx}>
            <button
              type="button"
              onMouseEnter={() => setIdx(i)}
              onClick={() => go(it)}
              className={clsx("flex w-full items-center gap-3 rounded-md px-3 py-2.5 text-left text-[14px]", i === idx && "bg-brand-subtle text-brand-on-subtle")}
            >
              <span className="truncate">{it.label}</span>
              <span className="ml-auto shrink-0 text-[12px] text-fg-muted">{it.kind}</span>
            </button>
          </li>
        ))}
      </ul>
    </Dialog>
  );
}
