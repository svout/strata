import type { Money } from "@/api/types";

/** Pounds → Money in pence. Rounds to the nearest penny. */
export const gbp = (pounds: number): Money => ({ minor: Math.round(pounds * 100), currency: "GBP" });
export const zero: Money = { minor: 0, currency: "GBP" };
export const addMoney = (a: Money, b: Money): Money => ({ minor: a.minor + b.minor, currency: a.currency });

const fmt2 = new Intl.NumberFormat("en-GB", { style: "currency", currency: "GBP", minimumFractionDigits: 2 });
const fmt0 = new Intl.NumberFormat("en-GB", { style: "currency", currency: "GBP", maximumFractionDigits: 0 });

/** £1,234.50 */
export const money = (m: Money) => fmt2.format(m.minor / 100);
/** £1,235: for limits and caps, where pence are noise */
export const moneyShort = (m: Money) => fmt0.format(m.minor / 100);

const pad = (n: number) => String(n).padStart(2, "0");
export const clock = (t: number) => {
  const d = new Date(t);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
};
export const clockShort = (t: number) => {
  const d = new Date(t);
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`;
};
export const dayMonth = (t: number) => new Date(t).toLocaleDateString("en-GB", { day: "numeric", month: "short" });
export const dateLong = (t: number) => new Date(t).toLocaleDateString("en-GB", { day: "numeric", month: "long", year: "numeric" });

export function ago(t: number | null, now = Date.now()): string {
  if (t == null) return "never";
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 45) return "just now";
  const m = Math.round(s / 60);
  if (m < 60) return `${m} min ago`;
  const h = Math.round(m / 60);
  if (h < 24) return `${h} h ago`;
  const d = Math.round(h / 24);
  return d === 1 ? "yesterday" : `${d} days ago`;
}

/** 91ab…3e: enough of a hash to compare by eye */
export const shortHash = (h: string) => (h ? `${h.slice(0, 4)}…${h.slice(-2)}` : "—");

export const plural = (n: number, one: string, many = `${one}s`) => `${n.toLocaleString("en-GB")} ${n === 1 ? one : many}`;
