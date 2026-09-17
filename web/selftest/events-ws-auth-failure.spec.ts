// Regression guard for the #1674 WS-auth-failure escalation: a WebSocket upgrade
// the daemon rejects at the auth gate (HTTP 401, after the operator rotated the
// access token) fires onclose(1006) with NO onopen, and the browser's WebSocket
// API exposes no HTTP status to JS — so the old EventStream scheduled a reconnect
// with the same revoked credential forever and never told index.ts. The fix
// (events.ts) counts consecutive close-before-opens and past a small threshold
// fires onAuthFailure ONCE; index.ts routes that to requestResync(), whose
// fetchSessionSnapshot 401 trips the existing shouldForgetToken -> disconnect(),
// returning the SPA to the paste-token login.
//
// Modeling the rejected upgrade: routeWebSocket auto-completes the WS handshake
// (so the browser would see onopen, which does NOT trip the streak), and CDP's
// Network.setBlockedURLs does NOT reliably block ws:// upgrades in this harness.
// Instead the page installs a window.WebSocket proxy that, for /v1/events only,
// fires onerror + onclose(1006) WITHOUT onopen — exactly the 401-pre-empts-
// upgrade signature the bug rides on, controllable from JS, with all REST routes
// (auth-info, Snapshot) mocked so the daemon's real auth policy is irrelevant.
// This was validated to reproduce the bug literally: against a pre-fix build the
// data-live indicator stays "reconnecting" forever and the SPA never returns to
// login (snapshotCalls stays at 1); against the fixed build the escalation probe
// fires within ~1.5s and disconnect() returns the SPA to the paste-token form.

import { test, expect, type Page } from "@playwright/test";

const authInfoBody = (required: boolean): string =>
  JSON.stringify({ data: { auth_required: required }, error: null });

// The daemon's REAL failure envelope: error is an OBJECT ({message}), not a
// string (apiproto.EnvelopeError) — the shape shouldForgetToken and describeError
// consume.
const failureBody = (message: string): string =>
  JSON.stringify({ data: null, error: { message, daemon_rejected: true } });

const snapshotEnvelope = (): { data: Record<string, unknown>; error: null } => ({
  data: { instances: [], boot_id: "rotated-1", operation_lock_timeout_ms: 0, operation_clock_ms: 0 },
  error: null,
});

/**
 * Replaces window.WebSocket so any /v1/events upgrade fires onerror+onclose(1006)
 * WITHOUT onopen — the faithful signature of a 401 at the daemon's auth gate
 * (the browser surfaces a rejected WS handshake as onclose(1006) with no onopen
 * and no HTTP status). Other WS URLs (e.g. PTY streams) pass through to the
 * native constructor so the rest of the app's wiring is exercised for real.
 */
async function installRejectedUpgradeHook(p: Page): Promise<void> {
  await p.addInitScript(() => {
    const Native = window.WebSocket;
    window.WebSocket = new Proxy(Native, {
      construct(target, args) {
        const url = String(args[0]);
        if (url.includes("/v1/events")) {
          const fake = {
            url,
            readyState: 0,
            binaryType: "blob",
            onopen: null as null | (() => void),
            onmessage: null as null | ((e: MessageEvent) => void),
            onclose: null as null | ((e: CloseEvent) => void),
            onerror: null as null | ((e: Event) => void),
            close() { this.readyState = 3; },
            addEventListener() {},
            removeEventListener() {},
            dispatchEvent() { return false; },
          };
          // Fire onerror then onclose(1006) on the next microtask, no onopen —
          // exactly what the browser does when the daemon 401s the WS upgrade.
          setTimeout(() => {
            fake.onerror?.(new Event("error"));
            fake.onclose?.(new CloseEvent("close", { code: 1006, wasClean: false }));
          }, 0);
          return fake;
        }
        return new target(...args);
      },
    });
  });
}

test("WS upgrade rejected with 401 after token rotation returns the SPA to login via onAuthFailure (#1674)", async ({ browser }) => {
  test.setTimeout(60_000);
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  try {
    // The daemon's auth-info answer forces the paste-token login (the bug only
    // manifests on a token-gated daemon; loopback tokenless short-circuits the
    // auth gate and never 401s).
    await p.route("**/v1/auth-info", (route) =>
      route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
    );

    // Block the events WS at the JS layer so every upgrade fails with onclose(1006)
    // and NO onopen — the only visible signature of a 401 at the daemon's auth gate.
    await installRejectedUpgradeHook(p);

    // The Snapshot route: the login seed succeeds (200, empty rail) so the SPA
    // enters the authed shell and startStream() opens the (rejected) events WS;
    // every subsequent Snapshot is the escalated resync probe and returns 401,
    // which shouldForgetToken recognizes and disconnect() clears the token for.
    let snapshotCalls = 0;
    await p.route("**/v1/Snapshot", async (route) => {
      snapshotCalls += 1;
      if (snapshotCalls === 1) {
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(snapshotEnvelope()),
        });
        return;
      }
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: failureBody("unauthorized"),
      });
    });

    await p.goto("/");
    await expect(p.locator("#af-token")).toBeVisible();
    await p.locator("#af-token").fill("rotated-away");
    await p.locator(".af-login-form button[type=submit]").click();

    // The seed 200 admitted the SPA; the rejected WS now stacks three
    // close-before-opens (500ms + 1000ms of backoff before the 3rd close fires
    // onAuthFailure), requestResync()'s probe hits the 401 above, and
    // disconnect() returns the SPA to the paste-token form. 30s budget for a
    // flaky/slow box; against the fixed build this lands within ~2-4s.
    await expect(
      p.locator("#af-token"),
      "the SPA must return to the paste-token form after the WS-rejection escalation (not loop reconnecting forever)",
    ).toBeVisible({ timeout: 30_000 });
    await expect(
      p.locator(".af-error"),
      "the rejection must be explained with the canned 401 message from describeError",
    ).toContainText("That token was rejected");
    await expect(
      p.locator(".af-app"),
      "the authed shell must be torn down by disconnect() after the probe 401",
    ).toHaveCount(0);
    expect(
      snapshotCalls,
      "the escalation probe must have hit Snapshot a 2nd time and received the 401 that trips disconnect",
    ).toBeGreaterThanOrEqual(2);
  } finally {
    await p.close();
    await ctx.close();
  }
});
