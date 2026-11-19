"use client";

import Link from "next/link";
import { usePathname, useRouter } from "next/navigation";
import { useEffect, useState, type ReactNode } from "react";
import clsx from "clsx";
import Menu from "@mui/material/Menu";
import MenuItem from "@mui/material/MenuItem";
import Divider from "@mui/material/Divider";
import { Logo } from "@/components/brand";
import { Icon } from "@/components/ui/icons";
import Tooltip from "@mui/material/Tooltip";
import { NAV, PAGES, SECTIONS } from "@/lib/sections";
import { Kbd, Modal } from "@/components/ui";
import { RoleMatrix } from "@/components/ui/Allowed";
import Button from "@mui/material/Button";
import { useToast } from "@/components/ui/Toasts";
import { useSession } from "@/lib/session";
import { useResource, api } from "@/lib/useResource";
import { ROLE_INFO } from "@/lib/permissions";
import { money } from "@/lib/format";
import { IS_SAMPLE_DATA, type Role } from "@/api";
import CommandPalette from "./CommandPalette";

export default function AppShell({ children }: { children: ReactNode }) {
  const s = useSession();
  const router = useRouter();
  const path = usePathname();
  const [navOpen, setNavOpen] = useState(false);
  const [palette, setPalette] = useState(false);

  // Signed-out users go to P1; first visit goes through onboarding (P2)
  useEffect(() => {
    if (s.ready && !s.signedIn) {
      // Keep the deep link (e.g. /decisions?id=…) so sign-in can send the person back to it
      const here = `${window.location.pathname}${window.location.search}`;
      router.replace(`/sign-in?next=${encodeURIComponent(here)}`);
    }
  }, [s.ready, s.signedIn, router]);

  useEffect(() => setNavOpen(false), [path]);

  // ⌘K / Ctrl+K opens search from anywhere
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        setPalette((p) => !p);
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);

  if (!s.ready || !s.signedIn) return null;

  return (
    <div className="flex h-full">
      <a href="#main" className="sr-only focus:not-sr-only focus:absolute focus:top-2 focus:left-2 focus:z-50 focus:rounded-md focus:bg-elevated focus:p-2">
        Skip to content
      </a>
      <Sidebar open={navOpen} onSearch={() => setPalette(true)} />
      {navOpen && <button aria-label="Close navigation" className="fixed inset-0 z-30 bg-ink/30 lg:hidden" onClick={() => setNavOpen(false)} />}
      <div className="flex min-w-0 flex-1 flex-col">
        <header className="flex h-14 shrink-0 items-center gap-3 border-b border-line bg-surface px-4 lg:hidden">
          <button type="button" aria-label="Open navigation" className="grid size-10 place-items-center rounded-full" onClick={() => setNavOpen(true)}>
            <Icon name="menu" />
          </button>
          <Logo size={24} />
          <button type="button" aria-label="Search" className="ml-auto grid size-10 place-items-center rounded-full" onClick={() => setPalette(true)}>
            <Icon name="search" />
          </button>
        </header>
        {s.role === "viewer" && (
          <div className="flex flex-wrap items-center gap-3 border-b border-line bg-brand-faint px-6 py-2.5 text-[13.5px] text-brand-on-subtle">
            <Icon name="user" size={16} />
            You&apos;re viewing as a Viewer: you can see everything and change nothing.
            <button type="button" className="ml-auto font-semibold underline underline-offset-2" onClick={() => s.setRole("owner")}>
              Back to Owner
            </button>
          </div>
        )}
        <main id="main" className="min-h-0 flex-1 overflow-auto">
          {children}
        </main>
      </div>
      <CommandPalette open={palette} onClose={() => setPalette(false)} />
      <LiveTraffic />
    </div>
  );
}

