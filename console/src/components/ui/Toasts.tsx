"use client";

import { createContext, useCallback, useContext, useState, type ReactNode } from "react";
import clsx from "clsx";

/**
 * Toast (UI kit): p14 · r14 · signal dot · at most one action.
 * Announced politely to screen readers.
 */
type Tone = "approve" | "review" | "block" | "info";
type Toast = { id: number; text: string; tone: Tone; action?: { label: string; run: () => void } };
type Push = (text: string, tone?: Tone, action?: Toast["action"]) => void;

const Ctx = createContext<Push>(() => {});
const DOT: Record<Tone, string> = { approve: "bg-approve", review: "bg-review", block: "bg-block", info: "bg-cobalt-400" };

export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<Toast[]>([]);
  const push = useCallback<Push>((text, tone = "approve", action) => {
    const id = Date.now() + Math.random();
    setToasts((t) => [...t.slice(-3), { id, text, tone, action }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 5200);
  }, []);
  return (
    <Ctx.Provider value={push}>
      {children}
      <div aria-live="polite" className="pointer-events-none fixed right-5 bottom-5 z-[1500] flex max-w-[min(420px,calc(100vw-40px))] flex-col gap-2">
        {toasts.map((t) => (
          <div key={t.id} className="pointer-events-auto flex items-start gap-2.5 rounded-md bg-ink p-3.5 text-[13.5px] text-white shadow-[var(--shadow-pop)]">
            <span aria-hidden className={clsx("mt-1.5 size-2 shrink-0 rounded-full", DOT[t.tone])} />
            <span className="flex-1">{t.text}</span>
            {t.action && (
              <button
                type="button"
                className="shrink-0 font-semibold underline underline-offset-2"
                onClick={() => {
                  t.action!.run();
                  setToasts((x) => x.filter((y) => y.id !== t.id));
                }}
              >
                {t.action.label}
              </button>
            )}
          </div>
        ))}
      </div>
    </Ctx.Provider>
  );
}

export const useToast = () => useContext(Ctx);
