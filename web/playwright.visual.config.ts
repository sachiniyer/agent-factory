import { defineConfig } from "@playwright/test";
import demo from "./playwright.demo.config.js";

if (process.env.AF_PERF_MODE !== "1") throw new Error("Use scripts/testbox.sh perf");
if (process.env.CI && process.env.AF_UPDATE_GOLDENS === "1") throw new Error("CI cannot update goldens");
export default defineConfig(demo, {
  snapshotPathTemplate: "{testDir}/goldens/{arg}{ext}",
  outputDir: "./test-results/visual",
  updateSnapshots: process.env.AF_UPDATE_GOLDENS === "1" ? "all" : "none",
  expect: { timeout: 30_000, toHaveScreenshot: { maxDiffPixels: 0, threshold: 0.2 } },
  use: { trace: "retain-on-failure" },
});
