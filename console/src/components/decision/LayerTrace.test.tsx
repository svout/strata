import { afterEach, describe, expect, it } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import { MockGateway } from "@/api/mock/gateway";
import { mandateSentence } from "@/lib/mandateSentence";
import { LayerTrace, layersOf } from "./LayerTrace";

const NOW = new Date("2026-11-13T21:00:00Z").getTime();

// Vitest runs without globals, so Testing Library can't register its own cleanup
afterEach(cleanup);

describe("LayerTrace", () => {
  it("shows the three layers in order with the reason, for a blocked payment", async () => {
    const g = new MockGateway(1, () => NOW);
    const d = await g.runScenario(4); // merchant spoof
    render(<LayerTrace layers={layersOf(d)} />);
    const items = screen.getAllByRole("listitem");
    expect(items.map((li) => li.textContent?.split(" · ")[0])).toEqual(["Mandate", "Risk", "Ledger"]);
    expect(items[0].textContent).toContain("failed");
    expect(items[0].textContent).toContain("checkout.harb0ur-hotels.example");
    expect(items[1].textContent).toContain("not reached");
    expect(items[2].textContent).toMatch(/recorded#\d+/);
  });

  it("hides the layers a run hasn't reached yet", async () => {
    const d = await new MockGateway(1, () => NOW).runScenario(8);
    render(<LayerTrace layers={layersOf(d)} upTo={0} />);
    const items = screen.getAllByRole("listitem");
    expect(items[1].textContent).toBe("Risk · waiting");
    expect(items[2].textContent).toBe("Ledger · waiting");
  });
});

describe("mandateSentence", () => {
  it("reads a rule the way the design boards write it", async () => {
    const g = new MockGateway(1, () => NOW);
    const m = (await g.mandate("mnd_procure"))!;
    expect(mandateSentence(m, await g.merchants())).toBe(
      "agt_procure-bot may buy office supplies and components from Northwind Supplies, Papermill Direct, Brightline Components and Kestrel Parts on weekdays, 07:00–20:00, up to £12,000 each, £30,000 a day and £400,000 a month. Anything over £5,000 waits for a person.",
    );
  });
});
