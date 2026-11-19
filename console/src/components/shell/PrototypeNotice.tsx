import { IS_SAMPLE_DATA } from "@/api";
import { GATEWAY_IMPLEMENTED } from "@/config/prototype";

/** "a", "a and b", "a, b and c". */
function joinList(items: readonly string[]): string {
  return items.length < 2 ? items.join("") : `${items.slice(0, -1).join(", ")} and ${items[items.length - 1]}`;
}

/**
 * The honesty rule (SPEC-002 18, P7): every screen says what isn't real.
 * The engine sentence is generated from GATEWAY_IMPLEMENTED, a list of what
 * exists, and declares everything else simulated; the named examples after
 * "including" are illustrations, not a complete list. Left stale, they only
 * call something simulated that has become real, which is the safe direction;
 * drop an example in the step that ships it. The data sentence has its own condition (the mock in
 * use) because it stops being true at a different time. It sits in the page
 * flow at the top, not floating, so it can't cover content and a screenshot
 * of a screen includes it.
 */
export function PrototypeNotice({
  sampleData = IS_SAMPLE_DATA,
  implemented = GATEWAY_IMPLEMENTED,
}: {
  sampleData?: boolean;
  implemented?: readonly string[];
}) {
  const engine =
    implemented.length === 0
      ? "Nothing in the gateway is built yet: everything on these screens is simulated."
      : `In the gateway, only ${joinList(implemented)} exist${implemented.length === 1 ? "s" : ""}. Everything else on these screens is simulated, including mandate, signature, policy, risk and audit-chain behaviour.`;
  return (
    <div role="note" aria-label="About this console" className="shrink-0 border-b border-dashed border-line bg-surface px-4 py-1.5 text-center text-[12px] text-fg-muted">
      <strong className="font-semibold">Prototype.</strong>{" "}
      {sampleData && <span>The data shown is simulated. </span>}
      <span>{engine}</span>
    </div>
  );
}
