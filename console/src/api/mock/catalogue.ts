import type { Category, Merchant, Role, Weekday } from "../types";

/**
 * Sample workspace for the mock gateway. Names follow the canvas boards
 * (P3–P10). Every host is under .example (RFC 2606) so nothing here can be
 * mistaken for a real merchant.
 */

export const MERCHANTS: (Merchant & { typical: [number, number] })[] = [
  { id: "mer_skylark", name: "SkyLark Air", category: "flights", host: "pay.skylarkair.example", typical: [90, 650] },
  { id: "mer_harbour", name: "Harbour Hotels", category: "hotels", host: "book.harbourhotels.example", typical: [80, 420] },
  { id: "mer_railnow", name: "RailNow", category: "rail", host: "checkout.railnow.example", typical: [18, 140] },
  { id: "mer_northwind", name: "Northwind Supplies", category: "office supplies", host: "shop.northwind.example", typical: [30, 900] },
  { id: "mer_papermill", name: "Papermill Direct", category: "office supplies", host: "orders.papermill.example", typical: [20, 400] },
  { id: "mer_brightline", name: "Brightline Components", category: "components", host: "orders.brightline.example", typical: [200, 4200] },
  { id: "mer_kestrel", name: "Kestrel Parts", category: "components", host: "pay.kestrelparts.example", typical: [150, 2500] },
  { id: "mer_clouddesk", name: "CloudDesk", category: "software", host: "billing.clouddesk.example", typical: [49, 299] },
  { id: "mer_pagewise", name: "Pagewise", category: "software", host: "billing.pagewise.example", typical: [12, 99] },
  { id: "mer_venue", name: "Venue Collective", category: "events", host: "book.venuecollective.example", typical: [300, 1800] },
  { id: "mer_refunds", name: "Customer refunds", category: "refunds", host: "refunds.strata-demo.example", typical: [5, 150] },
  { id: "mer_adwell", name: "Adwell Ads", category: "advertising", host: "billing.adwell.example", typical: [100, 900] },
  { id: "mer_lumen", name: "Lumen Electronics", category: "electronics", host: "pay.lumen.example", typical: [60, 1400] },
  { id: "mer_giftvault", name: "GiftVault", category: "gift cards", host: "checkout.giftvault.example", typical: [25, 250] },
  { id: "mer_freshcart", name: "FreshCart", category: "groceries", host: "pay.freshcart.example", typical: [22, 110] },
];

export const CATEGORIES: Category[] = [
  "flights",
  "hotels",
  "rail",
  "office supplies",
  "components",
  "software",
  "events",
  "refunds",
  "advertising",
  "electronics",
  "groceries",
  "gift cards",
];

export const PEOPLE: { id: string; name: string; email: string; role: Role; initials: string }[] = [
  { id: "usr_ivan", name: "Ivan Kovalchuk", email: "ivan@strata-demo.example", role: "owner", initials: "IK" },
  { id: "usr_maya", name: "Maya Robinson", email: "maya@strata-demo.example", role: "admin", initials: "MR" },
  { id: "usr_tom", name: "Tom Adeyemi", email: "tom@strata-demo.example", role: "member", initials: "TA" },
  { id: "usr_priya", name: "Priya Shah", email: "priya@strata-demo.example", role: "viewer", initials: "PS" },
];

type AgentSeed = {
  id: string;
  description: string;
  owner: string;
  createdDaysAgo: number;
  createdBy: string;
  paused?: string;
  /** Requests per day in the synthetic traffic, and which merchants it uses. */
  perDay: number;
  weekdaysOnly?: boolean;
  merchants: string[];
};

export const AGENTS: AgentSeed[] = [
  { id: "agt_travel-booker", description: "Books travel for the sales team", owner: "Sales ops", createdDaysAgo: 42, createdBy: "Ivan Kovalchuk", perDay: 12, merchants: ["mer_skylark", "mer_harbour", "mer_railnow", "mer_venue"] },
  { id: "agt_procure-bot", description: "Reorders office supplies and parts", owner: "Finance", createdDaysAgo: 39, createdBy: "Ivan Kovalchuk", perDay: 8, weekdaysOnly: true, merchants: ["mer_northwind", "mer_papermill", "mer_brightline", "mer_kestrel"] },
  { id: "agt_saas-renewals", description: "Renews the team's software subscriptions", owner: "Engineering", createdDaysAgo: 30, createdBy: "Maya Robinson", perDay: 3, merchants: ["mer_clouddesk", "mer_pagewise"] },
  { id: "agt_support-refunds", description: "Issues small customer refunds", owner: "Support", createdDaysAgo: 24, createdBy: "Maya Robinson", perDay: 9, merchants: ["mer_refunds"] },
  { id: "agt_marketing", description: "Tops up ad accounts", owner: "Marketing", createdDaysAgo: 20, createdBy: "Maya Robinson", paused: "Maya Robinson", perDay: 0.4, merchants: ["mer_adwell"] },
];

