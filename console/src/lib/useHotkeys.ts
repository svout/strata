"use client";

import { useEffect, useRef } from "react";

/**
 * Keyboard-first (Platform Screens principle 4): J/K rows, A approve,
 * D decline, ⌘K search. Single keys are ignored while typing in a field or
 * while a dialog is open, so they never fire by accident.
 */
export function useHotkeys(map: Record<string, (e: KeyboardEvent) => void>, enabled = true) {
  const ref = useRef(map);
  ref.current = map;
  useEffect(() => {
    if (!enabled) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.metaKey || e.ctrlKey || e.altKey) return;
      const t = e.target as HTMLElement | null;
      if (t && (t.isContentEditable || /^(INPUT|TEXTAREA|SELECT)$/.test(t.tagName))) return;
      if (document.querySelector(".MuiDialog-root, [role='dialog'][aria-modal='true']")) return;
      const fn = ref.current[e.key.toLowerCase()] ?? ref.current[e.key];
      if (fn) {
        e.preventDefault();
        fn(e);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [enabled]);
}
