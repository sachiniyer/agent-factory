// Playwright config for the web-driver-selftest harness (#1592 Phase 5 PR6).
//
// The harness is gated ENTIRELY behind `make web-selftest-container`: like
// web-build/web-test, Node + Playwright never enter the Go build or the Go test
// suite (the locked toolchain decision). It drives the daemon's embedded SPA in a
// headless Chromium against a REAL af daemon that the container entry script
// (scripts/container/web-selftest-entry.sh) brings up on a throwaway home — a
// loopback TLS listener — and exports as env:
//
//   AF_WEB_BASE_URL   https://127.0.0.1:<port>   the SPA + /v1 API origin
//   AF_WEB_SESSION_A  / AF_WEB_SESSION_B          the two seeded session titles
//
// No token is exported: the daemon binds loopback, so under #1696 the browser is a
// loopback peer the daemon exempts from the bearer token — the SPA auto-connects
// with no credential. The harness asserts exactly that tokenless flow.
//
// The listener is self-signed (the daemon's generated default), so
// ignoreHTTPSErrors accepts it — the browser cannot TOFU-pin a fingerprint the
// way the CLI does, and this is a loopback test origin, not a trust boundary.

import { defineConfig, devices } from "@playwright/test";

const baseURL = process.env.AF_WEB_BASE_URL;
if (!baseURL) {
  throw new Error(
    "AF_WEB_BASE_URL is unset — run the harness through `make web-selftest-container` " +
      "(or scripts/container/web-selftest-entry.sh), which boots the daemon and exports it.",
  );
}

export default defineConfig({
  testDir: "./selftest",
  // One daemon, one browser: the flows mutate shared session state (create / kill /
  // archive), so they must run serially against a single worker.
  fullyParallel: false,
  workers: 1,
  forbidOnly: true,
  retries: 0,
  // Generous but bounded: a real session create spins up a git worktree + tmux
  // pane, so individual assertions poll for a few seconds.
  timeout: 60_000,
  expect: { timeout: 15_000 },
  reporter: [["list"]],
  use: {
    baseURL,
    headless: true,
    ignoreHTTPSErrors: true,
    // Capture a trace only on a failing run, into the gitignored artifacts dir.
    trace: "retain-on-failure",
    // The harness container runs as root (it builds af + runs the daemon), and
    // Chromium's setuid sandbox refuses to start as root; disable it. Safe here —
    // the whole run is fenced inside a throwaway container, exactly like testbox.
    // --disable-dev-shm-usage: a container's default 64MB /dev/shm is too small for
    // Chromium and crashes it under load; route shared memory to /tmp instead.
    launchOptions: { args: ["--no-sandbox", "--disable-dev-shm-usage"] },
  },
  outputDir: "./test-results",
  projects: [{ name: "chromium", use: { ...devices["Desktop Chrome"] } }],
});
