"use client";

import { createContext, useCallback, useContext, useEffect, useMemo, useState, type ReactNode } from "react";
import type { Role } from "@/api/types";
import { can as canDo, type Action } from "./permissions";

/**
 * Who is using the console, and their view preferences.
 *
 * Sign-in is simulated until auth lands (Google / GitHub, P1 board). The
 * "view as" role switch exists so the role rules can be demonstrated; with
 * real auth the role comes from the server and the switch goes away.
 * Preferences live in localStorage: one browser, one person.
 */
type Session = {
  ready: boolean;
  signedIn: boolean;
  onboarded: boolean;
  user: { name: string; initials: string; email: string };
  role: Role;
  theme: "light" | "dark";
  live: boolean;
  signIn: () => void;
  signOut: () => void;
  finishOnboarding: () => void;
  setRole: (r: Role) => void;
  setTheme: (t: "light" | "dark") => void;
  setLive: (on: boolean) => void;
  can: (a: Action) => boolean;
};

const KEY = "strata.console.session";
type Stored = { signedIn: boolean; onboarded: boolean; role: Role; theme: "light" | "dark"; live: boolean };
const DEFAULTS: Stored = { signedIn: false, onboarded: false, role: "owner", theme: "light", live: false };

const Ctx = createContext<Session | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<Stored>(DEFAULTS);
  const [ready, setReady] = useState(false);

  useEffect(() => {
    try {
      const raw = localStorage.getItem(KEY);
      if (raw) setState({ ...DEFAULTS, ...(JSON.parse(raw) as Partial<Stored>) });
    } catch {
      // Storage can be unavailable (private mode); defaults still work
    }
    setReady(true);
  }, []);

  useEffect(() => {
    if (!ready) return;
    try {
      localStorage.setItem(KEY, JSON.stringify(state));
    } catch {
      /* see above */
    }
    document.documentElement.dataset.theme = state.theme;
  }, [state, ready]);

  const patch = useCallback((p: Partial<Stored>) => setState((s) => ({ ...s, ...p })), []);

  const value = useMemo<Session>(
    () => ({
      ready,
      ...state,
      user: { name: "Ivan Kovalchuk", initials: "IK", email: "ivan@strata-demo.example" },
      signIn: () => patch({ signedIn: true }),
      signOut: () => patch({ signedIn: false }),
      finishOnboarding: () => patch({ onboarded: true }),
      setRole: (role) => patch({ role }),
      setTheme: (theme) => patch({ theme }),
      setLive: (live) => patch({ live }),
      can: (a) => canDo(state.role, a),
    }),
    [ready, state, patch],
  );

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>;
}

export function useSession() {
  const s = useContext(Ctx);
  if (!s) throw new Error("useSession must be used inside SessionProvider");
  return s;
}
