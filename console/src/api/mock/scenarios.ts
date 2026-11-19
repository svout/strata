import { gbp } from "@/lib/format";
import type { Merchant, Scenario } from "../types";
import type { PaymentRequest } from "./engine";

/**
 * The eight scenarios from Project/Threat Model, mapped onto the sample
 * workspace. The real harness (harness/, P6) sends these to the real
 * gateway; here they run against the mock so the attack lab works today.
 */
export const SCENARIOS: Scenario[] = [
  { n: 1, title: "Prompt-injected agent buys the wrong thing", attacker: "Manipulated agent", control: "Cart must be a subset of the intent mandate", expected: "block" },
  { n: 2, title: "Mandate replay", attacker: "Network attacker", control: "Single-use nonce and expiry", expected: "block" },
  { n: 3, title: "Split purchase: £40 at a time against a £150 cap", attacker: "Malicious agent", control: "Rolling-window velocity", expected: "review" },
  { n: 4, title: "Merchant spoof", attacker: "Hostile merchant", control: "Merchant identity bound to the checkout host", expected: "block" },
  { n: 5, title: "Stolen key used from a new context", attacker: "Malicious agent", control: "Context features in the risk score", expected: "review" },
  { n: 6, title: "Scope creep: travel mandate used for gift cards", attacker: "Manipulated agent", control: "Category scope in the intent mandate", expected: "block" },
  { n: 7, title: "Mule pattern: many agents, one new beneficiary", attacker: "Malicious agent", control: "Graph features in the risk model", expected: "review" },
  { n: 8, title: "Benign shopping traffic", attacker: "Control case", control: "Whole pipeline, false-positive check", expected: "approve" },
];

type Step = Omit<PaymentRequest, "id" | "at" | "source" | "scenario">;

export function scenarioRequests(n: number, merchant: (id: string) => Merchant): Step[] {
  const at = (id: string) => ({ merchantId: id, checkoutHost: merchant(id).host });
  switch (n) {
    case 1:
      return [{ agentId: "agt_travel-booker", ...at("mer_harbour"), amount: gbp(412 + 100), flags: { cartExtra: { name: "£100 gift card (added by text injected into the hotel page)", amount: gbp(100) } } }];
    case 2:
      return [{ agentId: "agt_travel-booker", ...at("mer_harbour"), amount: gbp(412), flags: { replay: true } }];
    case 3:
      // Up to six £40 refunds in a row; the run stops at the first one that isn't approved
      return Array.from({ length: 6 }, () => ({ agentId: "agt_support-refunds", ...at("mer_refunds"), amount: gbp(40), flags: {} }));
    case 4:
      return [{ agentId: "agt_travel-booker", merchantId: "mer_harbour", checkoutHost: "checkout.harb0ur-hotels.example", amount: gbp(389), flags: {} }];
    case 5:
      return [{ agentId: "agt_saas-renewals", ...at("mer_clouddesk"), amount: gbp(240), flags: { contextChange: true } }];
    case 6:
      return [{ agentId: "agt_travel-booker", ...at("mer_giftvault"), amount: gbp(120), flags: {} }];
    case 7:
      return [{ agentId: "agt_support-refunds", ...at("mer_refunds"), amount: gbp(95), flags: { newBeneficiary: true, fanIn: 6 } }];
    default:
      return [{ agentId: "agt_travel-booker", ...at("mer_railnow"), amount: gbp(46.2), flags: {} }];
  }
}
