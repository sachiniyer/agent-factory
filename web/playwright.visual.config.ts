import { defineConfig } from "@playwright/test";
import demo from "./playwright.demo.config.js";

if (process.env.AF_PERF_MODE !== "1") throw new Error("Use scripts/testbox.sh perf");
if (process.env.CI && process.env.AF_UPDATE_GOLDENS === "1") throw new Error("CI cannot update goldens");
export default defineConfig(demo, {
  snapshotPathTemplate: "{testDir}/goldens/{arg}{ext}",
  outputDir: "./test-results/visual",
  // Update mode is "changed", never "all". "all" rewrites a golden whenever its
  // PNG bytes differ and never consults the pixel comparator, but the capture is
  // byte-nondeterministic below the gate threshold: two captures of one master
  // tree differed in 13 of 122 goldens, every one passing the gate (#4557). So
  // "all" handed each update run a different ~11% of noise to commit. "changed"
  // rewrites only goldens the gate itself rejects, with the options it uses.
  updateSnapshots: process.env.AF_UPDATE_GOLDENS === "1" ? "changed" : "none",
  expect: { timeout: 30_000, toHaveScreenshot: { maxDiffPixels: 0, threshold: 0.2 } },
  use: { trace: "retain-on-failure" },
});
