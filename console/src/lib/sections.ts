import type { IconName } from "@/components/ui/icons";

/**
 * Where you are and what each page is for.
 *
 * Every page belongs to one section, and each section has a colour. The
 * colour appears in the sidebar and in the page header, so you can tell
 * where you are at a glance. These are wayfinding colours only: they
 * never mean an outcome. Approve / review / block keep the signal colours
 * (green, amber, red), and nothing else uses those.
 *
 * Each page has a one-line purpose (shown under the title) and up to three
 * short tips (behind the "?" button). Keep both short: if a tip needs a
 * second sentence, the screen itself should change instead.
 */
export type SectionKey = "monitor" | "control" | "record" | "admin" | "demo";

export const SECTIONS: Record<SectionKey, { label: string; blurb: string }> = {
  monitor: { label: "Monitor", blurb: "What's happening" },
  control: { label: "Control", blurb: "What agents may do" },
  record: { label: "Record", blurb: "Proof of what happened" },
  admin: { label: "Admin", blurb: "People and workspace" },
  demo: { label: "Demo", blurb: "Try the attacks" },
};

export type PageKey = "overview" | "decisions" | "review" | "mandates" | "mandate" | "agents" | "ledger" | "settings" | "attacks" | "dispute";

export const PAGES: Record<PageKey, { section: SectionKey; title: string; icon: IconName; href: string; purpose: string; tips: string[] }> = {
  overview: {
    section: "monitor",
    title: "Overview",
    icon: "overview",
    href: "/overview",
    purpose: "Is anything wrong right now?",
    tips: ["Amber means a person needs to decide.", "Red means Strata stopped a payment. No money moved.", "Click any bar to see those payments."],
  },
  decisions: {
    section: "monitor",
    title: "Decisions",
    icon: "decisions",
    href: "/decisions",
    purpose: "Every payment an agent tried, and why it went that way.",
    tips: ["Click a row to see the three checks it went through.", "Filter by outcome, agent or reason.", "Keyboard: J / K to move, A / D to approve or decline."],
  },
  review: {
    section: "monitor",
    title: "Review queue",
    icon: "review",
    href: "/review",
    purpose: "Payments waiting for a person to decide.",
    tips: ["Nothing moves until someone approves.", "Declining asks for a one-word reason.", "Keyboard: A approve, D decline, J / K next."],
  },
  mandates: {
    section: "control",
    title: "Mandates",
    icon: "mandates",
    href: "/mandates",
    purpose: "Spending rules: what each agent may buy, and how much.",
    tips: ["An agent can only spend inside an active mandate.", "Limits apply per payment, per day and per month.", "Above the review limit, a person decides."],
  },
  mandate: {
    section: "control",
    title: "Mandate",
    icon: "mandates",
    href: "/mandates",
    purpose: "Five questions decide what this agent may spend.",
    tips: ["The dark card reads the rule back in plain words.", "The impact check replays the last 30 days.", "Every save is a new version on the ledger."],
  },
  agents: {
    section: "control",
    title: "Agents",
    icon: "agents",
    href: "/agents",
    purpose: "Every AI agent that can ask to spend.",
    tips: ["Pause stops an agent's next payment instantly.", "Keys prove a request really came from this agent.", "A new key is shown once, then only its last 4 characters."],
  },
  ledger: {
    section: "record",
    title: "Ledger",
    icon: "ledger",
    href: "/ledger",
    purpose: "A tamper-evident record of everything that happened.",
    tips: ["Each entry is chained to the one before it.", "Verify recomputes every link in the chain.", "If anything was edited, the badge turns red and names it."],
  },
  settings: {
    section: "admin",
    title: "Settings",
    icon: "settings",
    href: "/settings",
    purpose: "People, roles and workspace details.",
    tips: ["Roles decide what each person can change.", "Agents don't have roles: their mandates limit them."],
  },
  attacks: {
    section: "demo",
    title: "Attack lab",
    icon: "shield",
    href: "/attack-lab",
    purpose: "Run the eight known attacks and watch Strata stop them.",
    tips: ["Each run is recorded like real traffic, tagged as an attack.", "Pausing an agent changes which check stops an attack.", "Sample data: the real test harness replaces this later."],
  },
  dispute: {
    section: "monitor",
    title: "Dispute view",
    icon: "decisions",
    href: "/decisions",
    purpose: "What happened to one payment, step by step.",
    tips: ["Rebuilt from the ledger, not from today's settings.", "Copy it as text to send to a merchant or auditor."],
  },
};

/** Sidebar order. */
export const NAV: { section: SectionKey; pages: PageKey[] }[] = [
  { section: "monitor", pages: ["overview", "decisions", "review"] },
  { section: "control", pages: ["mandates", "agents"] },
  { section: "record", pages: ["ledger"] },
  { section: "admin", pages: ["settings"] },
  { section: "demo", pages: ["attacks"] },
];

/** Tailwind classes per section (colours are CSS variables in globals.css, light and dark). */
export const SECTION_CLASS: Record<SectionKey, { text: string; tint: string; solid: string; ring: string }> = {
  monitor: { text: "text-sec-monitor", tint: "bg-sec-monitor-tint", solid: "bg-sec-monitor", ring: "border-sec-monitor" },
  control: { text: "text-sec-control", tint: "bg-sec-control-tint", solid: "bg-sec-control", ring: "border-sec-control" },
  record: { text: "text-sec-record", tint: "bg-sec-record-tint", solid: "bg-sec-record", ring: "border-sec-record" },
  admin: { text: "text-sec-admin", tint: "bg-sec-admin-tint", solid: "bg-sec-admin", ring: "border-sec-admin" },
  demo: { text: "text-sec-demo", tint: "bg-sec-demo-tint", solid: "bg-sec-demo", ring: "border-sec-demo" },
};
