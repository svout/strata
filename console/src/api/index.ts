import type { ConsoleApi } from "./client";
import { MockGateway } from "./mock/gateway";

/**
 * The one place that decides which implementation the console talks to.
 *
 * Today: the in-browser mock gateway (sample data, resets on reload).
 * Later: the generated control-plane client, chosen with
 * NEXT_PUBLIC_STRATA_API=http. Nothing outside this file needs to change.
 */
let instance: ConsoleApi | null = null;

export function getApi(): ConsoleApi {
  if (!instance) {
    const mode = process.env.NEXT_PUBLIC_STRATA_API ?? "mock";
    if (mode !== "mock") {
      throw new Error(`NEXT_PUBLIC_STRATA_API=${mode} isn't available yet: the control-plane client is generated in P7. Use "mock".`);
    }
    instance = new MockGateway();
  }
  return instance;
}

export const IS_SAMPLE_DATA = (process.env.NEXT_PUBLIC_STRATA_API ?? "mock") === "mock";

export type { ConsoleApi } from "./client";
export * from "./types";
