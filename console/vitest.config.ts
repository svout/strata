import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Component tests run in jsdom; the mock gateway tests are plain TS.
export default defineConfig({
  esbuild: { jsx: "automatic" },
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
