import { fileURLToPath } from "node:url";
import { defineConfig } from "vitest/config";

// Component tests run in jsdom; the mock gateway tests are plain TS.
export default defineConfig({
  esbuild: { jsx: "automatic" },
  resolve: { alias: { "@": fileURLToPath(new URL("./src", import.meta.url)) } },
  // Tests don't need styles. An inline config stops Vite loading
  // postcss.config.mjs when a test imports a file that imports CSS (the root
  // layout does): that file names its plugin as a string, Next's format, and
  // Vite's PostCSS loader only accepts plugin functions.
  css: { postcss: { plugins: [] } },
  test: {
    environment: "jsdom",
    include: ["src/**/*.test.{ts,tsx}"],
  },
});
