// Playwright config for the DEMO recorder (#3855 lane A) — the sibling of
// playwright.config.ts, and deliberately a separate file rather than a project
// inside it.
//
// The gate config is what `npx playwright test` picks up with no arguments,
// which is exactly how scripts/container/web-selftest-entry.sh invokes it. A
// second project there would have been silently added to every CI Web selftest
// run: the demo would gate merges, its ~90 seconds would be charged to every
// PR, and a recording flake would read as a product regression. A separate
// config cannot be reached by accident — the recorder passes --config, and the
// gate config additionally ignores this spec by name, so neither entry point
// can pick up the other's tests.
//
// Everything the recorder needs is handed in by
// scripts/container/web-demo-entry.sh, which boots the daemon and seeds the
// project the same way the self-test's entry script does.

import { defineConfig, devices } from "@playwright/test";
import { DEMO_VIEWPORT } from "./selftest/demo-viewport.js";

const baseURL = process.env.AF_WEB_BASE_URL;
if (!baseURL) {
  throw new Error(
    "AF_WEB_BASE_URL is unset — run the recorder through `make demo-assets` " +
      "(or scripts/container/web-demo-entry.sh), which boots the daemon and exports it.",
  );
}

export default defineConfig({
  testDir: "./selftest",
  testMatch: /web-demo\.spec\.ts$/,
  // Two passes (default theme, then dark) over one daemon, mutating shared
  // session state. Serial, one worker, like the gate.
  fullyParallel: false,
  workers: 1,
  forbidOnly: true,
  // A retry would re-run a pass whose first attempt already created
  // `tidy-tests`, so the second attempt would collide on the title rather than
  // record anything. A failed recording is a failed `make demo-assets`.
  retries: 0,
  // A whole pass — six beats, one real session create, deliberate pauses for
  // pacing — is one test.
  timeout: 300_000,
  expect: { timeout: 30_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    headless: true,
    viewport: DEMO_VIEWPORT,
    // --no-sandbox: the container runs as root and Chromium's setuid sandbox
    // refuses to start there; the whole run is fenced in a throwaway container.
    // --disable-dev-shm-usage: a container's 64MB /dev/shm crashes Chromium.
    // --hide-scrollbars: a scrollbar that appears for one beat is the kind of
    // frame-to-frame difference that makes a still look like a mistake.
    // --force-device-scale-factor=1: the recording's pixel size must be the
    // viewport's, whatever the host reports.
    launchOptions: {
      args: [
        "--no-sandbox",
        "--disable-dev-shm-usage",
        "--hide-scrollbars",
        "--force-device-scale-factor=1",
      ],
    },
  },
  outputDir: "./demo-results",
  projects: [{ name: "demo", use: { ...devices["Desktop Chrome"], viewport: DEMO_VIEWPORT } }],
});