function Sidebar({ open, onSearch }: { open: boolean; onSearch: () => void }) {
  const path = usePathname();
  const s = useSession();
  const router = useRouter();
  const toast = useToast();
  const { data: queue } = useResource("queue", (a) => a.reviewQueue());
  const { data: ws } = useResource("workspace", (a) => a.workspace());
  const [wsMenu, setWsMenu] = useState<HTMLElement | null>(null);
  const [userMenu, setUserMenu] = useState<HTMLElement | null>(null);
  const [rolesOpen, setRolesOpen] = useState(false);
  const waiting = queue?.length ?? 0;

  return (
    <aside
      aria-label="Main navigation"
      className={clsx(
        "fixed inset-y-0 left-0 z-40 flex w-[232px] shrink-0 flex-col border-r border-line bg-surface px-3 py-[18px] transition-transform lg:static lg:translate-x-0",
        open ? "translate-x-0 shadow-[var(--shadow-pop)]" : "-translate-x-full",
      )}
    >
      <Link href="/overview" className="px-2 pb-3.5">
        <Logo size={28} />
      </Link>
      <button
        type="button"
        onClick={(e) => setWsMenu(e.currentTarget)}
        aria-haspopup="menu"
        className="flex h-[38px] items-center justify-between rounded-md border border-line bg-elevated px-2.5 text-[13px] font-semibold hover:border-fg-muted"
      >
        <span className="truncate">{ws?.name ?? "…"}</span>
        <Icon name="chevronDown" size={16} className="text-fg-muted" />
      </button>
      <Menu anchorEl={wsMenu} open={!!wsMenu} onClose={() => setWsMenu(null)} slotProps={{ paper: { sx: { minWidth: 208 } } }}>
        <MenuItem selected onClick={() => setWsMenu(null)}>
          {ws?.name}
        </MenuItem>
        <Divider />
        <MenuItem
          onClick={() => {
            setWsMenu(null);
            router.push("/onboarding");
          }}
        >
          Create workspace
        </MenuItem>
      </Menu>
      <button
        type="button"
        onClick={onSearch}
        className="mt-2 flex h-[34px] items-center gap-2 rounded-md px-2.5 text-[13px] text-fg-muted hover:bg-neutral-tint"
      >
        <Icon name="search" size={16} />
        Search
        <span className="ml-auto">
          <Kbd>⌘K</Kbd>
        </span>
      </button>

      <nav className="mt-1 flex min-h-0 flex-1 flex-col overflow-y-auto">
        {NAV.map((g) => {
          return (
            <div key={g.section} className="flex flex-col gap-0.5">
              <p className="px-2.5 pt-4 pb-1 text-[12px] font-semibold text-fg-subtle">{SECTIONS[g.section].label}</p>
              {g.pages.map((key) => {
                const it = PAGES[key];
                const active = path === it.href || path.startsWith(`${it.href}/`) || (key === "decisions" && path.startsWith("/disputes"));
                return (
                  <Tooltip key={key} title={it.purpose} placement="right" enterDelay={600} arrow>
                    <Link
                      href={it.href}
                      aria-current={active ? "page" : undefined}
                      className={clsx("flex items-center gap-2.5 rounded-md px-2.5 py-1.5 text-[14px] font-medium", active ? "bg-brand-faint text-brand" : "text-fg hover:bg-neutral-tint")}
                    >
                      <Icon name={it.icon} size={16} className={active ? "" : "text-fg-subtle"} />
                      {it.title}
                      {key === "review" && waiting > 0 && (
                        <span className="ml-auto grid h-[18px] min-w-[18px] place-items-center rounded-sm bg-review-tint px-1 text-[11px] font-semibold text-review-fg" aria-label={`${waiting} waiting`}>
                          {waiting}
                        </span>
                      )}
                    </Link>
                  </Tooltip>
                );
              })}
            </div>
          );
        })}
      </nav>

      {IS_SAMPLE_DATA && (
        <div className="mx-1 mb-2 flex flex-col gap-2 rounded-md border border-dashed border-mist p-2.5 text-[12px] text-fg-muted">
          <span className="text-[12px] font-semibold">Sample data</span>
          <label className="flex cursor-pointer items-center gap-2 text-fg">
            <input type="checkbox" className="size-4 accent-[var(--color-brand)]" checked={s.live} onChange={(e) => s.setLive(e.target.checked)} />
            Live agent traffic
            {s.live && <span aria-hidden className="ml-auto size-2 animate-pulse-dot rounded-full bg-approve" />}
          </label>
        </div>
      )}

      <button
        type="button"
        onClick={(e) => setUserMenu(e.currentTarget)}
        aria-haspopup="menu"
        className="flex items-center gap-2.5 rounded-md p-2 text-left hover:bg-neutral-tint"
      >
        <span className="grid size-[30px] shrink-0 place-items-center rounded-full bg-ink text-[12px] font-semibold text-white dark:bg-cobalt-500">{s.user.initials}</span>
        <span className="flex min-w-0 flex-col">
          <span className="truncate text-[13px] font-semibold">{s.user.name}</span>
          <span className="text-[12px] text-fg-muted">{ROLE_INFO[s.role].label}</span>
        </span>
        <Icon name="chevronDown" size={16} className="ml-auto text-fg-muted" />
      </button>
      <Menu
        anchorEl={userMenu}
        open={!!userMenu}
        onClose={() => setUserMenu(null)}
        anchorOrigin={{ vertical: "top", horizontal: "left" }}
        transformOrigin={{ vertical: "bottom", horizontal: "left" }}
        slotProps={{ paper: { sx: { minWidth: 232 } } }}
      >
        <p className="px-4 pt-1 pb-1.5 text-[12px] font-semibold text-fg-muted">View as (demo)</p>
        {(Object.keys(ROLE_INFO) as Role[]).map((r) => (
          <MenuItem
            key={r}
            selected={s.role === r}
            onClick={() => {
              s.setRole(r);
              setUserMenu(null);
            }}
          >
            {ROLE_INFO[r].label}
            {s.role === r && <Icon name="check" size={16} className="ml-auto text-brand" />}
          </MenuItem>
        ))}
        <MenuItem
          onClick={() => {
            setUserMenu(null);
            setRolesOpen(true);
          }}
        >
          <Icon name="user" size={16} className="mr-2.5" />
          What can each role do?
        </MenuItem>
        <Divider />
        <MenuItem
          onClick={() => {
            s.setTheme(s.theme === "dark" ? "light" : "dark");
            setUserMenu(null);
          }}
        >
          <Icon name={s.theme === "dark" ? "sun" : "moon"} size={16} className="mr-2.5" />
          {s.theme === "dark" ? "Light mode" : "Dark mode"}
        </MenuItem>
        {IS_SAMPLE_DATA && (
          <MenuItem
            onClick={async () => {
              setUserMenu(null);
              await api().reset();
              toast("Sample data reset to its starting state.", "info");
            }}
          >
            <Icon name="refresh" size={16} className="mr-2.5" />
            Reset sample data
          </MenuItem>
        )}
        <MenuItem
          onClick={() => {
            setUserMenu(null);
            s.signOut();
            router.replace("/sign-in");
          }}
        >
          <Icon name="out" size={16} className="mr-2.5" />
          Sign out
        </MenuItem>
      </Menu>
      <Modal
        open={rolesOpen}
        onClose={() => setRolesOpen(false)}
        width={640}
        title="What each role can do"
        body={`You're signed in as ${s.role === "owner" || s.role === "admin" ? "an" : "a"} ${ROLE_INFO[s.role].label}. ${ROLE_INFO[s.role].text}`}
        actions={
          <>
            <Button variant="ghost" href="/settings?tab=members" onClick={() => setRolesOpen(false)}>
              Manage members
            </Button>
            <Button variant="primary" onClick={() => setRolesOpen(false)}>
              Done
            </Button>
          </>
        }
      >
        <RoleMatrix />
      </Modal>
    </aside>
  );
}

/** Simulated agent traffic while "Live agent traffic" is on (sample data only). */
function LiveTraffic() {
  const s = useSession();
  const toast = useToast();
  const router = useRouter();
  useEffect(() => {
    if (!s.live || !IS_SAMPLE_DATA) return;
    const t = setInterval(async () => {
      const d = await api().simulateTraffic();
      if (d.outcome !== "approve") {
        toast(`${d.outcome === "block" ? "Blocked" : "Held for review"}: ${d.agentId}, ${money(d.amount)}`, d.outcome, {
          label: "Open",
          run: () => router.push(`/decisions?id=${d.id}`),
        });
      }
    }, 7000);
    return () => clearInterval(t);
  }, [s.live, toast, router]);
  return null;
}
