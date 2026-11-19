import type { ReasonCode } from "./types";

/**
 * Short labels for reason codes. Part of the API contract: the real
 * control plane will publish the same table.
 */
export const REASON_LABEL: Record<ReasonCode, string> = {
  UNKNOWN_AGENT_KEY: "Unknown agent key",
  SIGNATURE_INVALID: "Bad signature",
  AGENT_PAUSED: "Agent paused",
  NO_MANDATE: "No mandate",
  NONCE_REPLAYED: "Mandate replayed",
  HOST_MISMATCH: "Merchant host mismatch",
  CATEGORY_OUT_OF_SCOPE: "Category not allowed",
  MERCHANT_NOT_IN_SCOPE: "Merchant not allowed",
  CART_EXCEEDS_INTENT: "Cart outside intent",
  PER_PAYMENT_CAP: "Over per-payment cap",
  DAILY_LIMIT: "Over daily limit",
  MONTHLY_LIMIT: "Over monthly limit",
  OUTSIDE_TIME_WINDOW: "Outside allowed time",
  SPLIT_PURCHASE: "Split purchase",
  ABOVE_REVIEW_THRESHOLD: "Over review threshold",
  AMOUNT_DEVIATION: "Unusual amount",
  NEW_BENEFICIARY: "New beneficiary",
  CONTEXT_CHANGE: "New context for key",
  GRAPH_FANIN: "Many agents, one payee",
  SCORING_DEGRADED: "Rules-only scoring",
  WITHIN_MANDATE: "Within mandate",
};
