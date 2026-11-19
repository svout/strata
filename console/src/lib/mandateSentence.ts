import type { Mandate, MandateInput, Merchant } from "@/api/types";
import { moneyShort } from "./format";
import { windowText } from "@/api/mock/engine";

/**
 * The plain-words sentence for a spending rule. The same sentence appears
 * on the Mandates cards (P6), in the editor's live summary (P7) and in
 * onboarding (P2), "so people always read rules the same way" (P2 board).
 */
export function mandateSentence(m: MandateInput | Mandate, merchants: Merchant[], reviewer = "a person"): string {
  const cats = list(m.categories.map(String)) || "nothing yet";
  const where = m.merchantIds
    ? m.merchantIds.length
      ? ` from ${list(m.merchantIds.map((id) => merchants.find((x) => x.id === id)?.name ?? id))}`
      : " from no merchants yet"
    : "";
  const when = m.days.length === 7 && !m.hours ? "" : ` on ${windowText(m)}`;
  const limits = `up to ${moneyShort(m.limits.perPayment)} each, ${moneyShort(m.limits.perDay)} a day and ${moneyShort(m.limits.perMonth)} a month`;
  const review = m.review.enabled ? ` Anything over ${moneyShort(m.review.above)} waits for ${reviewer}.` : " Nothing waits for a person.";
  return `${m.agentId} may buy ${cats}${where}${when}, ${limits}.${review}`;
}

function list(items: string[]): string {
  if (items.length <= 1) return items[0] ?? "";
  return `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}
