import { defineConfig } from "@playwright/test";
import demo from "./playwright.demo.config.js";
if (process.env.AF_PERF_MODE !== "1") throw new Error("Use scripts/testbox.sh perf");
export default defineConfig(demo, {
  testMatch: /web-perf\.spec\.ts$/,
  outputDir: "./test-results/perf",
  use: { trace: "retain-on-failure" },
});
