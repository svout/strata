import { isValidElement, type ReactElement, type ReactNode } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { cleanup, render, screen } from "@testing-library/react";
import Providers from "@/components/Providers";
import RootLayout from "@/app/layout";
import NotFound from "@/app/not-found";
import SignInPage from "@/app/sign-in/page";
import { PrototypeNotice } from "./PrototypeNotice";

// The sign-in page reads the router and the query string.
vi.mock("next/navigation", () => ({
  useRouter: () => ({ replace: vi.fn(), push: vi.fn() }),
  useSearchParams: () => new URLSearchParams(),
  usePathname: () => "/sign-in",
}));

// Vitest runs without globals, so Testing Library can't register its own cleanup
afterEach(cleanup);

const SIMULATED = "The data shown is simulated.";
const TODAY =
  "Prototype. The data shown is simulated. In the gateway, only payment creation and the double-entry ledger exist. Everything else on these screens is simulated, including mandate, signature, policy, risk and audit-chain behaviour.";

const noteText = () => screen.getByRole("note").textContent ?? "";

describe("PrototypeNotice", () => {
  it("says exactly this today, with no props", () => {
    render(<PrototypeNotice />);
    expect(noteText()).toBe(TODAY);
  });

  // Generated from the list of what exists, so an empty list claims nothing.
  it.each([
    { implemented: [], says: "Nothing in the gateway is built yet: everything on these screens is simulated." },
    { implemented: ["payment creation"], says: "only payment creation exists. Everything else" },
    { implemented: ["a", "b", "c"], says: "only a, b and c exist. Everything else" },
  ])("implemented $implemented", ({ implemented, says }) => {
    render(<PrototypeNotice sampleData implemented={implemented} />);
    expect(noteText()).toContain(says);
    expect(noteText().startsWith("Prototype.")).toBe(true);
  });

  // The data sentence stops being true at a different time from the engine
  // one, so it has its own condition; turning it off keeps the rest.
  it("drops only the data sentence once the mock is off", () => {
    render(<PrototypeNotice sampleData={false} />);
    expect(noteText()).not.toContain(SIMULATED);
    expect(noteText()).toBe(TODAY.replace(`${SIMULATED} `, ""));
  });
});

// "Every screen": the root layout wraps every route in Providers, Providers
// renders the notice once above the page, and two different pages rendered
// inside it (a client page that reads the router and the static 404) show
// exactly one.
describe("the notice on every screen", () => {
  it("the root layout wraps every route in Providers", () => {
    const html = RootLayout({ children: <p>page</p> }) as ReactElement<{ children: ReactNode }>;
    const body = html.props.children as ReactElement<{ children: ReactNode }>;
    const inner = body.props.children;
    expect(isValidElement(inner) && inner.type).toBe(Providers);
  });

  it.each([
    { route: "/sign-in", page: <SignInPage /> },
    { route: "not found", page: <NotFound /> },
  ])("$route shows exactly one notice, the full text", ({ page }) => {
    render(<Providers>{page}</Providers>);
    const notes = screen.getAllByRole("note");
    expect(notes).toHaveLength(1);
    expect(notes[0].textContent).toBe(TODAY);
  });
});