/** Requests per day signed by a key nobody registered. */
export const UNKNOWN_AGENT_PER_DAY = 0.6;

const WEEKDAYS: Weekday[] = [0, 1, 2, 3, 4];
const EVERY_DAY: Weekday[] = [0, 1, 2, 3, 4, 5, 6];

type MandateSeed = {
  id: string;
  name: string;
  agentId: string;
  categories: Category[];
  merchantIds: string[] | null;
  perPayment: number;
  perDay: number;
  perMonth: number;
  days: Weekday[];
  hours: { from: string; to: string } | null;
  reviewAbove: number | null;
  paused?: boolean;
  /** Oldest first. v1 is always "Created". */
  history: { daysAgo: number; by: string; change: string }[];
};

export const MANDATES: MandateSeed[] = [
  {
    id: "mnd_travel", name: "Travel & accommodation", agentId: "agt_travel-booker",
    categories: ["flights", "hotels", "rail"], merchantIds: null,
    perPayment: 800, perDay: 5000, perMonth: 60000, days: EVERY_DAY, hours: null, reviewAbove: 500,
    history: [
      { daysAgo: 42, by: "Ivan Kovalchuk", change: "Created" },
      { daysAgo: 25, by: "Maya Robinson", change: "Added rail; daily limit £2,000 → £5,000" },
      { daysAgo: 11, by: "Ivan Kovalchuk", change: "Review threshold £800 → £500" },
    ],
  },
  {
    id: "mnd_procure", name: "Procurement", agentId: "agt_procure-bot",
    categories: ["office supplies", "components"], merchantIds: ["mer_northwind", "mer_papermill", "mer_brightline", "mer_kestrel"],
    perPayment: 12000, perDay: 30000, perMonth: 400000, days: WEEKDAYS, hours: { from: "07:00", to: "20:00" }, reviewAbove: 5000,
    history: [
      { daysAgo: 39, by: "Ivan Kovalchuk", change: "Created" },
      { daysAgo: 31, by: "Ivan Kovalchuk", change: "Added Brightline Components and Kestrel Parts" },
      { daysAgo: 18, by: "Ivan Kovalchuk", change: "Weekdays only" },
      { daysAgo: 6, by: "Maya Robinson", change: "Time window 08:00–19:00 → 07:00–20:00" },
    ],
  },
  {
    id: "mnd_saas", name: "SaaS renewals", agentId: "agt_saas-renewals",
    categories: ["software"], merchantIds: null,
    perPayment: 1500, perDay: 3000, perMonth: 8000, days: EVERY_DAY, hours: null, reviewAbove: 500,
    history: [{ daysAgo: 30, by: "Maya Robinson", change: "Created" }],
  },
  {
    id: "mnd_events", name: "Team events", agentId: "agt_travel-booker",
    categories: ["events"], merchantIds: null,
    perPayment: 1500, perDay: 3000, perMonth: 3000, days: EVERY_DAY, hours: null, reviewAbove: 1000,
    history: [{ daysAgo: 16, by: "Ivan Kovalchuk", change: "Created" }],
  },
  {
    id: "mnd_refunds", name: "Customer refunds", agentId: "agt_support-refunds",
    categories: ["refunds"], merchantIds: ["mer_refunds"],
    perPayment: 150, perDay: 1500, perMonth: 15000, days: EVERY_DAY, hours: null, reviewAbove: 120,
    history: [
      { daysAgo: 24, by: "Maya Robinson", change: "Created" },
      { daysAgo: 9, by: "Maya Robinson", change: "Daily limit £1,000 → £1,500" },
    ],
  },
  {
    id: "mnd_ads", name: "Ad spend", agentId: "agt_marketing",
    categories: ["advertising"], merchantIds: ["mer_adwell"],
    perPayment: 2000, perDay: 2000, perMonth: 8000, days: EVERY_DAY, hours: null, reviewAbove: null, paused: true,
    history: [
      { daysAgo: 20, by: "Maya Robinson", change: "Created" },
      { daysAgo: 4, by: "Maya Robinson", change: "Paused" },
    ],
  },
];
