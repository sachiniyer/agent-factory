// web-driver-selftest (#1592 Phase 5 PR6) — the acceptance proof for the embedded
// browser web client, the browser analogue of tui-driver-selftest.sh.
//
// It drives a headless Chromium against a REAL af daemon (a throwaway home on a
// loopback plain-HTTP listener, brought up by web-selftest-entry.sh) and asserts the core
// v1 loop end to end — assertions are the gate, not screenshots:
//
//   1. tokenless open  no token required ⇒ the SPA auto-connects with NO credential
//                      and NEVER shows the paste-token login (#1696); the decision
//                      follows the daemon's /v1/auth-info answer, and a forced
//                      auth_required=true brings the login back even on loopback
//   1b. real errors    a failed login renders the daemon's own message — never the
//                      literal "[object Object]" the string/object envelope fork gave
//   2. sidebar         the rail lists the sessions from the Snapshot/events plane
//   3. attach          click-to-attach opens the xterm terminal + shows live output
//   4. keyboard (#1694) j/k navigate the rail, Enter attaches, Escape returns to rail
//   5. create          the + New modal creates a session; its row appears
//   6. kill            the kill confirm removes the session's row
//   7. archive         the archive confirm moves a session to the archived group
//
// The daemon runs with the shipped default require_token=false, so NO peer needs a
// token — the SPA's /v1/auth-info probe reports auth_required=false, the login screen
// is skipped, and every core action (create/kill/archive/send-prompt/attach) runs on
// the empty-token credential. That makes this harness the end-to-end regression guard
// that tokenless authorization works for ALL actions, not just the read path.
//
// The token-PASTE UI is exercised here by intercepting /v1/auth-info to force
// auth_required=true: that both proves the SPA obeys the daemon's answer rather than
// sniffing its own loopback address (so the tokenless path works for a network peer
// too) and gives the failed-login error assertions a screen to render on. The real
// server-side enforcement stays covered by the Go handler tests
// (daemon/httpauth_test.go network-peer → 401 + spoof-resistance, and
// daemon/web_listener_policy_test.go for the config→policy→gate wiring).
//
// Everything the test needs is handed in via env by the entry script (see
// playwright.config.ts): AF_WEB_BASE_URL and the two seeded session titles
// AF_WEB_SESSION_A / AF_WEB_SESSION_B. No token is needed.

import { expect, type Locator, type Page, test } from "@playwright/test";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";
const SESSION_B = process.env.AF_WEB_SESSION_B ?? "probe-b";
// The session seeded in a SECOND repo (redesign PR2), used to prove the single-
// project rail scopes to one project and hides the others. It is created BEFORE
// A/B/web, so the most-recently-active default lands on the first repo — A/B are the
// visible rail on load, and SESSION_C is hidden until its project is selected.
const SESSION_C = process.env.AF_WEB_SESSION_C ?? "probe-c";
// The name of the task the harness seeds (web-selftest-entry.sh) so the tasks list
// is non-empty on load.
const SEEDED_TASK = process.env.AF_WEB_TASK_NAME ?? "probe-task";
// The task in the TASK-ONLY project (a third repo with a task but no session,
// redesign PR2): proves a task-only repo lists in the switcher and its tasks scope.
const TASK3_NAME = process.env.AF_WEB_TASK3_NAME ?? "mock3-task";
// The marker the seeded fake agent prints on launch (web-selftest-entry.sh), so
// "the terminal shows live output" is a deterministic string assertion.
const READY_MARKER = process.env.AF_WEB_READY_MARKER ?? "AF_SELFTEST_READY";
// The web-tab session (feat: web/iframe tabs) and its seeded targets: a LOCAL web
// tab named "preview" pointing at a loopback server the daemon proxies, and an
// EXTERNAL web tab named "external" whose host this test intercepts.
const SESSION_WEB = process.env.AF_WEB_SESSION_WEB ?? "probe-web";
// A session the harness already drove through web tab -> archive -> restore (#1809).
const SESSION_WEB_RESTORED = process.env.AF_WEB_SESSION_WEB_RESTORED ?? "probe-restored";
// A session the harness left ARCHIVED holding a preserved web tab (#1809 follow-up).
const SESSION_WEB_SHELVED = process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved";
// The malformed/older web tab (kind=web, no target url) the harness seeds directly
// into the daemon's store, because every API path refuses to mint one (#1818). It is
// part of probe-web's REAL roster for the whole run, not a per-test mock.
const NOURL_TAB = process.env.AF_WEBTAB_NOURL_NAME ?? "nourl";
const WEBTAB_LOCAL_MARKER = process.env.AF_WEBTAB_LOCAL_MARKER ?? "AF_WEBTAB_LOCAL_OK";
const VSCODE_MARKER = process.env.AF_VSCODE_MARKER ?? "AF_VSCODE_TAB_OK";
// No vscode session/tab is seeded — the test below creates its tab through the +
// menu, which needs no fixture and covers the real create flow.
const WEBTAB_EXTERNAL_URL = process.env.AF_WEBTAB_EXTERNAL_URL ?? "https://blocked.example.test/";
// The mirror-path session (#1806/#1811): one web tab targeting a SUBDIRECTORY
// document with sibling, parent-relative and absolute-path assets — the shape the
// original single-document fixture had no sub-resources to express.
const SESSION_VITE = process.env.AF_WEB_SESSION_VITE ?? "probe-vite";
const VITE_MARKER = process.env.AF_VITE_MARKER ?? "AF_VITE_OK";
const VITE_ABS_TITLE = process.env.AF_VITE_ABS_TITLE ?? "AF_ABS_ASSET_EXECUTED";
// The #1810 misroute session — tabs [agent, lower, mis, after], where "mis" is the
// vite-shaped server and "lower"/"after" are the plain one. Visited by exactly one
// test, so its close-a-tab assertions are independent of what an earlier test left
// selected. See web-selftest-entry.sh for why that tab layout is load-bearing.
const SESSION_MIS = process.env.AF_WEB_SESSION_MIS ?? "probe-misroute";

/** A rail row by its session title. */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
}

/** One state's checkbox in the rail's filter menu (feat: hide archived by default). */
function filterItem(page: Page, kind: string): Locator {
  return page.locator(`.af-filter-item[data-kind="${kind}"]`);
}

/**
 * Sets one state's checkbox in the rail filter and closes the menu.
 *
 * Idempotent (it clicks only when the box disagrees), because these flows share one
 * page in serial order and the filter is PERSISTED: a test that assumed a starting
 * state instead of asserting it would pass or fail on whatever the previous test
 * left in localStorage. Every test that changes the filter restores it before it
 * ends, for the same reason.
 */
async function setFilter(page: Page, kind: string, on: boolean): Promise<void> {
  await page.locator(".af-rail-filter").click();
  const item = filterItem(page, kind);
  await expect(item).toBeVisible();
  if ((await item.getAttribute("aria-checked")) !== String(on)) {
    await item.click();
  }
  await expect(item).toHaveAttribute("aria-checked", String(on));
  // Dismiss by clicking outside the control (the rail title is inert chrome).
  await page.locator(".af-rail-title").click();
  await expect(page.locator(".af-filter-menu")).toBeHidden();
}

/** Restores the default filter — every state but archived — via the menu's own reset,
 *  so a test leaves the shared page as it found it. */
async function resetFilter(page: Page): Promise<void> {
  await page.locator(".af-rail-filter").click();
  const reset = page.locator(".af-filter-reset");
  if (await reset.isEnabled()) {
    await reset.click();
  }
  await expect(reset).toBeDisabled();
  await page.locator(".af-rail-title").click();
  await expect(page.locator(".af-filter-menu")).toBeHidden();
}

/** A project switcher menu item by its EXACT repo basename (redesign PR2). Filters on
 *  the name span with an anchored regex so "mock-repo" never also matches
 *  "mock-repo-2" / "mock-repo-3" (they share the prefix). */
function projectItem(page: Page, name: string): Locator {
  return page
    .locator(".af-project-menu .af-project-item")
    .filter({ has: page.locator(".af-project-item-name", { hasText: new RegExp(`^${name}$`) }) });
}

/**
 * Simulates dragging the tab labelled `tabText` from the tab bar onto an `edge` of
 * the (single) current pane. Playwright's mouse-based dragTo doesn't drive HTML5
 * drag-and-drop reliably, so we dispatch the drag events ourselves with a shared
 * DataTransfer — the same object across dragstart/dragover/drop makes getData work,
 * exactly as a real drag would — and aim the drop at the edge band so the pane splits
 * in that direction (see split.ts zoneAt). A center drop uses the middle instead.
 */
async function dragTabToPane(page: Page, tabText: string, edge: "left" | "right" | "top" | "bottom" | "center"): Promise<void> {
  await page.evaluate(
    ({ tabText, edge }) => {
      const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes(tabText));
      const pane = document.querySelector(".af-term-host .af-pane");
      if (!tab || !pane) {
        throw new Error("drag source or target pane not found");
      }
      const dt = new DataTransfer();
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
      const r = pane.getBoundingClientRect();
      let x = r.left + r.width / 2;
      let y = r.top + r.height / 2;
      const m = 6;
      if (edge === "left") {
        x = r.left + m;
      } else if (edge === "right") {
        x = r.right - m;
      } else if (edge === "top") {
        y = r.top + m;
      } else if (edge === "bottom") {
        y = r.bottom - m;
      }
      const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
      pane.dispatchEvent(new DragEvent("dragenter", init));
      pane.dispatchEvent(new DragEvent("dragover", init));
      pane.dispatchEvent(new DragEvent("drop", init));
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
    },
    { tabText, edge },
  );
}

/**
 * Reduces the shown session's tab bar to its single unclosable Agent tab, whatever the
 * previous test left behind — and with it the layout, since dropping to one tab
 * validates every pane back down to one.
 *
 * The suite is serial against one daemon, so a test that fails BEFORE its own cleanup
 * hands the next test a stray tab. That next test then either quietly exercises a
 * different path than it claims, or reds out on a precondition — a second failure
 * pointing at the wrong subject, which is exactly what makes a red suite hard to read
 * (#1897). Asserting the state you need is cheaper than inheriting it.
 *
 * The agent tab (index 0) renders no ×, so closing every × converges on it. Bounded by
 * af's 9-tab-per-session ceiling.
 */
async function resetToAgentTab(page: Page): Promise<void> {
  const tabbar = page.locator(".af-tabbar");
  for (let guard = 0; guard < 9; guard++) {
    const closable = tabbar.locator(".af-tab-close");
    if ((await closable.count()) === 0) {
      break;
    }
    const before = await tabbar.locator(".af-tab").count();
    await closable.first().click();
    await expect(tabbar.locator(".af-tab")).toHaveCount(before - 1, { timeout: 30_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
}

/**
 * Dispatches a drop carrying a HAND-CRAFTED drag payload ({index, tabs}) onto the
 * (single) current pane — bypassing the real tab buttons so a stale / out-of-range /
 * mid-drag-reorder snapshot can be injected. Used to prove the drop handler validates
 * the payload (range + drag-time tab-set snapshot vs live) and no-ops an invalid one
 * instead of binding a pane to the wrong / a nonexistent tab.
 */
async function dropSnapshotOnPane(page: Page, payload: { index: number; tabs: string[] }): Promise<void> {
  await page.evaluate((payload) => {
    const pane = document.querySelector(".af-term-host .af-pane");
    if (!pane) {
      throw new Error("no pane to drop on");
    }
    const dt = new DataTransfer();
    dt.setData("application/x-af-tab", JSON.stringify(payload));
    const r = pane.getBoundingClientRect();
    const init = {
      bubbles: true,
      cancelable: true,
      dataTransfer: dt,
      clientX: r.right - 6, // aim at the right edge → a split, if it were allowed
      clientY: r.top + r.height / 2,
    };
    pane.dispatchEvent(new DragEvent("dragover", init));
    pane.dispatchEvent(new DragEvent("drop", init));
  }, payload);
}

/**
 * Drags the tab labelled `tabText` onto an `edge` of the pane at `paneIndex`, HIT-TESTING
 * the drop point the way the browser does instead of dispatching straight at the pane.
 *
 * This is what makes it a regression test for the iframe-swallow bug. dragTabToPane above
 * dispatches directly on .af-pane, which bypasses hit-testing entirely — it would pass
 * whether or not a frame covers the pane, so it can't see this bug. Here the drop point is
 * resolved with elementFromPoint (which honours pointer-events, exactly like the browser's
 * own drag hit-testing). Landing on an <iframe> means the real browser delivers the event
 * to the FRAMED document and this document sees no dragover/drop at all, so that case
 * dispatches NOTHING — reproducing the user's "nothing happens". Returns what was hit so a
 * test can assert on the swallow itself.
 *
 * The aim point is the MIDDLE of the edge band, which is where a user actually drags. A few
 * px from the rim (dragTabToPane's `m = 6`) would land in .af-pane-host's ~10px padding
 * gutter — parent DOM that never swallowed anything — and would quietly miss the bug.
 */
async function dragTabOntoPaneHitTested(
  page: Page,
  tabText: string,
  edge: "left" | "right" | "top" | "bottom" | "center",
  paneIndex = 0,
): Promise<{ hitTag: string; swallowed: boolean }> {
  return await page.evaluate(
    ({ tabText, edge, paneIndex, band }) => {
      const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes(tabText));
      const pane = [...document.querySelectorAll(".af-term-host .af-pane")][paneIndex];
      if (!tab || !pane) {
        throw new Error("drag source or target pane not found");
      }
      const dt = new DataTransfer();
      // The real delegated dragstart listener stamps the payload AND sets the body flag
      // the pointer-events rule keys off — so the hit-test below sees true drag state.
      tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));

      const r = pane.getBoundingClientRect();
      let x = r.left + r.width / 2;
      let y = r.top + r.height / 2;
      const inset = band / 2; // the middle of the outer band
      if (edge === "left") {
        x = r.left + r.width * inset;
      } else if (edge === "right") {
        x = r.right - r.width * inset;
      } else if (edge === "top") {
        y = r.top + r.height * inset;
      } else if (edge === "bottom") {
        y = r.bottom - r.height * inset;
      }

      const hitEl = document.elementFromPoint(x, y);
      const swallowed = hitEl instanceof HTMLIFrameElement;
      const hitTag = hitEl ? hitEl.tagName.toLowerCase() : "none";
      if (hitEl && !swallowed) {
        // Dispatch on whatever the pointer really lands on; the pane's handlers catch it
        // as it bubbles, just as a genuine drag would.
        const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: x, clientY: y };
        hitEl.dispatchEvent(new DragEvent("dragenter", init));
        hitEl.dispatchEvent(new DragEvent("dragover", init));
        hitEl.dispatchEvent(new DragEvent("drop", init));
      }
      tab.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: dt }));
      return { hitTag, swallowed };
    },
    { tabText, edge, paneIndex, band: 0.3 }, // EDGE_BAND in split.ts
  );
}

/** Opens the app on the loopback daemon and asserts the tokenless auto-connect
 *  (#1696): the SPA learns via /v1/auth-info that this loopback client needs no
 *  token, skips the paste-token login entirely, and renders the authed shell with
 *  NO credential. The absence of the #af-token field is the proof no login was
 *  shown; the rail being populated proves the Snapshot was fetched authorized. */
async function openTokenless(page: Page): Promise<void> {
  await page.goto("/");
  // The authed shell renders without any login interaction.
  await expect(page.locator(".af-app")).toBeVisible();
  // The paste-token login was never required — its input is absent from the DOM.
  await expect(page.locator("#af-token")).toHaveCount(0);
}

// NO file-wide serial mode, deliberately (#1898).
//
// The flows do share one daemon and one page, and they must run IN ORDER — but
// order is not what serial mode buys. `fullyParallel: false` + `workers: 1`
// (playwright.config.ts) already run every test sequentially, in declaration
// order, in one worker. With `retries: 0`, the ONLY thing a file-wide
// `mode: "serial"` added was this: when any test fails, skip every test after it.
//
// That is a gate that blinds itself. A failure is exactly when the rest of the
// suite most needs to run, and the cost is not hypothetical — a single flake in
// the status-dots test (#1898) skipped 41 tests, and #1895's fixture drift skipped
// every test behind it on the branch that found it. A regression in any of them
// would have been invisible behind an unrelated red.
//
// Ordinary test isolation replaces it. Playwright discards the worker process
// after a failure and starts a new one, so beforeAll re-runs and the next test
// gets a fresh page, a fresh login, and fresh module state — the page-level
// pollution serial mode was protecting against cannot cross a failure. What
// survives a restart is the DAEMON's state, which is why the few tests that hand
// state to each other are grouped into their own small serial describe (see
// "create → kill" below): the blast radius of a failure is that group, not the
// file.
//
// So: a new test belongs at the top level unless it consumes state another test
// produced. If it does, put the pair in their own serial describe — never reach
// for a file-wide one.

let page: Page;
// The title of the session the create flow makes, handed to the kill flow. It is
// module state, so a worker restart resets it — which is exactly why the two flows
// that share it live in one serial describe rather than at the top level.
let createdTitle = "";

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage();
  await openTokenless(page);
});

test.afterAll(async () => {
  await page.close();
});

test("tokenless loopback (#1696): the SPA auto-connects with no token, no login screen", async () => {
  // The authed shell is up (openTokenless asserted it) with NO paste-token step —
  // reload to prove it is not a one-off: a fresh load re-probes /v1/auth-info and
  // again auto-connects with no credential.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator("#af-token")).toHaveCount(0);
  // The events WS connected on the empty-token credential (the ?access_token= is
  // blank and the loopback peer is exempt): the live pip reads open.
  await expect(page.locator(".af-live-pip.af-live-open")).toBeVisible();
});

// The auth-info envelope the SPA probes, as the daemon would send it.
function authInfoBody(required: boolean): string {
  return JSON.stringify({ data: { auth_required: required }, error: null });
}

// The daemon's REAL failure envelope: `error` is an OBJECT ({message}), not a
// string (apiproto.EnvelopeError). This is the shape that used to render as the
// literal text "[object Object]" on every error surface.
function failureBody(message: string): string {
  return JSON.stringify({ data: null, error: { message } });
}

test("the tokenless path follows the daemon's answer, not loopback detection (#1696)", async ({ browser }) => {
  // This browser IS a loopback peer, so a client that decided "no token" by sniffing
  // its own address would auto-connect here. Force the daemon's answer to
  // auth_required=true and the SPA must show the paste-token login anyway — that is
  // the proof the tokenless path keys off /v1/auth-info and nothing else, so it will
  // equally trigger for a NETWORK peer whose daemon reports no token required
  // (the require_token=false default).
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );

  await p.goto("/");
  await expect(p.locator("#af-token")).toBeVisible();
  await expect(p.locator(".af-app")).toHaveCount(0);

  await ctx.close();
});

test("a failed login renders the daemon's real message, never [object Object]", async ({ browser }) => {
  // The regression this pins: the client typed the envelope's `error` as a string
  // while the daemon sends {message}, so `new ApiError(status, env.error)` stringified
  // an object and every failure read "Login failed: [object Object]" — telling the
  // operator nothing. Drive a real failed login and assert the daemon's own words
  // reach the screen.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );
  // The login probe hits Snapshot; fail it with a structured, non-401 envelope so
  // describeError takes the branch that renders the message (401 has canned text).
  await p.route("**/v1/Snapshot", (route) =>
    route.fulfill({
      status: 400,
      contentType: "application/json",
      body: failureBody("token rejected by policy: expired credential"),
    }),
  );

  await p.goto("/");
  await p.locator("#af-token").fill("some-token");
  await p.locator(".af-login-form button[type=submit]").click();

  const err = p.locator(".af-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("token rejected by policy: expired credential");
  await expect(err).not.toContainText("[object Object]");
  await expect(err).not.toContainText("undefined");

  await ctx.close();
});

test("an unreachable daemon reports a real transport message, not [object Object]", async ({ browser }) => {
  // The other failure mode Sachin hit: connection refused rather than a bad token.
  // The thrown value is a TypeError, not an envelope — errorText must still yield a
  // readable string.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/auth-info", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: authInfoBody(true) }),
  );
  await p.route("**/v1/Snapshot", (route) => route.abort("connectionrefused"));

  await p.goto("/");
  await p.locator("#af-token").fill("some-token");
  await p.locator(".af-login-form button[type=submit]").click();

  const err = p.locator(".af-error");
  await expect(err).toBeVisible();
  await expect(err).toContainText("Couldn't reach the daemon");
  await expect(err).not.toContainText("[object Object]");
  await expect(err).not.toContainText("undefined");

  await ctx.close();
});

test("sidebar lists the seeded sessions from the Snapshot/events plane", async () => {
  // Both seeded rows are present — proof the rail is driven by the daemon
  // projection, not a static list. The rail is SCOPED to the default project (the
  // first repo, redesign PR2), so A and B (that repo) show while SESSION_C (the
  // second repo) is hidden until its project is selected.
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toBeVisible();
  // The other project's session is NOT in the scoped rail.
  await expect(row(page, SESSION_C)).toHaveCount(0);
  // The top-right switcher shows the current (default) project.
  await expect(page.locator(".af-project-switch-name")).toContainText("mock-repo");
  await expect(page.locator(".af-rail-count")).toHaveText(/[2-9]|\d{2,}/);
  // The events WebSocket connected: the live pip reads "Live" (open), proving the
  // push plane the rail resyncs from is up.
  await expect(page.locator(".af-live-pip.af-live-open")).toBeVisible();
});

test("status dots (#1766): waiting shows a green dot, working shows none, error states are static — no spin anywhere", async ({
  browser,
}) => {
  // The daemon can't be coerced to Running/Lost/Dead/Limit on demand, so pin the
  // states by rewriting the Snapshot. Every asserted row is SYNTHETIC, and that is
  // the whole point rather than a convenience (#1898).
  //
  // A REAL row cannot be pinned. The Snapshot fixes its liveness exactly once, at
  // load; the events plane then keeps pushing `session.updated` deltas for it, and
  // applyEvent upserts them in place with NO resync (web/src/sessions.ts) — so the
  // daemon's truth lands straight on top of the fixture. The seeded fake agent
  // prints its marker and then `exec cat`s, so the daemon's pane-churn liveness
  // flips LiveRunning→LiveReady a beat after seeding. Whenever that beat fell inside
  // this test, probe-a's pinned "working" row acquired a real green dot and the
  // "working shows none" assertion failed — a flake that, under the old global
  // serial mode, took the whole suite behind it down with it.
  //
  // A synthetic id the daemon has never heard of receives no deltas, so the pinned
  // state is the only state there will ever be. That makes this deterministic by
  // construction rather than by out-waiting a race. It costs nothing in coverage:
  // the dot is a pure function of liveness, and these rows drive that render path
  // through exactly the same code.
  //
  // Its own context, too: routing Snapshot on the shared page meant a reload in,
  // a reload out, and a window where a leaked route could color another test.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    // Clone a REAL record (same worktree/repo_path, so the synthetic rows land in
    // this project's scoped rail) and vary only what the dot reads. Each gets a
    // distinct branch so its rendered branch line never contains another row's name
    // (the row() locator matches row text by substring).
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, liveness: number) => ({
      ...proto,
      id: `synth-${title}`,
      title,
      branch: `synth-${title}`,
      liveness,
      in_flight_op: 0,
    });
    list.push(
      synth("probe-working", 1), // Running → working → no dot
      synth("probe-waiting", 2), // Ready → waiting → green dot
      synth("probe-lost", 3),
      synth("probe-dead", 4),
      synth("probe-limit", 6),
    );
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // Working row: NO status dot at all — the dot span is omitted entirely. The row
  // must be asserted present FIRST: toHaveCount(0) on a row that never rendered
  // would pass for the wrong reason.
  await expect(row(p, "probe-working")).toBeVisible({ timeout: 15_000 });
  await expect(row(p, "probe-working").locator(".af-dot")).toHaveCount(0);
  // Waiting row: the static green ● dot, in the ready color bucket, never spinning.
  const readyDot = row(p, "probe-waiting").locator(".af-dot");
  await expect(readyDot).toHaveClass(/af-dot-ready/);
  await expect(readyDot).toHaveText("●");
  await expect(readyDot).not.toHaveClass(/af-dot-spin/);
  // Error/terminal states keep their STATIC glyphs (◌/○/◆), copied from the TUI.
  await expect(row(p, "probe-lost").locator(".af-dot")).toHaveText("◌");
  await expect(row(p, "probe-lost").locator(".af-dot")).toHaveClass(/af-dot-lost/);
  await expect(row(p, "probe-dead").locator(".af-dot")).toHaveText("○");
  await expect(row(p, "probe-limit").locator(".af-dot")).toHaveText("◆");
  // The animation class is gone from every status row, and the removed "working" dot
  // kind never renders anywhere.
  await expect(p.locator(".af-dot-spin")).toHaveCount(0);
  await expect(p.locator(".af-dot-working")).toHaveCount(0);

  await ctx.close();
});

test("click-to-attach opens the xterm terminal and shows live output", async () => {
  await row(page, SESSION_A).click();

  // The main pane switched to the terminal view and mounted a real xterm instance.
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .xterm")).toBeVisible();

  // Live output: the seeded fake agent printed its ready marker over the WS PTY
  // stream, and xterm rendered it into the pane. This is the flagship assertion —
  // a real binary PTY frame decoded by the TS codec and painted in the browser.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  // The pane header status resolved to the live stream, and the keyboard followed
  // the attach into terminal mode (#1693/#1694).
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();
});

test("the #1694 keyboard model: j/k navigate, Enter attaches, Escape returns to rail", async () => {
  // We are attached to A (terminal mode from the previous flow). Escape is the one
  // hatch back to the rail — no stray ESC byte leaks to the PTY.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  // j moves the selection off A to the next row; the terminal is NOT stolen — j/k
  // always navigate the rail in nav mode. (Pre-#1693 j/k silently fed the agent.)
  await page.keyboard.press("j");
  await expect(row(page, SESSION_A)).not.toHaveClass(/af-row-selected/);
  const movedTo = page.locator(".af-rail-list .af-row.af-row-selected");
  await expect(movedTo).toHaveCount(1);
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();

  // k moves back up to A — j/k are symmetric rail navigation.
  await page.keyboard.press("k");
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  // Enter attaches the selected row and hands the keyboard to its terminal.
  await page.keyboard.press("Enter");
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();

  // Escape detaches back to the rail, completing the round trip.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
});

test("the #1694 keyboard model: [ / ] cycle the top-level view (sessions → tasks → config)", async () => {
  // Rail mode from the previous flow. [ / ] cycle the top-level view; they fire in
  // rail mode only (a modal or focused terminal would swallow them). After Escape
  // the active element is document.body, so the document-level capture-phase keydown
  // listener (index.ts) handles the press. The Projects view is gone (redesign PR2);
  // the config view joins the cycle with the config editor.
  const active = (view: string) =>
    expect(page.locator(`.af-viewtab[data-view="${view}"]`)).toHaveClass(/af-viewtab-active/);
  await active("sessions");
  // ] advances sessions -> tasks -> config, then wraps config -> sessions.
  await page.keyboard.press("]");
  await active("tasks");
  await page.keyboard.press("]");
  await active("config");
  await page.keyboard.press("]");
  await active("sessions");
  // [ steps the other way, wrapping sessions -> config, and back to the start view so
  // the following rail-driven flows still see the sessions rail.
  await page.keyboard.press("[");
  await active("config");
  await page.keyboard.press("[");
  await active("tasks");
  await page.keyboard.press("[");
  await active("sessions");
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("config: the editor renders from the manifest and writes through the real path", async () => {
  // The config view is rendered entirely from GetConfig (the config manifest zipped
  // with live values) — the bundle carries no key list of its own. This drives the
  // real daemon against the container's throwaway AF home, so the write goes through
  // config.SetGlobalConfigValue: the same validated, file-locked, atomic path
  // `af config set` uses.
  await page.locator('.af-viewtab[data-view="config"]').click();
  const pane = page.locator(".af-config");
  await expect(pane).toBeVisible();

  // A tier-1 key the manifest always carries, with its purpose line.
  const row = pane.locator('.af-config-row[data-key="default_program"]');
  await expect(row).toBeVisible();
  await expect(row.locator(".af-config-purpose")).not.toBeEmpty();

  // The advanced tier folds until asked for, so the handful of keys that matter
  // are not buried under twenty.
  await expect(pane.locator('.af-config-row[data-key="daemon_poll_interval"]')).toHaveCount(0);
  await pane.locator(".af-config-toggle").click();
  await expect(pane.locator('.af-config-row[data-key="daemon_poll_interval"]')).toBeVisible();

  // An enumerated key renders a picker built from the manifest's own enum.
  const channel = pane.locator('.af-config-row[data-key="update_channel"] select');
  await expect(channel).toBeVisible();
  await channel.selectOption("preview");

  // The echo names what was WRITTEN, and the restart notice appears at the moment
  // of the edit naming the command to run — config.toml is read at startup, so an
  // editor that changed a value the running daemon ignores must say so.
  const echo = pane.locator('.af-config-row[data-key="update_channel"] .af-config-echo');
  await expect(echo).toHaveText(/set update_channel = preview/);
  await expect(pane.locator('.af-config-row[data-key="update_channel"] .af-config-notice')).toContainText(
    "af daemon restart",
  );

  // A hand-edited key is shown but never offered as a field whose save could only
  // be refused.
  const readOnly = pane.locator('.af-config-row[data-key="theme"] .af-config-readonly');
  await expect(readOnly).toHaveText(/hand-edited/);
  await expect(page.locator('.af-config-row[data-key="theme"] input')).toHaveCount(0);

  // Back to the sessions view for the flows that follow.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test("tabs: create a shell tab, switch to it, see its distinct output, close it (#1592 PR7)", async () => {
  // Capture the tab-mutation request bodies so we can assert they carry the stable
  // session id (#1592 PR7 fix 1 — the daemon must resolve by id, not the cross-repo
  // ambiguous title), and let one CloseTab be forced to fail (fix 3 — the error
  // surfaces as a visible toast).
  let lastCreateBody: { id?: string } | null = null;
  let lastCloseBody: { id?: string } | null = null;
  let failClose = false;
  await page.route("**/v1/CreateTab", async (route) => {
    lastCreateBody = route.request().postDataJSON();
    await route.continue();
  });
  await page.route("**/v1/CloseTab", async (route) => {
    lastCloseBody = route.request().postDataJSON();
    if (failClose) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        // The daemon's real failure shape: `error` is an object carrying `message`
        // (apiproto.EnvelopeError). This mock used to send a bare string, mirroring
        // the client's old (wrong) type — so the two agreed with each other and
        // disagreed with the daemon, which is how "[object Object]" survived.
        body: JSON.stringify({ data: null, error: { message: "simulated tab-close failure" } }),
      });
      return;
    }
    await route.continue();
  });

  // Attach to A so its tab bar renders. A fresh session has exactly one tab — the
  // unclosable "Agent" tab — and it is active.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1);
  await expect(tabbar.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");

  // Create a $SHELL tab via the + button (mirrors the TUI `t`). The tab bar grows
  // to two tabs, the new "Terminal" tab appears AND becomes active (createSessionTab
  // attaches it), and the terminal re-points its WS stream to that tab.
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  const shellTab = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(shellTab).toHaveClass(/af-tab-active/);

  // Fix 1: the CreateTab request carried the stable session id, not just the title.
  expect(lastCreateBody?.id, "CreateTab must send the stable session id").toBeTruthy();

  // Distinct output: the shell tab is a FRESH PTY, not the agent's — its pane does
  // not carry the agent's ready marker (the terminal was rebuilt for the new tab).
  await expect(page.locator(".af-term-host")).not.toContainText(READY_MARKER);
  // Wait for the shell tab's stream to be live before typing — keystrokes sent
  // before the WS opens are dropped by the terminal's send() guard.
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  // The + attached the shell tab, so keys reach it: type a marker and see it come
  // back over the new tab's stream — proof the switch re-pointed to a live PTY.
  await page.keyboard.type("echo AF_TAB_OUTPUT_OK");
  await page.keyboard.press("Enter");
  await expect(page.locator(".af-term-host")).toContainText("AF_TAB_OUTPUT_OK", { timeout: 15_000 });

  // 1-9 switch tabs in rail nav mode: Escape back to the rail, then 1 selects the
  // agent tab and 2 the shell tab — the active highlight follows the digit.
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-app.af-kb-rail")).toBeVisible();
  await page.keyboard.press("1");
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await page.keyboard.press("2");
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

  // Fix 3: a failed close surfaces a visible toast (tab ops have no modal). Force
  // the next CloseTab to error, click ×, and assert the toast — the tab stays.
  failClose = true;
  await shellTab.locator(".af-tab-close").click();
  await expect(page.locator(".af-toast.af-toast-show")).toContainText("simulated tab-close failure");
  // A failed close leaves the tab in place.
  await expect(tabbar.locator(".af-tab")).toHaveCount(2);
  // The CloseTab request was also id-scoped, at the same session as the create.
  expect(lastCloseBody?.id, "CloseTab must send the stable session id").toBeTruthy();
  expect(lastCloseBody?.id).toBe(lastCreateBody?.id);

  // Now let the close succeed via its × (mirrors `w`): the bar shrinks back to the
  // unclosable agent tab, which becomes active again AND the terminal re-points to
  // it (fix 2 — the visible tab and the streamed tab stay in sync as the list
  // shrinks; the agent pane's ready marker is back on screen).
  failClose = false;
  await shellTab.locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  await page.unroute("**/v1/CreateTab");
  await page.unroute("**/v1/CloseTab");
});

test("split panes (feat): drag a tab to a pane edge splits into two live panes; close collapses back", async () => {
  // Attach to A and give it a second tab, so there is a distinct tab to drag into a
  // split (dragging the only tab onto itself just moves it — no split).
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-term-meta")).toContainText("Live");

  // A single pane so far — today's zero-config default.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the Agent tab (index 0) onto the RIGHT edge → the pane splits into two, the
  // new right pane bound to the agent tab with its OWN live WS stream.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  // Two concurrent xterm instances now render side by side, each with per-pane chrome.
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);

  // BOTH panes show live output. The new agent pane streams the ready marker over its
  // own stream — identify it by that marker.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(shellPane).toHaveCount(1);
  // The other pane (the shell tab) is an independent live PTY — focus it and type, and
  // its echo comes back over its own stream, proving both panes are live at once.
  await shellPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  await page.keyboard.type("echo AF_SPLIT_OK");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("AF_SPLIT_OK", { timeout: 15_000 });

  // Close the agent pane via its × — the split collapses and the shell pane fills the
  // whole area (one pane again), without closing the underlying tab.
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  // The tab list is unchanged — only the pane closed, not the underlying tab.
  await expect(tabbar.locator(".af-tab")).toHaveCount(2);

  // Restore A to a single tab for the later create/kill/archive flows.
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): a FRESHLY-CREATED tab is a drag source too — drag the new tab splits (#1737 follow-up)", async () => {
  // The regression: only tabs present at first render were drag sources; a tab created
  // AFTER load (a new terminal tab) could not be dragged into a split. Create a new
  // terminal tab, then drag THAT tab (not an initial one) onto a pane edge and prove it
  // splits into two live panes — the drag wiring must cover tabs created after render.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  const shellTab = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(shellTab).toHaveClass(/af-tab-active/);

  // The freshly-created tab is a real HTML5 drag source: draggable is set on it just
  // like an initial tab. A real mouse drag needs this attribute (a synthetic dispatch
  // would fire regardless), so assert it directly — a missing draggable is exactly the
  // "the new tab isn't a drag source" framing of the bug.
  await expect(shellTab).toHaveJSProperty("draggable", true);

  // Show the AGENT tab in the single pane, so dragging the new Terminal tab produces a
  // split with two DISTINCT tabs (agent + the freshly-created shell), not a self-split.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the NEWLY-CREATED Terminal tab (index 1) onto the RIGHT edge → the pane splits,
  // the new right pane bound to the shell tab with its OWN live WS stream.
  await dragTabToPane(page, "Terminal", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);

  // BOTH panes are live: the agent pane still streams the ready marker, and the other
  // pane is the shell tab the freshly-created drag source bound. Type into the shell
  // pane and its echo returns over its own stream — proving the split from the NEW tab
  // is a real, independent PTY, not a dead pane.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(shellPane).toHaveCount(1);
  await shellPane.locator(".af-pane-host").click();
  await page.keyboard.type("echo AF_NEWTAB_DRAG_OK");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("AF_NEWTAB_DRAG_OK", { timeout: 15_000 });

  // Collapse the split and restore A to a single tab for the later flows.
  await shellPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): a bar rebuild that replaces a drag's source ends the drag cleanly — no stuck state (#1737 Greptile)", async () => {
  // If the source tab button is REPLACED mid-drag (a concurrent tab change rebuilds the
  // bar), no dragend can fire on the now-detached source — the global "dragging" state
  // would otherwise stick, leaving the pane hints + drop overlay on screen forever. The
  // bar rebuild must reconcile-clear that state.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });

  // Begin a real drag on the Terminal tab and drive a dragover so a pane shows its drop
  // overlay — the exact on-screen state a drop/dragend would normally clear. The single
  // shared DataTransfer carries the tab MIME, so split.ts recognises the drag.
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Terminal"));
    const pane = document.querySelector(".af-term-host .af-pane");
    if (!tab || !pane) {
      throw new Error("drag source or pane not found");
    }
    const dt = new DataTransfer();
    tab.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: dt }));
    const r = pane.getBoundingClientRect();
    const init = { bubbles: true, cancelable: true, dataTransfer: dt, clientX: r.right - 6, clientY: r.top + r.height / 2 };
    pane.dispatchEvent(new DragEvent("dragenter", init));
    pane.dispatchEvent(new DragEvent("dragover", init));
  });
  // The drag is visibly in progress: body flag set, a drop overlay shown.
  await expect(page.locator("body.af-dragging-tab")).toHaveCount(1);
  await expect(page.locator(".af-term-host .af-drop-overlay.af-drop-show")).toBeVisible();

  // Force a bar rebuild that REPLACES the drag's source button, with NO dragend on it —
  // add another tab (a concurrent tab change would do the same).
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(3, { timeout: 30_000 });

  // The drag ended cleanly: no stuck flag, and the overlay is no longer shown (its
  // visibility is gated on the now-cleared flag).
  await expect(page.locator("body.af-dragging-tab")).toHaveCount(0);
  await expect(page.locator(".af-term-host .af-drop-overlay.af-drop-show")).not.toBeVisible();

  // Restore A to a single tab for the later flows.
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).first().locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).first().locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): an out-of-range dropped tab is ignored — no broken pane", async () => {
  // Attach to A (a single agent tab, so tab index 1+ does not exist).
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drop an out-of-range tab index (99) on the pane's edge. The drop handler validates
  // it against the live tab count and no-ops it — no split is created, so no pane can
  // bind to a nonexistent tab and break its stream.
  await dropSnapshotOnPane(page, { index: 99, tabs: ["0:x"] });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  // The one pane still shows the live agent output — it was never disturbed.
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);
});

test("split panes (feat): a mid-drag tab-set change cancels the drop — no misbinding (#1738 repro)", async () => {
  // Attach to A and give it a second tab, so a drop index of 1 is IN RANGE (2 tabs).
  // This is the T-Rex reproduction: the index is valid, but the tab set changed since
  // the drag began, so binding by index alone would attach the new pane to the WRONG
  // live tab.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drop an IN-RANGE index (1) whose drag-time snapshot (2 entries — count matches the
  // live 2 tabs, so neither the range nor a count check would catch it) does NOT match
  // the live tab identities. Only the snapshot-vs-live comparison can reject this, and
  // it must: the layout stays a single pane, no split bound to the wrong tab.
  await dropSnapshotOnPane(page, { index: 1, tabs: ["0:stale-agent", "1:stale-shell"] });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  // A well-formed drag with the LIVE snapshot still splits (the happy path is intact) —
  // proven here to show the cancel above was the stale check, not a broken drop path.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });

  // Clean up: collapse back to one pane and restore A to a single tab.
  await page.locator(".af-term-host .af-pane", { hasText: READY_MARKER }).locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (#1901): dragging the ACTIVE tab splits and opens a DIFFERENT tab beside it", async () => {
  // The regression: the tab a pane already shows could not be dragged into a split. The
  // drop bound that tab to BOTH halves, so the one-tab-one-pane dedupe closed the
  // original and the split collapsed back — the drag read as a dead gesture. The new
  // half must open one of the session's OTHER tabs instead, so the user gets two
  // different tabs side by side, never A|A.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  const tabbar = page.locator(".af-tabbar");
  await resetToAgentTab(page);
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  // Creating a tab switches to it: the pane now shows Terminal, and Terminal is the tab
  // the bar marks active. That is the gesture's precondition — the dragged tab and the
  // target pane's tab are THE SAME.
  await expect(tabbar.locator(".af-tab.af-tab-active")).toContainText("Terminal");
  const paneTabId = async (nth: number) =>
    await page.locator(".af-term-host .af-pane").nth(nth).getAttribute("data-tab-id");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  const beforeId = await paneTabId(0);

  // Drag the ACTIVE tab onto the pane's own right edge.
  await dragTabToPane(page, "Terminal", "right");

  // It splits — the assertion that fails outright against the bug (one pane, unchanged).
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(2);

  // The two halves are bound to DIFFERENT tabs — the point of the fix. Read the stable
  // tab id each pane binds, not its label: the label is an ordinal and would still read
  // as two different panes if both were on one tab.
  const leftId = await paneTabId(0);
  const rightId = await paneTabId(1);
  expect(leftId, "the left pane binds a real tab id").toBeTruthy();
  expect(rightId, "the right pane binds a real tab id").toBeTruthy();
  expect(rightId, "the two halves must bind DIFFERENT tabs — never A|A").not.toBe(leftId);
  // The dragged tab stayed put and the new half (on the dragged edge) took the other
  // tab — here the Agent tab, the one focus was last on.
  expect(leftId, "the dragged tab keeps its original half").toBe(beforeId);

  // Both halves are LIVE, each on its own stream. The Agent tab streams the ready
  // marker; the Terminal half echoes what we type into its own PTY.
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  await expect(agentPane).toHaveCount(1, { timeout: 15_000 });
  await expect(agentPane, "the new half is the Agent tab").toHaveAttribute("data-tab-id", rightId ?? "");
  const termPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await termPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  await page.keyboard.type("echo AF_SELFSPLIT_OK");
  await page.keyboard.press("Enter");
  await expect(termPane).toContainText("AF_SELFSPLIT_OK", { timeout: 15_000 });

  // Restore A to one pane and one tab for the flows that follow.
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (#1901): dragging the only tab of a ONE-tab session is an inert no-op", async () => {
  // The documented degradation: with no other tab to put in the new half, the drag does
  // NOTHING — it does not split, and it does not duplicate the tab into two panes. A
  // duplicate would be the very A|A the dedupe exists to prevent, and it would collapse
  // on the next reconcile anyway. Nothing here may throw.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  // Assert the one-tab precondition rather than inherit it — it IS the subject here.
  await resetToAgentTab(page);
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);
  const before = await page.locator(".af-term-host .af-pane").getAttribute("data-tab-id");

  await dragTabToPane(page, "Agent", "right");

  // Still exactly one pane, still on the same tab, still live.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  await expect(page.locator(".af-term-host .af-pane")).toHaveAttribute("data-tab-id", before ?? "");
  // The pane is untouched, not merely present: its stream still carries the session.
  await page.locator(".af-term-host .af-pane-host").click();
  await expect(page.locator(".af-term-meta")).toContainText("Live");
});

test("web tab (feat): a local dev-server preview is daemon-proxied and rendered in an iframe", async () => {
  // The web-tab session was seeded (web-selftest-entry.sh) with a LOCAL web tab
  // "preview" pointing at a loopback HTTP server. Attaching shows the agent tab
  // plus the two web tabs; the tab bar renders web tabs by name.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const previewTab = tabbar.locator(".af-tab", { hasText: "preview" });
  await expect(previewTab).toHaveCount(1);

  // Switch to the local web tab: the pane mounts an IFRAME (not an xterm) whose src
  // is the SAME-ORIGIN daemon proxy path, so a remote viewer's browser hits the
  // daemon (which reaches the loopback dev server) rather than its own machine.
  await previewTab.click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);
  // The pane is an iframe, not a terminal.
  await expect(page.locator(".af-term-host .af-pane-host .xterm")).toHaveCount(0);
  // Every web tab has a reload control for dev-preview refreshes.
  await expect(page.locator(".af-webpane-reload")).toHaveCount(1);

  // The daemon actually reverse-proxied the loopback server: the framed document
  // shows the marker the server served (proof the proxy relayed real content).
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
});

test("web tab (#1806/#1811): a Vite-shaped subdirectory app previews, and its absolute-path asset 404s instead of getting the SPA shell", async () => {
  // The fixture is the shape the old single-document fixture structurally could not
  // fail on: a document under /app/ referencing a SIBLING (x.css), a PARENT-relative
  // (../shared.css) and an ABSOLUTE (/assets/app.js) asset.
  await row(page, SESSION_VITE).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "vite" }).click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // The src MIRRORS the target's path and is keyed by the tab's stable id — not an
  // ordinal (#1810), not a bare prefix that drops the target's path (#1806).
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\/[^/]+\/[^/]+\/app\/viewer\.html/);

  const framed = page.frameLocator(".af-webframe");
  // 1. The SUBDIRECTORY target document itself loads. This is what #1806 documented
  //    as a known limit; the mirror model removes it.
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });
  // 2. A SIBLING asset resolves beside the document (/app/x.css upstream).
  await expect(framed.locator("#sib")).toHaveCSS("color", "rgb(1, 2, 3)", { timeout: 15_000 });
  // 3. A PARENT-relative asset resolves INSIDE the prefix (/shared.css upstream).
  //    This is the one only a depth-mirroring URL can serve: with a flat prefix,
  //    "../" climbs out of the tab entirely.
  await expect(framed.locator("#par")).toHaveCSS("color", "rgb(4, 5, 6)", { timeout: 15_000 });

  // 4. The ABSOLUTE-path asset escaped the prefix and hit the daemon ROOT. The page
  //    above really did request it (the framed document carries the <script>), but an
  //    opaque-origin subframe's escaped request is not reported to page.on("response")
  //    — so ask the daemon for that exact URL directly instead, which is the contract
  //    that matters: it must fail HONESTLY rather than silently, with a 404 and never
  //    "200 text/html" handing af's own SPA shell to a <script>.
  const abs = await page.request.get("/assets/app.js");
  expect(abs.status(), "/assets/app.js must 404, not be answered with the SPA shell").toBe(404);
  expect(abs.headers()["content-type"] ?? "", "/assets/app.js must not be answered as HTML").not.toContain(
    "text/html",
  );
  expect(await abs.text(), "the af SPA's own HTML was served as the app's JavaScript").not.toContain(
    'id="app"',
  );

  // 5. And nothing executed: the framed document's title is untouched, so the shell
  //    was never run as the app's script.
  const viteFrame = page.frames().find((f) => f.url().includes("/app/viewer.html"));
  expect(viteFrame, "the previewed frame should be attached").toBeTruthy();
  expect(await viteFrame!.title()).not.toContain(VITE_ABS_TITLE);
});

test("web tab (#1810): closing a LOWER tab leaves an open preview on its OWN dev server", async () => {
  // The #1810 misroute, end to end. This session's tabs are [agent, lower, mis,
  // after]; a pane opens on "mis" (index 2, the vite-shaped server). Closing "lower"
  // shifts mis 2->1 and after 3->2, so an ORDINAL-keyed iframe would start serving
  // "after" — the OTHER dev server, at HTTP 200, with no error and no reload. The
  // stable-id route makes that impossible.
  await row(page, SESSION_MIS).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar.locator(".af-tab", { hasText: "lower" })).toHaveCount(1, { timeout: 15_000 });

  await tabbar.locator(".af-tab", { hasText: "mis" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  const framed = page.frameLocator(".af-webframe");
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });

  // Stamp the live iframe: the expando survives only if this exact element stays
  // mounted. Id-keying means a mere ordinal shift no longer forces a remount, so the
  // dev server's in-page state is kept too.
  await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    if (f) {
      f.__afStamp = "af-not-remounted";
    }
  });

  // The developer closes an UNRELATED, LOWER tab.
  await tabbar.locator(".af-tab", { hasText: "lower" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab", { hasText: "lower" })).toHaveCount(0, { timeout: 30_000 });
  // The shift really happened — without this the assertion below proves nothing.
  await expect(tabbar.locator(".af-tab")).toHaveCount(3, { timeout: 30_000 });

  // THE ASSERTION: the pane still shows mis's OWN dev server. Under the ordinal route
  // this read WEBTAB_LOCAL_MARKER — a different app, silently.
  await expect(framed.locator("#marker")).toHaveText(VITE_MARKER, { timeout: 15_000 });
  await expect(framed.locator("#marker")).not.toHaveText(WEBTAB_LOCAL_MARKER);
  // Still the same frame element: followed, not torn down.
  const stamp = await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    return f?.__afStamp ?? null;
  });
  expect(stamp).toBe("af-not-remounted");
});

test("web tab (#1809): a web tab survives archive -> restore and still renders through the proxy", async () => {
  // The harness already drove the issue's CLI repro end to end against the real
  // daemon: this session was given a web tab ("webpreview" -> the loopback server)
  // plus a process tab ("watcher"), then archived and restored. Archive used to
  // truncate the tab roster to the agent tab, erasing the URL with no way to get it
  // back. Assert the restored session is whole and its web tab is LIVE, not just a
  // surviving row.
  await row(page, SESSION_WEB_RESTORED).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();

  const restoredTab = tabbar.locator(".af-tab", { hasText: "webpreview" });
  await expect(restoredTab).toHaveCount(1);
  // The process tab does NOT come back: its tmux was torn down at archive time
  // (#1028). The fix is kind-aware, not a blanket "keep every tab".
  await expect(tabbar.locator(".af-tab", { hasText: "watcher" })).toHaveCount(0);

  // The restored web tab renders through the SAME-ORIGIN daemon proxy...
  await restoredTab.click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);

  // ...and the proxy relays real content from the still-live target, so the URL
  // survived the round trip intact rather than coming back as an empty shell.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });
});

test("web tab (#1809 follow-up): an ARCHIVED session's preserved web tab is inert — placeholder, no proxy, no ×", async () => {
  // The harness left this session archived holding a preserved web tab (and already
  // proved the CLI refuses to delete it). Preserving the URL must not make an
  // archived session live again: the target is a loopback address whose dev server
  // is gone and whose port may now host something else.
  //
  // Archived rows are hidden by default (feat: hide archived by default), so reveal
  // them to reach this one — and put the filter back before leaving.
  await setFilter(page, "archived", true);
  await row(page, SESSION_WEB_SHELVED).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  const shelvedTab = tabbar.locator(".af-tab", { hasText: "shelvedweb" });
  await expect(shelvedTab).toHaveCount(1, { timeout: 15_000 });

  // The × is withdrawn: clicking it would strip the very URL the archive preserved,
  // and the daemon would refuse anyway — so the affordance must not be offered.
  await expect(shelvedTab.locator(".af-tab-close")).toHaveCount(0);
  // ...and so is the + (an archived session can't gain tabs either).
  await expect(tabbar.locator(".af-tab-new")).toHaveCount(0);

  await shelvedTab.click();

  // The pane shows the archived placeholder instead of a frame...
  const archived = page.locator(".af-webpane-fallback.af-webpane-archived");
  await expect(archived).toBeVisible({ timeout: 15_000 });
  await expect(archived).toContainText("archived");
  // ...the iframe is hidden and never had a src assigned at all, so no proxy request
  // is issued — this is the assertion that would fail if the pane merely overlaid a
  // loaded frame with the placeholder.
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toBeHidden();
  expect(await frame.getAttribute("src")).toBeNull();
  // ...and the live target's content is nowhere on the page.
  await expect(page.locator(".af-term-host")).not.toContainText(WEBTAB_LOCAL_MARKER);
  // The "open ↗" escape hatch is withdrawn too: it would only hit the refusing proxy.
  await expect(page.locator(".af-webpane-open:visible")).toHaveCount(0);

  await resetFilter(page);
});

test("web tab (feat): an external URL is iframed directly and shows a fallback when embedding is blocked", async () => {
  // Speed up the fallback timeout for a deterministic assertion, and make the
  // external host HANG (intercept the request and never resolve it) so no load
  // event ever arrives — the reliable "didn't load in time" signal the load-timeout
  // detects. af never tries to defeat framing protections, so a fast-but-blocked
  // (X-Frame-Options) load isn't auto-detected; the always-present "open ↗" link is
  // the guaranteed escape hatch, asserted below.
  await page.evaluate(() => {
    (window as unknown as { __afWebtabFallbackMs: number }).__afWebtabFallbackMs = 400;
  });
  await page.route("**/blocked.example.test/**", () => {
    // Intentionally never fulfill/abort: the iframe request hangs, so no load event
    // fires and the fallback timeout wins.
  });

  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const externalTab = tabbar.locator(".af-tab", { hasText: "external" });
  await expect(externalTab).toHaveCount(1);
  await externalTab.click();

  // The external tab iframes the URL DIRECTLY (not through the daemon proxy).
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", WEBTAB_EXTERNAL_URL);
  // The always-present escape hatch: an "open in a new tab" link at the external URL.
  await expect(page.locator("a.af-webpane-open")).toHaveAttribute("href", WEBTAB_EXTERNAL_URL);

  // A site that doesn't load in time surfaces the clean fallback with its own
  // open-in-new-tab link.
  const fallback = page.locator(".af-webpane-fallback");
  await expect(fallback).toBeVisible({ timeout: 10_000 });
  await expect(fallback.locator("a.af-webpane-fallback-link")).toHaveAttribute("href", WEBTAB_EXTERNAL_URL);

  await page.unroute("**/blocked.example.test/**");
});

test("web tab (feat): a tab with no target URL renders a clean fallback, not a blank pane", async () => {
  // The URL-less web tab is a malformed/older record that no API can mint — three
  // guards refuse an empty target — so the harness writes it the way an older version
  // would have, straight into the daemon's own store before it boots
  // (web-selftest-entry.sh). It is therefore REAL here: the daemon serves it on the
  // Snapshot AND on every session.updated, so nothing can contradict it mid-test.
  //
  // This used to rewrite the Snapshot in the browser to inject the tab. That mock
  // patched only ONE plane, so any session.updated — which a tab change now emits
  // (#1812) — replaced the projection with the real roster and deleted the injected
  // tab out from under the assertion below: an intermittent, entirely fictional
  // failure (#1818). Seeding it for real is what removes the race; there is no reload,
  // no route, and no retry left to prop it up.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const nourlTab = tabbar.locator(".af-tab", { hasText: NOURL_TAB });
  await expect(nourlTab).toHaveCount(1, { timeout: 15_000 });
  await nourlTab.click();

  // A clean fallback (no broken iframe), not a blank pane.
  const fallback = page.locator(".af-term-host .af-pane-host .af-webpane-fallback");
  await expect(fallback).toBeVisible({ timeout: 10_000 });
  await expect(fallback).toContainText("no URL");
  // The "open ↗" link is WITHDRAWN — there is nothing to open. This asserts the
  // `hidden` actually takes effect: .af-webpane-open carries an author `display`
  // rule, which outranks the UA [hidden] rule, so hiding it needs an explicit
  // [hidden] override in styles.css. Without that this link renders and goes nowhere
  // (the state #1827's voice polish silently left it in until #1809 caught it).
  await expect(page.locator(".af-webpane-open:visible")).toHaveCount(0);
});

test("split panes (fix): a WEB/iframe pane doesn't swallow a tab drag — dropping on its edge splits", async () => {
  // The bug: dragover/drop over an <iframe> go to the FRAMED document, so a pane showing a
  // web tab ate the drag — no drop zone, no split, "nothing happens". The fix makes pane
  // iframes pointer-events:none for the duration of a drag, so the drag reaches the pane.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "preview" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Drag the Agent tab onto the RIGHT edge of the iframe pane. The drop point is over the
  // frame, exactly where a user aims.
  const res = await dragTabOntoPaneHitTested(page, "Agent", "right");
  // The drag reached the parent document rather than being taken by the frame — the bug
  // itself, asserted directly. Before the fix this is `true` and the split below never
  // happens.
  expect(res.swallowed).toBe(false);

  // The split really happened: two panes, the web tab's iframe alongside a live xterm.
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .af-pane.af-pane-multi")).toHaveCount(2);
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);
  // The framed content survived the split — still the proxied dev server, not a blank pane.
  await expect(page.frameLocator(".af-webframe").locator("#marker")).toHaveText(WEBTAB_LOCAL_MARKER, {
    timeout: 15_000,
  });

  // Collapse back to a single pane for the flows that follow.
  const termPane = page.locator(".af-term-host .af-pane", { hasNot: page.locator("iframe.af-webframe") });
  await termPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (fix): the WEB tab itself drags onto a terminal pane edge and splits", async () => {
  // The mirror of the case above: the web tab as the drag SOURCE, dropped on a terminal
  // pane. The target isn't framed, so this proves the fix didn't break the ordinary path.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  await dragTabOntoPaneHitTested(page, "preview", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  // One pane is the web tab's iframe, the other the agent's terminal.
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  const webPane = page.locator(".af-term-host .af-pane", { has: page.locator("iframe.af-webframe") });
  await webPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (#1817 follow-up): Alt+j onto a WEB pane returns the keyboard to the rail, not the pane you left", async () => {
  // The cyclePane half of the SplitView.focus() boolean contract. nav.ts resolves Alt+j/k
  // in EITHER mode, ahead of the terminal branch, so a user ATTACHED to a terminal pane can
  // cycle onto an iframe pane that has no xterm to receive the keyboard. When cyclePane
  // ignored the boolean, focus() silently no-opped and the pane the user cycled AWAY from
  // kept DOM focus: the header highlighted the web pane while keystrokes still went to the
  // agent — a silent WRONG target, worse than the silent no-target on the create path.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  await dragTabOntoPaneHitTested(page, "preview", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });
  await expect(page.locator(".af-term-host .xterm")).toHaveCount(1);

  // The precondition: really attached, with a real xterm holding the keyboard. (Clicking
  // the terminal directly — rail+Enter would attach the FOCUSED pane, which the drop just
  // made the web one.)
  await page.locator(".af-term-host .xterm").click();
  await expect(page.locator(".af-app.af-kb-terminal")).toBeVisible();

  // Alt+j cycles pane focus onto the web pane. There is no terminal to hand the keyboard
  // to, so it must come back to the rail rather than stay with the agent behind us.
  await page.keyboard.press("Alt+j");
  await expect(
    page.locator(".af-app.af-kb-rail"),
    "cycling onto a pane with no terminal must release the keyboard to the rail",
  ).toBeVisible();

  // Proof the keyboard really is the rail's: a bare j navigates instead of reaching the
  // agent we cycled away from.
  await expect(row(page, SESSION_WEB)).toHaveClass(/af-row-selected/);
  await page.keyboard.press("j");
  await expect(
    row(page, SESSION_WEB),
    "j must navigate the rail, not land in the terminal we cycled away from",
  ).not.toHaveClass(/af-row-selected/);
  await page.keyboard.press("k");
  await expect(row(page, SESSION_WEB)).toHaveClass(/af-row-selected/);

  // Leave one pane behind, exactly as the tests around this one do.
  const webPane = page.locator(".af-term-host .af-pane", { has: page.locator("iframe.af-webframe") });
  await webPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
});

test("split panes (fix): a pane iframe is inert ONLY while dragging — normal interaction is untouched", async () => {
  // The fix must not cost the web tab its interactivity: the frame is inert for the drag
  // and immediately usable again afterwards. Both states are asserted on the live frame.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await page.locator(".af-tabbar .af-tab", { hasText: "preview" }).click();
  await expect(page.locator(".af-term-host iframe.af-webframe")).toHaveCount(1, { timeout: 15_000 });

  const framePointerEvents = () =>
    page.evaluate(() => {
      const f = document.querySelector(".af-term-host iframe.af-webframe");
      return f ? getComputedStyle(f).pointerEvents : "missing";
    });

  // At rest the frame takes the pointer — clicks and scrolls inside the preview work.
  expect(await framePointerEvents()).toBe("auto");

  // Mid-drag it is inert, which is what lets the drag through to the pane.
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Agent"));
    tab?.dispatchEvent(new DragEvent("dragstart", { bubbles: true, cancelable: true, dataTransfer: new DataTransfer() }));
  });
  expect(await framePointerEvents()).toBe("none");

  // dragend restores it. (A drag whose source is replaced mid-drag is covered by the bar
  // rebuild clearing the same flag — see the #1737 stuck-state test above — so the frame
  // can't be left dead either way.)
  await page.evaluate(() => {
    const tab = [...document.querySelectorAll(".af-tabbar .af-tab")].find((t) => t.textContent?.includes("Agent"));
    tab?.dispatchEvent(new DragEvent("dragend", { bubbles: true, dataTransfer: new DataTransfer() }));
  });
  expect(await framePointerEvents()).toBe("auto");
});

test("web tab (feat): a surviving web tab that only SHIFTS ordinal is followed, not remounted (#1779)", async () => {
  // Ordering note: this consumes probe-web's "preview" tab, so it must stay AFTER the
  // web-tab tests above that rely on it.
  //
  // A pane shows the EXTERNAL web tab (index 2). A lower tab is closed, so the tab it
  // shows shifts to index 1. Nothing about that tab changed — and an external tab's
  // iframe src is the target URL, which encodes no ordinal — so the frame must be
  // FOLLOWED in place. Remounting would reload it and drop its in-page state.
  //
  // (A PROXIED tab used to be the opposite case and had to remount, because its src
  // was /v1/webtab/{session}/{ORDINAL}/. Since #1810 keyed that route by the stable
  // tab id, no web pane's address embeds an ordinal and both kinds are followed —
  // see the #1810 test above, which asserts it on a proxied pane.)
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar.locator(".af-tab", { hasText: "preview" })).toHaveCount(1, { timeout: 15_000 });

  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", WEBTAB_EXTERNAL_URL);

  // Stamp the live iframe. An expando rides the DOM node itself, so it survives if and
  // only if this exact element is still mounted — a remount builds a fresh one.
  await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    if (f) {
      f.__afStamp = "af-not-remounted";
    }
  });

  // Close the LOWER "preview" tab: the shown external tab shifts from index 2 to 1.
  await tabbar.locator(".af-tab", { hasText: "preview" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab", { hasText: "preview" })).toHaveCount(0, { timeout: 30_000 });
  // The shift really happened — without this the assertion below would prove nothing.
  // Asserted as "external now sits at ordinal 1", not as a roster total (#1863): probe-web
  // permanently carries the seeded URL-less tab (#1818), and any tab mutation republishes
  // the roster (#1812), so a total is both bigger than it looks and free to move under the
  // assertion. The ordinal is the thing this test actually turns on — [Agent, preview,
  // external, nourl] before the close, [Agent, external, nourl] after, so "external"
  // shifts 2 → 1. data-tab-index is the roster index the bar rendered it at.
  await expect(tabbar.locator('.af-tab[data-tab-index="1"] .af-tab-label')).toHaveText("external", {
    timeout: 30_000,
  });

  // The pane still shows the SAME tab, through the SAME iframe element.
  await expect(frame).toHaveCount(1);
  await expect(frame).toHaveAttribute("src", WEBTAB_EXTERNAL_URL);
  const stamp = await page.evaluate(() => {
    const f = document.querySelector(".af-term-host .af-pane-host iframe.af-webframe") as
      | (HTMLIFrameElement & { __afStamp?: string })
      | null;
    return f?.__afStamp ?? null;
  });
  expect(stamp).toBe("af-not-remounted");
});

// Ordering note: the #1779 test above consumed probe-web's "preview" tab, so the
// session's tabs here are [Agent, external] and "external" is the non-agent tab these
// two use as the retained pane.

test("tabs (#1855): re-selecting the SAME session leaves the bar on the tab the pane shows, not Agent", async () => {
  // Selecting a session asserted `activeTab: 0` about a layout it had already retained:
  // the tree still pointed at tab N, so the bar highlighted Agent while the pane kept
  // showing N. Re-selecting the session you are ALREADY on is the shortest proof —
  // setSession's same-session branch finds nothing changed, so report() (the only
  // writer of activeTab) never fires and the reset is never undone.
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");

  // Park the pane on "external" (index 1) via a REAL index change (0 then 1): clicking
  // the tab you are already on is itself a no-op report, so it could not establish this
  // precondition on a desynced store.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });

  // Click the row that is already selected. The pane is untouched (same session, same
  // tree) — so the bar must still name the tab it is showing.
  await row(page, SESSION_WEB).click();
  await expect(frame).toHaveCount(1);
  await expect(frame).toHaveAttribute("src", WEBTAB_EXTERNAL_URL);
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
});

test("tabs (#1855): switching away and back keeps activeTab on the visible pane — and the next close doesn't yank it to Agent", async () => {
  // The issue's own repro. It needs the re-entry to settle on the SAME focused index the
  // last report carried: report() dedups on (focusedTab, shownTabs, paneCount), so an
  // identical index makes it early-return and the `activeTab: 0` reset stands
  // uncorrected. probe-web is re-entered on tab 1 ("external"), so probe-b is parked on
  // ITS tab 1 to collide.
  const tabbar = page.locator(".af-tabbar");
  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");

  // A throwaway terminal tab on probe-web, so there is a tab to close later that is
  // NEITHER the agent tab nor the active one — closing it must not move the pane.
  //
  // Waited on BY NAME, never by a roster total (#1863). probe-web permanently carries the
  // seeded URL-less tab (#1818), so its roster here is [Agent, external, nourl] and the
  // old "3 tabs" wait was ALREADY satisfied before the + click even landed — it passed
  // instantly against the pre-create roster and let the test race ahead of the
  // precondition it was there to establish. The tab's own name can't be true early, and
  // it stays true across the session.updated rebuild the create publishes (#1812).
  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await tabbar.locator(".af-tab-new").click();
  const throwaway = tabbar.locator(".af-tab", { hasText: "Terminal" });
  await expect(throwaway).toHaveCount(1, { timeout: 30_000 });
  // The create attaches the new tab, so the pane is on it — which is what makes the
  // "external" click below the real index change this test needs going in.
  await expect(throwaway).toHaveClass(/af-tab-active/, { timeout: 30_000 });

  // Park probe-web on "external" (index 1) — an index change, so the store is truthful
  // going in.
  await tabbar.locator(".af-tab", { hasText: "external" }).click();
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });

  // Switch away, and leave probe-b focused on its own tab 1 (+ creates AND attaches),
  // so the last reported focused tab is 1 — the collision.
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

  // Back to probe-web: the retained pane still shows "external", so the bar must too.
  await row(page, SESSION_WEB).click();
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  await expect(frame).toHaveAttribute("src", WEBTAB_EXTERNAL_URL);
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");

  // Closing an UNRELATED tab (the throwaway, to the RIGHT of the active one) leaves the
  // pane where it was. With a stale activeTab the close arithmetic read 0 and re-pointed
  // the pane at Agent, tearing down the live iframe.
  // (The bar always renders the SELECTED session, and probe-web is selected here, so
  // `throwaway` resolves against probe-web's Terminal — not the one probe-b grew above.)
  await throwaway.locator(".af-tab-close").click();
  // Again by name (#1863): the disappearance of THIS tab is the event being waited on,
  // and the seeded nourl tab means the surviving total is 3, not 2.
  await expect(throwaway).toHaveCount(0, { timeout: 30_000 });
  await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("external");
  await expect(frame).toHaveCount(1);

  // Restore probe-b's tab set for the flows that follow.
  await row(page, SESSION_B).click();
  await expect(tabbar.locator(".af-tab", { hasText: "Terminal" })).toHaveCount(1);
  await tabbar.locator(".af-tab", { hasText: "Terminal" }).locator(".af-tab-close").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
});

test("split panes (feat): logout clears retained trees — a fresh login shows the single-leaf default", async () => {
  // Split A into two panes.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  await tabbar.locator(".af-tab-new").click();
  await expect(tabbar.locator(".af-tab")).toHaveCount(2, { timeout: 30_000 });
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });

  // Log out, then reconnect (tokenless loopback: the no-auth login offers a single
  // Connect button, no token to paste).
  await page.locator(".af-appbar button", { hasText: "Disconnect" }).click();
  await expect(page.locator(".af-login")).toBeVisible();
  await page.locator(".af-login button.af-primary").click();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-live-pip.af-live-open")).toBeVisible();

  // Re-select A: the retained split was cleared on logout, so it opens as a SINGLE
  // pane (the zero-config default), not the previous two-pane split.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1);

  // Restore A to a single tab (the shell tab survived on the daemon; only the pane
  // was gone) for the later create/kill/archive flows.
  const bar = page.locator(".af-tabbar");
  const shellTab = bar.locator(".af-tab", { hasText: "Terminal" });
  if ((await shellTab.count()) > 0) {
    await shellTab.locator(".af-tab-close").click();
    await expect(bar.locator(".af-tab")).toHaveCount(1, { timeout: 30_000 });
  }
});

test("project switcher (redesign PR2): lists projects with counts; selecting one scopes + swaps the rail; persists", async () => {
  // The top-right switcher shows the current (default) project — the first repo.
  const switcher = page.locator(".af-project-switch");
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");

  // Open the menu: it lists every project — the two session repos AND the task-only
  // repo (the cross-project glance that replaces the old all-projects rail), each with
  // its per-project count meta.
  await switcher.click();
  const menu = page.locator(".af-project-menu");
  await expect(menu).toBeVisible();
  const items = menu.locator(".af-project-item");
  await expect(items).toHaveCount(3);
  const currentItem = menu.locator(".af-project-item-current");
  await expect(currentItem.locator(".af-project-item-name")).toHaveText("mock-repo");
  // The second project (a session repo) shows its session count; the third (task-only,
  // no session) shows its task count — proof it's derived from sessions OR tasks.
  await expect(projectItem(page, "mock-repo-2").locator(".af-project-item-meta")).toContainText("1 session");
  await expect(projectItem(page, "mock-repo-3").locator(".af-project-item-meta")).toContainText("1 task");

  // Select the second project: the rail SWAPS to it — only SESSION_C shows, and the
  // first repo's A/B are gone (proof the rail is scoped to ONE project, the behavior
  // this PR replaces).
  await projectItem(page, "mock-repo-2").click();
  await expect(menu).toBeHidden();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await expect(row(page, SESSION_C)).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveCount(0);
  await expect(row(page, SESSION_B)).toHaveCount(0);

  // The choice persists across a reload (localStorage): the second project is still
  // selected and its rail still shows only SESSION_C.
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await expect(row(page, SESSION_C)).toBeVisible();
  await expect(row(page, SESSION_A)).toHaveCount(0);

  // Switch back to the first project so the following rail-driven flows (tabs, create,
  // kill, archive) see A/B again. Persisted as the first repo for the rest of the run.
  await switcher.click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toBeVisible();
});

test("task-only project (redesign PR2, Fix 1): a repo with a task but no session lists, scopes Tasks, and its delete is disabled", async () => {
  // Select the task-only project (a third repo with a task but NO session). It lists in
  // the switcher (derived from sessions OR tasks), so its tasks stay reachable.
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-3").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");

  // Its rail is the clean empty state (no sessions), not a blank rail. It has no
  // archived sessions either, so the empty state stays a bare one-liner — no
  // "N archived hidden" hint to explain something that isn't there.
  const empty = page.locator(".af-rail-empty-project");
  await expect(empty).toContainText("No active sessions in");
  await expect(empty).toContainText("mock-repo-3");
  await expect(empty.locator(".af-rail-empty-new")).toBeVisible();
  await expect(empty).not.toContainText("archived hidden");

  // The delete-project action is DISABLED here — there are no live sessions to archive,
  // so it can never be a silent no-op (Greptile Fix 2). An archived-only repo, by the
  // same rule, is never even a project. Toggle the menu open, assert, then closed.
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu .af-project-delete")).toBeDisabled();
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu")).toBeHidden();

  // The Tasks view is scoped to this project: its task shows, and the first repo's
  // seeded task does NOT (proof the Tasks view operates within the selected project).
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();
  await expect(tasks.locator(".af-task-row", { hasText: TASK3_NAME })).toHaveCount(1);
  await expect(tasks.locator(".af-task-row", { hasText: SEEDED_TASK })).toHaveCount(0);

  // Return to the first project + the sessions view for the following rail-driven flows.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
});

test("task-only project (redesign PR2, follow-on): add-task targets ITS repo, and a reload restores it as itself", async () => {
  // Capture the AddTask request so we can prove it targeted the task-only project's
  // repo — not a session-derived one, and not blocked by the absence of a session.
  let addBody: { task?: { project_path?: string } } | null = null;
  await page.route("**/v1/AddTask", async (route) => {
    addBody = route.request().postDataJSON();
    await route.continue();
  });

  // Select the task-only project.
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-3").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");

  // Fix 1: add a task from the Tasks view. The project picker DEFAULTS to this
  // (task-only) project, so submitting creates the task on mock-repo-3's repo, and the
  // form is not blocked despite the project having no session.
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const added = `mock3-added-${Date.now().toString(36)}`;
  await page.locator(".af-tasks-add").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Task name"]').fill(added);
  await modal.locator('input[aria-label="Cron expression"]').fill("*/5 * * * *");
  await modal.locator('textarea[aria-label="Prompt"]').fill("echo mock3-added");
  await modal.locator("button.af-primary").click();

  await expect(page.locator(".af-tasks .af-task-row", { hasText: added })).toBeVisible({ timeout: 30_000 });
  await expect(modal).toBeHidden();
  // The task was created on the SELECTED task-only project's repo (mock-repo-3), not
  // another project's (mock-repo / mock-repo-2).
  expect(addBody?.task?.project_path, "AddTask must target the selected task-only project's repo").toContain(
    "mock-repo-3",
  );
  expect(addBody?.task?.project_path).not.toContain("mock-repo-2");
  await page.unroute("**/v1/AddTask");

  // Fix 2: reload. The persisted task-only selection restores AS ITSELF (a real
  // task-derived project), not a temporary session-backed fallback — and the Tasks
  // view scopes to it (its tasks show, another project's seeded task does not).
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-3");
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await expect(page.locator(".af-tasks .af-task-row", { hasText: TASK3_NAME })).toHaveCount(1);
  await expect(page.locator(".af-tasks .af-task-row", { hasText: SEEDED_TASK })).toHaveCount(0);

  // Return to the first project + the sessions view for the following rail-driven flows.
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");
  await expect(row(page, SESSION_A)).toBeVisible();
});

test("tasks view (#1592 PR8): list the seeded task; add / trigger / remove round-trips", async () => {
  // Capture the task-mutation request bodies so we can prove every mutation carries
  // the STABLE task id — the add mints it client-side, and trigger/remove must send
  // that SAME id, never the (non-unique) name (the #1678 id-scoping class, PR8).
  let addedTaskId: string | undefined;
  let triggerId: string | undefined;
  let removeId: string | undefined;
  await page.route("**/v1/AddTask", async (route) => {
    addedTaskId = route.request().postDataJSON()?.task?.id;
    await route.continue();
  });
  await page.route("**/v1/TriggerTask", async (route) => {
    triggerId = route.request().postDataJSON()?.id;
    await route.continue();
  });
  await page.route("**/v1/RemoveTask", async (route) => {
    removeId = route.request().postDataJSON()?.id;
    await route.continue();
  });

  // Switch to the tasks view. The seeded cron task is listed on load — proof the
  // pane is driven by the daemon's ListTasks, not a static list.
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  await expect(page.locator('.af-viewtab[data-view="tasks"]')).toHaveClass(/af-viewtab-active/);
  const tasks = page.locator(".af-tasks");
  await expect(tasks).toBeVisible();
  const seeded = tasks.locator(".af-task-row", { hasText: SEEDED_TASK });
  await expect(seeded).toHaveCount(1);
  await expect(seeded.locator(".af-task-trigger")).toContainText("0 9 * * *");

  // Add a cron task via the + Add modal. The project picker defaults to the scoped
  // project (redesign PR2), so the task lands in it; a cron task requires a prompt
  // (the daemon rejects an empty one).
  const added = `probe-task-${Date.now().toString(36)}`;
  await tasks.locator(".af-tasks-add").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Task name"]').fill(added);
  await modal.locator('input[aria-label="Cron expression"]').fill("*/5 * * * *");
  await modal.locator('textarea[aria-label="Prompt"]').fill("echo scheduled-web");
  await modal.locator("button.af-primary").click();

  // The new task's row appears (AddTask succeeded; the list refetched).
  const addedRow = tasks.locator(".af-task-row", { hasText: added });
  await expect(addedRow).toBeVisible({ timeout: 30_000 });
  await expect(modal).toBeHidden();
  expect(addedTaskId, "AddTask must mint + send a stable task id").toBeTruthy();

  // Enable/disable round-trips via UpdateTask: the new task is enabled (Disable
  // shown). Disabling flips it, and re-enabling flips it back — proof the toggle
  // rides UpdateTask keyed by the task's id.
  await addedRow.locator("button", { hasText: "Disable" }).click();
  await expect(addedRow.locator("button", { hasText: "Enable" })).toBeVisible({ timeout: 30_000 });
  await addedRow.locator("button", { hasText: "Enable" }).click();
  await expect(addedRow.locator("button", { hasText: "Disable" })).toBeVisible({ timeout: 30_000 });

  // Trigger-now round-trips via TriggerTask (enabled cron tasks only). Await the RPC
  // response and assert the envelope carries no error — the daemon fired it — and the
  // id sent matches the one AddTask minted (id-stability, not the name).
  const [triggerResp] = await Promise.all([
    page.waitForResponse("**/v1/TriggerTask"),
    addedRow.locator("button", { hasText: "Trigger" }).click(),
  ]);
  expect((await triggerResp.json()).error, "TriggerTask must succeed (no envelope error)").toBeNull();
  expect(triggerId, "TriggerTask must send the same stable id AddTask minted").toBe(addedTaskId);

  // Remove round-trips via RemoveTask: the row disappears, and the id sent is again
  // the stable one (never the name).
  await addedRow.locator("button", { hasText: "Remove" }).click();
  await expect(tasks.locator(".af-task-row", { hasText: added })).toHaveCount(0, { timeout: 30_000 });
  expect(removeId, "RemoveTask must send the same stable id").toBe(addedTaskId);

  await page.unroute("**/v1/AddTask");
  await page.unroute("**/v1/TriggerTask");
  await page.unroute("**/v1/RemoveTask");

  // Return to the sessions view so the subsequent create/kill/archive flows drive the
  // rail (which is hidden while the tasks view shows).
  await page.locator('.af-viewtab[data-view="sessions"]').click();
  await expect(page.locator(".af-rail-list")).toBeVisible();
});

test.describe("create → kill (one session, two flows)", () => {
  // The ONE genuine ordering dependency in this file, and the only place serial
  // mode is warranted: create stashes the title it invented and kill consumes it,
  // so kill cannot run — or mean anything — on its own. Serial keeps the pair
  // honest: if create fails, kill SKIPS rather than failing a second time for a
  // reason that is not its own.
  //
  // Scoped to these two tests on purpose. This is the blast radius of a failure
  // here: two tests, not the 41 a file-wide serial took down in #1898.
  test.describe.configure({ mode: "serial" });

  test("create: the + New modal creates a session and its row appears", async () => {
    const created = `probe-created-${Date.now().toString(36)}`;

    // Regression guard (#1592 PR7 review): first move the CURRENT session onto a
    // NON-agent tab, so a create path that wrongly preserved activeTab would build a
    // ?tab=1 stream URL for the brand-new session (which has only the agent tab).
    await row(page, SESSION_A).click();
    await expect(page.locator(".af-main.af-main-term")).toBeVisible();
    await page.locator(".af-tabbar .af-tab-new").click();
    await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(2, { timeout: 30_000 });
    await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Terminal");

    // Capture every PTY stream WebSocket opened from here on, so we can assert the
    // new session's stream carries NO stale tab= selector.
    const streamUrls: string[] = [];
    page.on("websocket", (ws) => {
      if (ws.url().includes("/stream")) {
        streamUrls.push(ws.url());
      }
    });

    await page.locator("button.af-rail-new").click();
    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();

    // #1933 end-to-end: the backend picker is populated by the DAEMON (ListBackends
    // against the picked project), not by a list in the web. This is the only test
    // that proves the whole chain — enum → RPC → rendered options — through a real
    // daemon; the unit tests either side of it both stub their counterpart.
    const backendSelect = modal.locator('select[aria-label="Backend"]');
    await expect(backendSelect).toBeVisible();
    // Populated asynchronously, so wait for the daemon's answer rather than the
    // "repo default"-only placeholder the field is built with.
    await expect(backendSelect.locator("option")).not.toHaveCount(1, { timeout: 30_000 });
    await expect(backendSelect.locator("option")).toHaveText(
      [/^Repo default \(local\)$/, "local", "docker", "ssh", "hook"],
      { timeout: 30_000 },
    );
    // The mock repo configures no docker.image. Picking docker must state the missing
    // key AND block Create — the choose-time message standing in for the create-time
    // failure this issue is about. The reason is the daemon's own text, so this also
    // proves the CLI and the web say the same thing.
    await backendSelect.selectOption("docker");
    await expect(modal.locator(".af-modal-hint")).toHaveText(/docker\.image/);
    await expect(modal.locator("button.af-primary")).toBeDisabled();

    // Back to the repo default: the notice clears, Create is live again, and the
    // submit below sends NO backend — so this create stays local.
    await backendSelect.selectOption("");
    await expect(modal.locator(".af-modal-hint")).toHaveText("");
    await expect(modal.locator("button.af-primary")).toBeEnabled();

    // Title is required; the project picker defaults to the scoped project (redesign
    // PR2 — the first mock repo A/B live in), so the created session lands there and is
    // visible in the scoped rail. Program is left at "Repo default" (claude → the fake
    // agent). Submit with the modal's Create button.
    await modal.locator('input[aria-label="Session title"]').fill(created);
    await modal.locator("button.af-primary").click();

    // The created row lands in the rail (createSession returns the full projection,
    // which index.ts upserts + selects immediately).
    await expect(row(page, created)).toBeVisible({ timeout: 30_000 });
    await expect(modal).toBeHidden();

    // The new session is auto-selected AND attached at its AGENT tab (index 0), not
    // the tab-2 we were on: its tab bar has just the agent tab, and its terminal
    // shows the fake agent's ready marker — which it could not if the stream had
    // dialed a ?tab=<n> the session has no tab for.
    await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(1);
    await expect(page.locator(".af-tab.af-tab-active .af-tab-label")).toHaveText("Agent");
    await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 30_000 });

    // And the new session's stream URL carries no stale tab= selector (the agent tab
    // is the default, sent only for a non-agent tab).
    const lastStream = streamUrls[streamUrls.length - 1];
    expect(lastStream, "a PTY stream WS should have opened for the new session").toBeTruthy();
    expect(lastStream).not.toContain("tab=");

    // Stash it for the kill flow.
    createdTitle = created;
  });

  test("kill: the kill confirm removes the session's row", async () => {
    expect(createdTitle).not.toBe("");
    // The created session is the current selection, so the pane header shows its
    // actions. Kill it and confirm.
    await row(page, createdTitle).click();
    await expect(page.locator(".af-main.af-main-term")).toBeVisible();
    await page.locator(".af-term-head button.af-danger").click();

    const modal = page.locator(".af-modal-card");
    await expect(modal).toBeVisible();
    await modal.locator("button.af-danger").click();

    // The killed row disappears from the rail (the killed event removes it).
    await expect(row(page, createdTitle)).toHaveCount(0, { timeout: 30_000 });
  });
});

test("archive: the archive confirm retires the session out of the default rail", async () => {
  // Archive session B. Select it (click attaches, which is fine), then archive +
  // confirm.
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  // The Archive action is the ghost button labeled "Archive" in the pane header.
  await page.locator(".af-term-head button", { hasText: "Archive" }).click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator("button.af-primary").click();

  // B LEAVES the default rail (feat: hide archived by default) — archived sessions are
  // history, and the rail shows the work you can still act on. This is the real
  // archive flow, not a filter toggle: the row goes as the daemon's archived event
  // lands, which is the end-to-end proof the filter reads live projection state.
  await expect(row(page, SESSION_B)).toHaveCount(0, { timeout: 30_000 });

  // B is NOT killed, though — revealing the archive brings the very same row back,
  // muted, carrying the static ▧ archived dot (no spinner): the error/terminal states
  // keep their static glyphs (#1766).
  await setFilter(page, "archived", true);
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
  const archivedDot = row(page, SESSION_B).locator(".af-dot");
  await expect(archivedDot).toHaveClass(/af-dot-archived/);
  await expect(archivedDot).toHaveText("▧");
  await expect(archivedDot).not.toHaveClass(/af-dot-spin/);

  await resetFilter(page);
  await expect(row(page, SESSION_B)).toHaveCount(0);
});

test("#1933: a backend availability refresh must not re-enable Create mid-submit", async () => {
  // The race: a ListBackends is in flight when the user submits, and its response
  // lands DURING the create. Re-rendering the choices must not decide, on its own,
  // whether Create is clickable — the busy state is a separate reason it is
  // disabled, and clobbering it re-arms the button under an in-flight create (a
  // double-create is one Enter away).
  //
  // Both sides are held with route interception so the ordering is deterministic
  // rather than a timing hope. The create is aborted, never forwarded, so this test
  // makes no session.
  let releaseCatalog: () => void = () => {};
  const catalogHeld = new Promise<void>((resolve) => {
    releaseCatalog = resolve;
  });
  await page.route("**/v1/ListBackends", async (route) => {
    await catalogHeld;
    await route.continue();
  });
  await page.route("**/v1/CreateSession", async (route) => {
    // Held open so the modal stays busy across the assertion, then aborted so the
    // request never reaches the daemon.
    await new Promise((resolve) => setTimeout(resolve, 2000));
    await route.abort();
  });

  await page.locator("button.af-rail-new").click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator('input[aria-label="Session title"]').fill("probe-busy-race");

  const createBtn = modal.locator("button.af-primary");
  await expect(createBtn).toBeEnabled();
  await createBtn.click();
  await expect(createBtn).toBeDisabled();

  // Let the catalog land mid-create, then look AFTER the re-render has run — the
  // assertion has to outlast the event, or it would pass on the pre-render state.
  const catalogResponse = page.waitForResponse("**/v1/ListBackends");
  releaseCatalog();
  await catalogResponse;
  await page.waitForTimeout(300);
  await expect(createBtn, "an availability refresh must not re-enable Create while a create is in flight").toBeDisabled();

  // And once the create actually fails, the button comes back — the busy gate must
  // not strand the form either.
  await expect(createBtn).toBeEnabled({ timeout: 15_000 });
  await page.keyboard.press("Escape");
  await expect(modal).toBeHidden();
  await page.unroute("**/v1/ListBackends");
  await page.unroute("**/v1/CreateSession");
});

// --- the status filter (feat: hide archived by default) --------------------
//
// These run right after the archive flow, which leaves the first project holding both
// live sessions (A, the web probes) and an archived one (B) — the mix the filter
// exists to sort out. They restore the default filter before finishing, so the flows
// that follow see the rail they expect (the filter is persisted, and the page is
// shared).

test("filter (feat): the default shows every state EXCEPT archived", async () => {
  // The sane default: archived off, everything else on. Asserted through the menu the
  // user actually reads, so a default that drifts in filter.ts is caught here too.
  await page.locator(".af-rail-filter").click();
  await expect(filterItem(page, "archived")).toHaveAttribute("aria-checked", "false");
  for (const kind of ["working", "ready", "lost", "dead", "limit"]) {
    await expect(filterItem(page, kind), `${kind} must be shown by default`).toHaveAttribute("aria-checked", "true");
  }
  // The default is NOT a "narrowed" state — it must not nag with an indicator the
  // user has to go undo, and its reset is inert.
  await expect(page.locator(".af-rail-filter")).not.toHaveClass(/af-rail-filter-narrowed/);
  await expect(page.locator(".af-filter-reset")).toBeDisabled();
  await page.locator(".af-rail-title").click();

  // Live sessions show; the archived one does not.
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toHaveCount(0);
  // The count agrees with the rows on screen — it counts what the rail SHOWS, not
  // what the project holds.
  const shown = await page.locator(".af-rail-list .af-row").count();
  await expect(page.locator(".af-rail-count")).toHaveText(String(shown));
});

test("filter (feat): Show archived reveals the archived row, muted, and hides it again", async () => {
  await setFilter(page, "archived", true);
  // The archived row is back and reads as inactive: it reuses the dimmed archived
  // styling rather than looking like live work.
  const archived = row(page, SESSION_B);
  await expect(archived).toBeVisible();
  await expect(archived).toHaveClass(/af-row-archived/);
  expect(
    Number(await archived.evaluate((el) => Number(getComputedStyle(el).opacity))),
    "the archived row must render de-emphasized",
  ).toBeLessThan(1);
  // Revealing the archive IS a departure from the default, so the control says so.
  await expect(page.locator(".af-rail-filter")).toHaveClass(/af-rail-filter-narrowed/);
  await expect(page.locator(".af-rail-filter-dot")).toHaveClass(/af-rail-filter-dot-on/);

  // Unchecking puts it away again — the toggle is symmetric, not one-way.
  await setFilter(page, "archived", false);
  await expect(row(page, SESSION_B)).toHaveCount(0);
  await expect(page.locator(".af-rail-filter")).not.toHaveClass(/af-rail-filter-narrowed/);
});

test("filter (feat): the choice persists across a reload", async () => {
  await setFilter(page, "archived", true);
  await expect(row(page, SESSION_B)).toBeVisible();

  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();

  // The archived row is there on load with NO interaction — the choice came back from
  // localStorage, and the menu agrees with what the rail drew.
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
  await page.locator(".af-rail-filter").click();
  await expect(filterItem(page, "archived")).toHaveAttribute("aria-checked", "true");
  await page.locator(".af-rail-title").click();

  // ...and so does the default, once restored: the persistence is a round trip, not a
  // one-way write that can only ever add rows.
  await resetFilter(page);
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(row(page, SESSION_A)).toBeVisible({ timeout: 30_000 });
  await expect(row(page, SESSION_B)).toHaveCount(0);
});

test("filter (feat): the default hides ONLY archived, and each state's box hides exactly its own group", async ({
  browser,
}) => {
  // One row per state, SYNTHETIC and in their own context — the #1898 rule: a real
  // row cannot be pinned to a state (the daemon keeps pushing session.updated deltas
  // that land straight on top of a Snapshot fixture, and the seeded agent's liveness
  // flips LiveRunning→LiveReady a beat after seeding). A synthetic id the daemon has
  // never heard of receives no deltas, so "Ready is unchecked ⇒ the ready row is
  // gone" is deterministic by construction instead of by out-waiting a race. The
  // filter is a pure function of the row's state, so these drive the same code path.
  //
  // The context is its own for a second reason: the filter is PERSISTED, and toggling
  // six states on the shared page would leak into the serial flows that follow.
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    // Clone a REAL record so the synthetic rows land in this project's scoped rail,
    // and vary only what the filter reads. Distinct branches keep each row's text
    // free of another's name (row() matches by substring).
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const synth = (title: string, liveness: number) => ({
      ...proto,
      id: `synth-filter-${title}`,
      title,
      branch: `synth-filter-${title}`,
      liveness,
      in_flight_op: 0,
    });
    list.push(
      synth("filt-working", 1), // LiveRunning
      synth("filt-ready", 2), // LiveReady
      synth("filt-lost", 3), // LiveLost
      synth("filt-dead", 4), // LiveDead
      synth("filt-archived", 5), // LiveArchived
      synth("filt-limit", 6), // LiveLimitReached
    );
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // Each state's row and the checkbox that governs it, in the menu's own order.
  const states: Array<{ kind: string; title: string }> = [
    { kind: "working", title: "filt-working" },
    { kind: "ready", title: "filt-ready" },
    { kind: "lost", title: "filt-lost" },
    { kind: "dead", title: "filt-dead" },
    { kind: "limit", title: "filt-limit" },
    { kind: "archived", title: "filt-archived" },
  ];

  // THE DEFAULT: every state but archived. Asserted per state against a real row, so
  // a default that hides more than the archive is caught here, not by a user.
  for (const { kind, title } of states) {
    const shown = kind !== "archived";
    await expect(row(p, title), `${title} default visibility`).toHaveCount(shown ? 1 : 0, { timeout: 15_000 });
  }

  // Each box hides exactly its own group: uncheck it, that row goes and EVERY other
  // state stays — then recheck and it comes back.
  for (const { kind, title } of states) {
    if (kind === "archived") {
      continue; // already off by default; its reveal is covered above
    }
    await setFilter(p, kind, false);
    await expect(row(p, title), `${kind} unchecked ⇒ its row goes`).toHaveCount(0);
    for (const other of states) {
      if (other.kind === kind || other.kind === "archived") {
        continue;
      }
      await expect(row(p, other.title), `${kind} unchecked must not disturb ${other.kind}`).toHaveCount(1);
    }
    await setFilter(p, kind, true);
    await expect(row(p, title), `${kind} rechecked ⇒ its row is back`).toHaveCount(1);
  }

  // Narrowing to a SINGLE state is the "show me only what's working" case: uncheck
  // every other state and only that group is left standing.
  for (const { kind } of states) {
    if (kind !== "working") {
      await setFilter(p, kind, false);
    }
  }
  await expect(row(p, "filt-working")).toHaveCount(1);
  await expect(p.locator(".af-rail-list .af-row-archived")).toHaveCount(0);
  for (const { kind, title } of states) {
    if (kind !== "working") {
      await expect(row(p, title), `${title} must be filtered out`).toHaveCount(0);
    }
  }

  await ctx.close();
});

test("filter (feat): keyboard nav walks the VISIBLE rows — j never lands on a hidden one", async () => {
  // The rail's j/k order must be the rows on screen. If nav still walked the archived
  // rows the rail hides, j would select something invisible: the pane would swap to a
  // session the user cannot see in the rail, with no row highlighted.
  await setFilter(page, "archived", false);
  await row(page, SESSION_A).click();
  await page.keyboard.press("Escape"); // hand the keyboard back to the rail

  const visited: string[] = [];
  const rows = await page.locator(".af-rail-list .af-row").count();
  for (let i = 0; i < rows + 2; i++) {
    await page.keyboard.press("j");
    const sel = page.locator(".af-row-selected");
    if ((await sel.count()) === 1) {
      visited.push((await sel.locator(".af-row-title").innerText()).trim());
    }
  }
  // j walked the rail and never selected the archived session hidden from it.
  expect(visited.length, "j must move the selection").toBeGreaterThan(0);
  expect(visited, "j must never select a row the filter hides").not.toContain(SESSION_B);
  // Every row it did land on is one the rail is actually drawing.
  for (const title of new Set(visited)) {
    await expect(row(page, title), `${title} must be a visible row`).toBeVisible();
  }
});

test("delete project (#1735, redesign PR2, Fix 2): deleting an archived-only-bound project makes it go away — not a no-op", async () => {
  // Use the SECOND project (SESSION_C, no task): switch to it, then delete it from the
  // switcher menu footer. Delete archives its one live session; with no task left, the
  // repo is no longer a project — so it must DISAPPEAR from the switcher (not linger as
  // a stale archived-only entry whose delete is a silent no-op).
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo-2").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo-2");
  await expect(row(page, SESSION_C)).toBeVisible();

  // Open the switcher menu and click the reversible Delete-project footer action (it is
  // ENABLED here — there is a live session to archive). The copy makes the reversibility
  // explicit ("restorable").
  await page.locator(".af-project-switch").click();
  const del = page.locator(".af-project-menu .af-project-delete");
  await expect(del).toBeEnabled();
  await del.click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await expect(modal).toContainText("restorable");
  await modal.locator("button.af-danger").click();

  // The project ACTUALLY GOES AWAY: SESSION_C is archived, the repo now has no live
  // session and no task, so it drops from the derivation and the selection falls back
  // to the first project (its live sessions the most-recently-active).
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo", { timeout: 30_000 });
  await page.locator(".af-project-switch").click();
  await expect(projectItem(page, "mock-repo-2")).toHaveCount(0);
  await expect(projectItem(page, "mock-repo")).toHaveCount(1);
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu")).toBeHidden();

  // Back on the (fallen-back) first project, its live rail is intact.
  await expect(row(page, SESSION_A)).toBeVisible();
});

// NOTE on #1675 PR4 (ended PTY → "exited", not a reconnect loop): this is already
// wired end-to-end — the daemon emits a MsgExit control frame on session-end
// (daemon/ws_pty.go, covered by the Go handler tests), and terminal.ts settles to an
// "exited" state + stops reconnecting on it (see onControl's "exit" arm and the
// TerminalStatus="exited" pane header). It is NOT browser-tested here: a real
// mid-stream exit can't be forced without killing the session (which removes the row
// and disposes the terminal before the exit renders), and mocking the per-session WS
// against the loopback HTTP daemon proved unreliable in this harness. The Go side is
// the regression guard for the emit; the client arm is exercised by the daemon's own
// session-end path in manual play-testing.

test("empty state (#1592 PR9): an empty Snapshot renders the empty rail + placeholder", async () => {
  // Force the authoritative Snapshot to come back empty, then reload so bootstrap
  // re-seeds the rail from it. HTTP routing (unlike WS mocking) is deterministic
  // against the loopback daemon, so this reliably drives the zero-sessions state the
  // seeded harness otherwise never reaches. Runs LAST — it reloads and mocks Snapshot,
  // so it must not precede the create/kill/archive flows.
  await page.route("**/v1/Snapshot", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ data: { instances: [] }, error: null }),
    });
  });
  // Empty the task list too (redesign PR2): a task-only project would otherwise keep a
  // project selected, so the GLOBAL no-projects empty state needs both planes empty.
  await page.route("**/v1/ListTasks", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ data: { tasks: [] }, error: null }),
    });
  });
  await page.reload();

  // The authed shell still comes up (tokenless loopback), but the rail shows its
  // empty-state copy instead of rows, and the count reads 0 — the empty state renders
  // as designed rather than a broken/blank shell.
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(page.locator(".af-rail-empty")).toContainText("No sessions yet");
  await expect(page.locator(".af-rail-count")).toHaveText("0");
  // With nothing selected the main pane is the "Select a session" placeholder.
  await expect(page.locator(".af-main-empty")).toContainText("Select a session");

  await page.unroute("**/v1/Snapshot");
  await page.unroute("**/v1/ListTasks");
});

test("filter (feat): a project whose sessions are ALL archived reads as empty, and the filter still reveals them", async ({
  browser,
}) => {
  // An archived-only project is only reachable when something else keeps the repo a
  // project: a scheduled task. projectSummaries makes a repo a project if it has a LIVE
  // session OR a task, so an archived-only repo with no task stops being one and the
  // switcher drops it entirely (the deliberate #1735 rule — a stale archived-only entry
  // whose delete is a silent no-op must never list).
  //
  // So the fixture is a SYNTHETIC repo: archived sessions + a task to keep it a project.
  // Synthetic and in its own context for the #1898 reason — flipping the REAL rows to
  // archived does not hold, because the events plane keeps pushing session.updated for
  // them and applyEvent lands the daemon's truth straight back on top of the fixture
  // (an earlier draft of this test did exactly that, and the rail refilled with live
  // rows before the first assertion). Ids the daemon has never heard of receive no
  // deltas, so the state is the only state there will ever be.
  const ARCHIVED_REPO = "/work/mock-archived-only";
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const snap = body?.data as { instances?: Array<Record<string, unknown> & { title: string }> };
    const list = snap?.instances ?? [];
    const proto = { ...(list.find((s) => s.title === SESSION_A) ?? {}) };
    const shelved = (title: string) => ({
      ...proto,
      id: `synth-shelved-${title}`,
      title,
      branch: `synth-shelved-${title}`,
      liveness: 5, // LiveArchived
      in_flight_op: 0,
      worktree: { ...(proto.worktree as Record<string, unknown>), repo_path: ARCHIVED_REPO },
    });
    list.push(shelved("shelf-one"), shelved("shelf-two"));
    if (snap) {
      snap.instances = list;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  // The task is what keeps the archived-only repo a project at all.
  await p.route("**/v1/ListTasks", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    const data = body?.data as { tasks?: Array<Record<string, unknown>> };
    const tasks = data?.tasks ?? [];
    const proto = { ...(tasks[0] ?? {}) };
    tasks.push({
      ...proto,
      id: "synth-shelved-task",
      name: "shelf-task",
      project_path: ARCHIVED_REPO,
    });
    if (data) {
      data.tasks = tasks;
    }
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // The archived-only repo IS a project (its task keeps it listed) — select it.
  await p.locator(".af-project-switch").click();
  await projectItem(p, "mock-archived-only").click();
  await expect(p.locator(".af-project-switch-name")).toHaveText("mock-archived-only");

  // The rail is NOT a blank panel: it says the project has no active work, and —
  // crucially — accounts for what it is hiding rather than leaving the sessions
  // silently missing.
  const empty = p.locator(".af-rail-empty-project");
  await expect(empty).toBeVisible({ timeout: 15_000 });
  await expect(empty).toContainText("No active sessions in");
  await expect(empty).toContainText("mock-archived-only");
  await expect(empty).toContainText("2 archived hidden");
  await expect(p.locator(".af-rail-list .af-row")).toHaveCount(0);
  await expect(p.locator(".af-rail-count")).toHaveText("0");

  // The hint's inline "Show archived" is a real control, not decoration: it reveals the
  // archive in place, from the very empty state that reported it.
  await empty.locator(".af-rail-show-archived").click();
  await expect(p.locator(".af-rail-list .af-row")).toHaveCount(2);
  await expect(p.locator(".af-rail-count")).toHaveText("2");
  await expect(row(p, "shelf-one")).toHaveClass(/af-row-archived/);
  await expect(row(p, "shelf-two")).toHaveClass(/af-row-archived/);
  // The empty state still stands above them, and no longer claims to be hiding
  // anything: "no active sessions" is a statement about live work, not a claim that
  // the rail is bare.
  await expect(empty).toBeVisible();
  await expect(empty).not.toContainText("archived hidden");

  await ctx.close();
});

// --- theme (redesign PR1): design tokens + light/dark ----------------------
//
// These run LAST: the first reloads with a persisted dark choice (to prove the boot
// stamp beats first paint), and both mutate localStorage + data-theme. They assert on
// the chrome (data-theme + token-driven computed colors), not on session state, so
// the earlier flows are undisturbed. The final test resets to Auto + clears the saved
// choice so the page is left in its default theme.

/** The computed background color of a selector, for a token-driven-color diff. */
async function bgColor(p: Page, selector: string): Promise<string> {
  return p.evaluate((sel) => {
    const node = document.querySelector(sel);
    return node ? getComputedStyle(node).backgroundColor : "";
  }, selector);
}

/** The resolved value of a CSS custom property on :root, for a token-flip diff. */
async function cssVar(p: Page, name: string): Promise<string> {
  return p.evaluate((n) => getComputedStyle(document.documentElement).getPropertyValue(n).trim(), name);
}

test("theme (redesign PR1): a saved dark choice is stamped before the app mounts — no flash", async () => {
  // Persist a dark choice, then install a document-start trap on #app.replaceChildren
  // (how index.ts mounts its content into #app) that records data-theme AT THE EXACT
  // synchronous instant the app first mounts. Because the boot stamp runs at index.ts
  // module top — earlier in the SAME synchronous module turn than mount() — the trap
  // must see data-theme already "dark". This is race-free (no rAF/microtask timing),
  // unlike a paint- or observer-based probe.
  await page.evaluate(() => localStorage.setItem("af-theme", "dark"));
  await page.addInitScript(() => {
    interface ThemeProbe {
      __afMountTheme?: string | null;
    }
    const w = window as unknown as ThemeProbe;
    w.__afMountTheme = "__unset__";
    const orig = Element.prototype.replaceChildren;
    Element.prototype.replaceChildren = function (this: Element, ...args: (Node | string)[]): void {
      if (this.id === "app" && w.__afMountTheme === "__unset__") {
        w.__afMountTheme = document.documentElement.getAttribute("data-theme");
      }
      return orig.apply(this, args);
    };
  });
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();

  const atMount = await page.evaluate(
    () => (window as unknown as { __afMountTheme?: string | null }).__afMountTheme,
  );
  // data-theme was already "dark" the instant the app mounted: no light→dark flash.
  expect(atMount).toBe("dark");
  // And it stuck: <html data-theme="dark"> after the app is up.
  expect(await page.evaluate(() => document.documentElement.getAttribute("data-theme"))).toBe("dark");
  // The dark theme option reads active in the appbar toggle.
  await expect(page.locator('.af-theme-opt[data-theme-opt="dark"]')).toHaveClass(/af-theme-opt-active/);
});

test("theme (redesign PR1): toggling Light vs Dark changes token-driven colors live", async () => {
  // Force Dark and capture a token-driven color (the rail surface).
  await page.locator('.af-theme-opt[data-theme-opt="dark"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "dark");
  const darkRail = await bgColor(page, ".af-rail");
  const darkBody = await bgColor(page, "body");
  // The web/iframe-pane tokens (fixed in this PR to match the terminal pane) resolve
  // to their dark values.
  const darkTerm = await cssVar(page, "--af-bg-term");
  const darkBorderSubtle = await cssVar(page, "--af-border-subtle");

  // Toggle to Light: the SAME selectors resolve to different token values, proving the
  // chrome is driven by the CSS custom properties, not hardcoded colors.
  await page.locator('.af-theme-opt[data-theme-opt="light"]').click();
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  const lightRail = await bgColor(page, ".af-rail");
  const lightBody = await bgColor(page, "body");
  const lightTerm = await cssVar(page, "--af-bg-term");
  const lightBorderSubtle = await cssVar(page, "--af-border-subtle");

  expect(lightRail).not.toBe(darkRail);
  expect(lightBody).not.toBe(darkBody);
  // The web-pane canvas + separator tokens flip too, so the embedded web pane reads
  // correctly in both themes (the dark-mode regression this PR fixes).
  expect(lightTerm).not.toBe(darkTerm);
  expect(lightBorderSubtle).not.toBe(darkBorderSubtle);
  // The light rail surface is the white token (#ffffff → rgb(255, 255, 255)).
  expect(lightRail).toBe("rgb(255, 255, 255)");
  await expect(page.locator('.af-theme-opt[data-theme-opt="light"]')).toHaveClass(/af-theme-opt-active/);

  // Reset to Auto and clear the saved choice so the page is left in its default theme.
  // Auto removes data-theme entirely (follow prefers-color-scheme).
  await page.locator('.af-theme-opt[data-theme-opt="auto"]').click();
  await expect
    .poll(() => page.evaluate(() => document.documentElement.hasAttribute("data-theme")))
    .toBe(false);
  await page.evaluate(() => localStorage.removeItem("af-theme"));
});

// The #1812 regression guard: a tab created or deleted by ANY actor other than
// this browser window — an agent running `af sessions tab-create` inside its own
// session, the TUI, a script, a second window — must reach an already-open client
// live. Before the fix the daemon persisted the roster silently, so the SPA (which
// only re-Snapshots after its OWN mutation, and never polls) stayed stale until the
// user reloaded or an unrelated session.updated happened to repair it as a
// side-effect. On a quiet session that repair never comes — which broke the web
// tab's stated purpose: letting an agent inject a live browser view into the user's
// screen.
//
// The mutation is driven through the real `af` CLI over the control socket, not the
// HTTP API the SPA uses, so this also proves the event reaches the browser
// regardless of which transport drove the change (both mutate one Manager).
test("#1812: a tab created/deleted out-of-band reaches the open client with no reload", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const OOB_TAB = "livepreview";

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const oob = tabbar.locator(".af-tab", { hasText: OOB_TAB });
  await expect(oob).toHaveCount(0);

  // Stamp the live document. A reload wipes this, so asserting it survives is what
  // makes "WITHOUT a reload" a real claim rather than an assumption — the whole
  // point of #1812 is that the user must not have to refresh.
  await page.evaluate(() => {
    (window as Window & { __af1812?: boolean }).__af1812 = true;
  });
  const notReloaded = (): Promise<boolean> =>
    page.evaluate(() => (window as Window & { __af1812?: boolean }).__af1812 === true);

  // Repro A: an agent injects a live browser view mid-work. The tab must land in
  // this window, which did not create it.
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3200", "--name", OOB_TAB);
  await expect(oob, "a tab created out-of-band must appear without a reload").toHaveCount(1, {
    timeout: 15_000,
  });
  expect(await notReloaded(), "the tab must arrive on the LIVE page, not via a reload").toBe(true);

  // The delete side: the tab must vanish from a window that did not close it.
  af("sessions", "tab-delete", SESSION_A, "--name", OOB_TAB);
  await expect(oob, "a tab deleted out-of-band must disappear without a reload").toHaveCount(0, {
    timeout: 15_000,
  });
  expect(await notReloaded()).toBe(true);
});

// The other half of #1812/#1815's bill: the event must arrive WITHOUT disturbing
// what the user is doing. Delivering a tab into the open window is only half the
// feature if it rips the pane they are reading out from under them.
//
// The regression: reconcile re-inserted the split DOM on every resync, and
// re-inserting a pane container detaches it — even when it goes straight back into
// the same parent. The browser drops the scroll offset of every scrollable
// descendant on detach, so the xterm viewport rewound to 0 while xterm's own scroll
// position (ydisp) stayed put. That desync is what killed the WHEEL: a viewport
// pinned at 0 has nothing left to scroll up, emits no scroll event, and xterm never
// moves. It looked like "scrolling is broken", and it healed on the next chunk of
// output (which resyncs scrollTop) — so it bit hardest on a QUIET pane, which is
// exactly the pane someone is scrolled up reading.
//
// Driven through the real `af` CLI, like the #1812 test above: a tab created by
// someone else, on the session being watched, while its scrollback is parked.
test("#1815: a tab created out-of-band must not rewind the scrolled terminal", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const SCROLL_TAB = "scrollprobe";
  const SHELL_TAB = "scrollshell";

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER);

  // Scrollback needs a real PTY: the seeded agent tab is a fake agent that only
  // echoes. Create a shell tab under a name of our own rather than clicking + — the
  // suite is serial and "Terminal" is the default name every other tab test uses,
  // so a leaked one would make this ambiguous.
  const tabbar = page.locator(".af-tabbar");
  // Whatever this serial suite has left on the session — restored at the end, since
  // the tests after this one inherit it.
  const tabsBefore = await tabbar.locator(".af-tab").count();
  af("sessions", "tab-create", SESSION_A, "--command", "bash", "--name", SHELL_TAB);
  const shell = tabbar.locator(".af-tab", { hasText: SHELL_TAB });
  await expect(shell).toHaveCount(1, { timeout: 15_000 });
  await shell.click();
  await expect(shell).toHaveClass(/af-tab-active/);
  await expect(page.locator(".af-term-meta")).toContainText("Live");

  // More than one screen of numbered output, so which line is on screen is exact.
  // xterm renders only the VISIBLE rows, so the pane's text IS its viewport.
  const host = page.locator(".af-term-host");
  await page.keyboard.type("for i in $(seq 1 200); do echo scrollback-line-$i; done");
  await page.keyboard.press("Enter");
  await expect(host).toContainText("scrollback-line-200", { timeout: 15_000 });

  // Park the view up in the scrollback, the way a user reads back through output.
  await host.hover();
  await page.mouse.wheel(0, -900);
  await expect(host, "the wheel must move the viewport off the bottom").not.toContainText(
    "scrollback-line-200",
  );
  const viewport = page.locator(".af-term-host .xterm-viewport");
  const parked = await viewport.evaluate((el) => el.scrollTop);
  expect(parked, "a scrolled-up viewport sits off 0").toBeGreaterThan(0);
  const reading = await host.textContent();

  // Someone else adds a tab to the session being watched. #1815 fans the
  // session.updated out to this window, which re-projects the whole session.
  const probe = page.locator(".af-tabbar .af-tab", { hasText: SCROLL_TAB });
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3201", "--name", SCROLL_TAB);
  // The event really landed here — otherwise the assertions below would pass on a
  // client that never re-projected, proving nothing.
  await expect(probe, "the out-of-band tab must reach this window").toHaveCount(1, {
    timeout: 15_000,
  });

  // The pane is untouched: same shell tab, same line, same viewport offset.
  await expect(host).not.toContainText("scrollback-line-200");
  expect(await viewport.evaluate((el) => el.scrollTop), "the viewport must not rewind").toBe(parked);
  expect(await host.textContent(), "the parked view must not move").toBe(reading);

  // ...and the wheel still WORKS afterwards, which is the symptom that was reported:
  // the rewound viewport had nothing left to scroll, so wheel-up went dead.
  await page.mouse.wheel(0, -300);
  await expect
    .poll(async () => viewport.evaluate((el) => el.scrollTop), {
      message: "wheel-up must still scroll after an out-of-band resync",
    })
    .toBeLessThan(parked);

  // Put the roster back: this suite is serial, so both tabs would otherwise leak
  // into every test after this one.
  af("sessions", "tab-delete", SESSION_A, "--name", SCROLL_TAB);
  await expect(probe).toHaveCount(0, { timeout: 15_000 });
  af("sessions", "tab-delete", SESSION_A, "--name", SHELL_TAB);
  await expect(shell).toHaveCount(0, { timeout: 15_000 });
  // The roster this test perturbed is back as it found it. Which tab the pane FALLS
  // BACK to once its own tab is deleted out from under it is a separate contract
  // (the tab tests above own it), so this stops at the roster — but the suite is
  // serial, so park the pane back on the agent tab rather than leave the next test
  // to inherit whatever the fallback picked.
  await expect(tabbar.locator(".af-tab")).toHaveCount(tabsBefore, { timeout: 15_000 });
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
});

// The same rewind, one gesture later (local Codex review of #1894).
//
// Resizing a divider persists through setRatio, which rebuilds every SplitNode —
// so the root comes back as a NEW object even though the drag already applied the
// ratios to the live DOM and nothing structural moved. That left builtTree pointing
// at the superseded root, and the next roster resync saw tree !== builtTree, took
// the rebuild branch, and rewound the scrolled terminal exactly as before. The
// primary fix held right up until the user touched a divider.
//
// The resize ITSELF is allowed to move the viewport (a narrower pane reflows, and
// xterm rewraps), so the baseline is re-read once the resize has settled. What must
// not move it is the RESYNC that follows.
test("#1815: a resize must not re-arm the rewind on the next out-of-band resync", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const SHELL_TAB = "resizeshell";
  const PROBE_TAB = "resizeprobe";

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  // Park on the agent tab explicitly: layouts are retained per session, so this
  // pane shows whatever the last test left it on, not necessarily the agent.
  const tabbar = page.locator(".af-tabbar");
  const tabsBefore = await tabbar.locator(".af-tab").count();
  await tabbar.locator(".af-tab", { hasText: "Agent" }).click();
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });

  // A shell tab to scroll, and a split so there IS a divider to drag.
  af("sessions", "tab-create", SESSION_A, "--command", "bash", "--name", SHELL_TAB);
  const shellTab = tabbar.locator(".af-tab", { hasText: SHELL_TAB });
  await expect(shellTab).toHaveCount(1, { timeout: 15_000 });
  await shellTab.click();
  await expect(shellTab).toHaveClass(/af-tab-active/);
  await expect(page.locator(".af-term-meta")).toContainText("Live");

  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const shellPane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(shellPane).toHaveCount(1);

  // Fill the shell pane's scrollback and park the view up in it.
  await shellPane.locator(".af-pane-host").click();
  await expect(page.locator(".af-term-meta")).toContainText("Live");
  await page.keyboard.type("for i in $(seq 1 200); do echo resize-line-$i; done");
  await page.keyboard.press("Enter");
  await expect(shellPane).toContainText("resize-line-200", { timeout: 15_000 });
  await shellPane.hover();
  await page.mouse.wheel(0, -900);
  await expect(shellPane).not.toContainText("resize-line-200");

  // Drag the divider — real pointer events, since it captures the pointer.
  const divider = page.locator(".af-term-host .af-divider").first();
  const box = await divider.boundingBox();
  expect(box, "a split must have a divider to drag").toBeTruthy();
  const bx = (box as { x: number; width: number }).x + (box as { width: number }).width / 2;
  const by = (box as { y: number; height: number }).y + (box as { height: number }).height / 2;
  await page.mouse.move(bx, by);
  await page.mouse.down();
  await page.mouse.move(bx - 80, by, { steps: 8 });
  await page.mouse.up();

  // Let the resize settle (the fit is debounced) and take the post-resize baseline:
  // the reflow above is legitimate, the resync below is not.
  const viewport = shellPane.locator(".xterm-viewport");
  await expect
    .poll(async () => viewport.evaluate((el) => el.scrollTop), {
      message: "the resized pane must still be parked off the bottom",
      timeout: 10_000,
    })
    .toBeGreaterThan(0);
  await page.waitForTimeout(500);
  const parked = await viewport.evaluate((el) => el.scrollTop);
  const reading = await shellPane.textContent();

  // The resync that used to rewind it: someone else adds a tab.
  const probe = tabbar.locator(".af-tab", { hasText: PROBE_TAB });
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--port", "3202", "--name", PROBE_TAB);
  await expect(probe, "the out-of-band tab must reach this window").toHaveCount(1, {
    timeout: 15_000,
  });

  expect(
    await viewport.evaluate((el) => el.scrollTop),
    "a resync after a resize must not rewind the viewport",
  ).toBe(parked);
  expect(await shellPane.textContent(), "the parked view must not move").toBe(reading);

  // Put it all back for the serial tests after this one.
  af("sessions", "tab-delete", SESSION_A, "--name", PROBE_TAB);
  await expect(probe).toHaveCount(0, { timeout: 15_000 });
  await page
    .locator(".af-term-host .af-pane", { hasText: READY_MARKER })
    .locator(".af-pane-close")
    .click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  af("sessions", "tab-delete", SESSION_A, "--name", SHELL_TAB);
  await expect(shellTab).toHaveCount(0, { timeout: 15_000 });
  await expect(tabbar.locator(".af-tab")).toHaveCount(tabsBefore, { timeout: 15_000 });
});

// The #1812 review guard (Codex): a close is async, and `next` is computed from the
// index that was active when it was ISSUED. A slow CloseTab leaves a window in which
// the user can select another tab — and re-pointing the pane from that stale index
// afterwards would yank their selection away. The user's newer intent must win. The
// roster event races the same window and must still be free to pass the guard, which
// is why it keys off explicit layout mutations rather than the tab index itself.
test("#1812 review: a close held in flight must not clobber a tab the user picks meanwhile", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // Two named tabs: "doomed" is closed, "keeper" is focused when the close is issued
  // (so the stale `next` would point back at it). Named via the CLI so neither the
  // labels nor the ordinals depend on what earlier tests left behind.
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", "doomed");
  af("sessions", "tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", "keeper");
  const doomed = tabbar.locator(".af-tab", { hasText: "doomed" });
  const keeper = tabbar.locator(".af-tab", { hasText: "keeper" });
  await expect(doomed).toHaveCount(1, { timeout: 15_000 });
  await expect(keeper).toHaveCount(1, { timeout: 15_000 });

  await keeper.click();
  await expect(active).toHaveText("keeper");

  // Hold CloseTab open so the mid-flight window is deterministic rather than a race
  // against a fast local daemon.
  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await doomed.locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // Mid-flight: the user picks the agent tab. This is the intent the close must respect.
  await tabbar.locator(".af-tab", { hasText: "Agent" }).first().click();
  await expect(active).toHaveText("Agent");

  // Let the close land. Its `next` was computed against "keeper" — applying it now
  // would snap the pane back to "keeper" and lose the user's choice.
  releaseClose?.();
  await expect(doomed).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the user's mid-flight selection must survive the close").toHaveText("Agent");

  await page.unroute("**/v1/CloseTab");
  af("sessions", "tab-delete", SESSION_A, "--name", "keeper");
  await expect(keeper).toHaveCount(0, { timeout: 15_000 });
});

// The post-merge Codex findings on #1815. All three are the same shape: #1815 made
// out-of-band roster changes reach an open client LIVE, which turned three latent
// ordinal-vs-identity assumptions into reachable misroutes. Each drives the exact
// scenario Codex described and asserts on the tab the user is actually looking at.

test("#1815 review: a concurrent out-of-band close cannot re-point the pane to a neighbour", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  const tabbar = page.locator(".af-tabbar");
  await expect(tabbar).toBeVisible();
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");
  // Earlier flows leave their own tabs on this session, so every count here is
  // relative to what is already on the bar rather than an absolute the next test to
  // leave a tab behind would break.
  const baseline = await tabbar.locator(".af-tab").count();

  // Codex's scenario: this window closes t3 while ANOTHER client closes the LOWER t1
  // first. The old code re-pointed the pane by subtracting its own close's shift from
  // the pre-close ordinal — but t1's removal had already shifted t4 down underneath
  // it, so the result named t5, the neighbour, and the pane the user was reading was
  // torn down to show the wrong tab.
  // One at a time, each awaited: the events hub DROPS a publish for a subscriber that
  // is behind rather than blocking the daemon (clients re-Snapshot to resync), so a
  // burst of CLI mutations can outrun the browser and lose a roster. Real actors don't
  // burst, and this test is about ordering, not backpressure.
  for (const name of ["t1", "t2", "t3", "t4", "t5"]) {
    af("tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 15_000 });
  }

  // Click the LABEL, not the button. A bar this crowded shrinks each tab until the
  // button's CENTRE — where a bare .click() lands — is its × rather than its label,
  // so clicking the button would close the tab instead of selecting it. The label's
  // click still bubbles to the button's handler, which is the real select path.
  await tabbar.locator(".af-tab", { hasText: "t4" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("t4");

  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await tabbar.locator(".af-tab", { hasText: "t3" }).locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // The other client closes a LOWER tab mid-flight. Waiting for it to leave the bar
  // proves the #1815 event landed and the roster shifted BEFORE the close resolves —
  // which is the whole point: this is the race #1815 itself made reachable.
  af("tab-delete", SESSION_A, "--name", "t1");
  await expect(tabbar.locator(".af-tab", { hasText: "t1" })).toHaveCount(0, { timeout: 15_000 });

  releaseClose?.();
  await expect(tabbar.locator(".af-tab", { hasText: "t3" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the pane must still show the tab the user was on, not its neighbour").toHaveText("t4");

  await page.unroute("**/v1/CloseTab");
  for (const name of ["t2", "t4", "t5"]) {
    af("tab-delete", SESSION_A, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(0, { timeout: 15_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

test("#1815 review: a pane focused mid-close keeps its own tab", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // The guard keyed off layoutGeneration, which counted tab rebinds and splits but
  // NOT a change of which PANE holds focus — and the guarded write (setFocusedTab)
  // targets the focused pane. So a close held in flight could rebind the pane the
  // user had just clicked, using an index computed for the pane that issued it.
  // PROCESS tabs, not web tabs: this test has to move focus BETWEEN panes by clicking
  // them, and a web tab's pane is an iframe that swallows the mousedown the pane's
  // focus handler listens for. A PTY pane is the surface a user actually clicks
  // between, which is the gesture the guard has to survive.
  const baseline = await tabbar.locator(".af-tab").count();
  for (const name of ["k1", "k2"]) {
    af("tab-create", SESSION_A, "--command", "sleep 300", "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 30_000 });
  }

  await tabbar.locator(".af-tab", { hasText: "k2" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("k2");

  // Split, so there are two panes and "which pane is focused" is a real question. The
  // agent pane is the one streaming the ready marker; the other is k2's.
  await dragTabToPane(page, "Agent", "right");
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(2, { timeout: 15_000 });
  await expect(page.locator(".af-term-host")).toContainText(READY_MARKER, { timeout: 15_000 });
  const agentPane = page.locator(".af-term-host .af-pane", { hasText: READY_MARKER });
  const k2Pane = page.locator(".af-term-host .af-pane", { hasNotText: READY_MARKER });
  await expect(agentPane).toHaveCount(1);
  await expect(k2Pane).toHaveCount(1);

  // Focus the k2 pane — this is the pane the close is issued from, and the one whose
  // index the close's `next` is computed against.
  await k2Pane.locator(".af-pane-host").click();
  await expect(active).toHaveText("k2");

  let releaseClose: (() => void) | undefined;
  const closeHeld = new Promise<void>((resolve) => {
    releaseClose = resolve;
  });
  let closeInFlight = false;
  await page.unroute("**/v1/CloseTab");
  await page.route("**/v1/CloseTab", async (route) => {
    closeInFlight = true;
    await closeHeld;
    await route.continue();
  });

  await tabbar.locator(".af-tab", { hasText: "k1" }).locator(".af-tab-close").click();
  await expect.poll(() => closeInFlight, { timeout: 15_000 }).toBe(true);

  // Mid-flight the user clicks the OTHER pane. That is a deliberate move of focus, and
  // the close's stale index means nothing for the pane they just chose.
  await agentPane.locator(".af-pane-host").click();
  await expect(active).toHaveText("Agent");

  releaseClose?.();
  await expect(tabbar.locator(".af-tab", { hasText: "k1" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "the pane the user focused mid-close must keep its own tab").toHaveText("Agent");

  // Collapse the split and restore A's single-tab baseline for the later flows.
  await page.unroute("**/v1/CloseTab");
  await agentPane.locator(".af-pane-close").click();
  await expect(page.locator(".af-term-host .af-pane")).toHaveCount(1, { timeout: 15_000 });
  af("tab-delete", SESSION_A, "--name", "k2");
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

test("#1815 review: a retained layout follows its tab when the roster changes while away", async () => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, "sessions", ...args], { stdio: "pipe" });
  };

  const tabbar = page.locator(".af-tabbar");
  const active = page.locator(".af-tab.af-tab-active .af-tab-label");

  // The split view retains a layout per session, but only ever remapped the SHOWN
  // session's panes onto a changed roster; a session the user had navigated away from
  // was merely clamped on return. Its leaves hold ORDINALS, so an out-of-band close
  // while away — which #1815 now delivers live — silently rebound them to whatever
  // slid into the slot. Tabs [Agent,r1,r2,r3] with the pane on r2 (index 2): drop r1
  // and the stale ordinal 2 is r3, an entirely different tab.
  await row(page, SESSION_A).click();
  await expect(tabbar).toBeVisible();
  const baseline = await tabbar.locator(".af-tab").count();
  for (const name of ["r1", "r2", "r3"]) {
    af("tab-create", SESSION_A, "--kind", "web", "--url", WEBTAB_EXTERNAL_URL, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(1, { timeout: 15_000 });
  }
  await tabbar.locator(".af-tab", { hasText: "r2" }).locator(".af-tab-label").click();
  await expect(active).toHaveText("r2");

  // Navigate away, so A's layout is only RETAINED — not the live one being reconciled.
  // B is archived by the time this runs, so reveal the archive to reach it (feat: hide
  // archived by default). B stays the right target for exactly the reason it always
  // was: an agent-tab-only session no other test has moved off its pane.
  await setFilter(page, "archived", true);
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  await resetFilter(page);

  af("tab-delete", SESSION_A, "--name", "r1");

  // Back to A: its pane must still be on r2, which is now index 1.
  await row(page, SESSION_A).click();
  await expect(tabbar.locator(".af-tab", { hasText: "r1" })).toHaveCount(0, { timeout: 15_000 });
  await expect(active, "a retained pane must follow its TAB across an out-of-band close, not its slot").toHaveText(
    "r2",
  );

  for (const name of ["r2", "r3"]) {
    af("tab-delete", SESSION_A, "--name", name);
    await expect(tabbar.locator(".af-tab", { hasText: name })).toHaveCount(0, { timeout: 15_000 });
  }
  await expect(tabbar.locator(".af-tab")).toHaveCount(baseline, { timeout: 15_000 });
});

// --- PWA: favicon, theme-color, manifest, service worker, install (feat) --------
//
// These run in their OWN context rather than on the shared serial `page`, because a
// service worker and a localStorage dismissal are per-origin state that would leak
// into every test after them. A fresh context each time also means each test starts
// from "never registered, never dismissed", which is the state a real first visit has.
//
// Worth stating plainly: the SPA registers the worker on EVERY load, so every other
// test in this file already runs with it active. The suite staying green is itself
// the broadest evidence the worker doesn't disturb the app — these tests are the
// specific, deliberate proofs on top of that.

/**
 * Opens the app in a fresh page and returns once a service worker is not merely
 * registered but actually CONTROLLING it — the state in which it could do damage, and
 * therefore the only state worth asserting a bypass against.
 *
 * The reload is not defensive padding, it is the point. On a FIRST visit the shell
 * (page, bundle, CSS) is fetched before the worker exists, so those requests never
 * pass through it and nothing is cached; the worker only takes over via clients.claim
 * once everything is already on screen. It is the SECOND load whose requests actually
 * reach the fetch handler. So this reloads to reach the steady state a returning user
 * is in, which is the state worth testing — a first-load-only check would assert the
 * worker does nothing, which is true and useless.
 */
async function openControlledByWorker(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  await page.evaluate(async () => {
    await navigator.serviceWorker.ready;
    if (navigator.serviceWorker.controller) {
      return;
    }
    // clients.claim() takes control of an already-open page asynchronously, so the
    // controller may land just after `ready`.
    await new Promise<void>((resolve) => {
      navigator.serviceWorker.addEventListener("controllerchange", () => resolve(), { once: true });
    });
  });
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
}

/** Every URL currently held in every Cache Storage bucket, with the bucket names. */
async function cacheContents(page: Page): Promise<{ names: string[]; urls: string[] }> {
  return page.evaluate(async () => {
    const names = await caches.keys();
    const urls: string[] = [];
    for (const name of names) {
      const cache = await caches.open(name);
      for (const request of await cache.keys()) {
        urls.push(request.url);
      }
    }
    return { names, urls };
  });
}

test("the shell links an SVG favicon, PNG fallbacks, an apple-touch icon, and the manifest", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");

  await expect(p.locator('link[rel="icon"][type="image/svg+xml"]')).toHaveAttribute("href", "/icons/icon.svg");
  await expect(p.locator('link[rel="icon"][sizes="32x32"]')).toHaveAttribute("href", "/icons/favicon-32.png");
  await expect(p.locator('link[rel="icon"][sizes="16x16"]')).toHaveAttribute("href", "/icons/favicon-16.png");
  await expect(p.locator('link[rel="apple-touch-icon"]')).toHaveAttribute("href", "/icons/apple-touch-icon-180.png");
  await expect(p.locator('link[rel="manifest"]')).toHaveAttribute("href", "/manifest.webmanifest");

  // Linking them is half the claim; the daemon actually serving them is the other
  // half, and a link to a 404 looks identical in the DOM.
  const results = await p.evaluate(async () => {
    const hrefs = [...document.querySelectorAll('link[rel="icon"], link[rel="apple-touch-icon"]')].map(
      (l) => (l as HTMLLinkElement).getAttribute("href") ?? "",
    );
    return Promise.all(
      hrefs.map(async (href) => {
        const res = await fetch(href);
        return { href, status: res.status, type: res.headers.get("content-type") ?? "" };
      }),
    );
  });
  expect(results.length).toBeGreaterThanOrEqual(4);
  for (const r of results) {
    expect(r.status, `${r.href} must be served`).toBe(200);
    expect(r.type, `${r.href} must carry an image type`).toMatch(/^image\/(png|svg\+xml)/);
  }
  await ctx.close();
});

test("theme-color is declared per scheme, and an explicit theme choice repoints the chrome (#1826)", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  const metas = p.locator('meta[name="theme-color"]');
  await expect(metas).toHaveCount(2);
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#ffffff");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");

  // The audit item is "the chrome matches the app theme", and per-scheme metas alone
  // don't deliver that: they follow the OS, so an explicit Dark on a light OS would
  // leave a white chrome over a dark app. Picking Dark must collapse BOTH metas.
  await p.locator('.af-theme-opt[data-theme-opt="dark"]').click();
  await expect(p.locator("html")).toHaveAttribute("data-theme", "dark");
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#141a22");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");

  await p.locator('.af-theme-opt[data-theme-opt="light"]').click();
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#ffffff");

  // Back to Auto and the metas go per-scheme again, handing the decision back to the
  // media queries.
  await p.locator('.af-theme-opt[data-theme-opt="auto"]').click();
  await expect(p.locator('meta[name="theme-color"][media*="light"]')).toHaveAttribute("content", "#ffffff");
  await expect(p.locator('meta[name="theme-color"][media*="dark"]')).toHaveAttribute("content", "#141a22");
  await ctx.close();
});

test("the manifest is fetched by the browser, typed application/manifest+json, and install-complete", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");

  const manifest = await p.evaluate(async () => {
    const res = await fetch("/manifest.webmanifest");
    return { status: res.status, type: res.headers.get("content-type"), body: await res.json() };
  });
  expect(manifest.status).toBe(200);
  // Go has no .webmanifest in its MIME table and we serve nosniff, so without the
  // daemon's explicit override this arrives as text/plain.
  expect(manifest.type).toBe("application/manifest+json");
  expect(manifest.body).toMatchObject({
    name: "Agent Factory",
    short_name: "af",
    start_url: "/",
    scope: "/",
    display: "standalone",
  });
  expect(manifest.body.description).toBeTruthy();
  expect(manifest.body.theme_color).toBeTruthy();
  expect(manifest.body.background_color).toBeTruthy();

  // Every icon the manifest promises must actually be served — Chrome silently drops
  // an install offer over a manifest whose icons 404.
  const icons = await p.evaluate(async () => {
    const res = await fetch("/manifest.webmanifest");
    const m = (await res.json()) as { icons: { src: string; purpose: string }[] };
    return Promise.all(
      m.icons.map(async (i) => ({ src: i.src, purpose: i.purpose, status: (await fetch(i.src)).status })),
    );
  });
  for (const icon of icons) {
    expect(icon.status, `${icon.src} must be served`).toBe(200);
  }
  expect(icons.some((i) => i.purpose === "maskable")).toBe(true);
  await ctx.close();
});

test("the service worker registers, controls the page, and caches the static shell", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);

  // Controlling the page is the precondition for every claim below: an installed
  // worker that never took control could not intercept anything, so proving the
  // bypass against an idle worker would prove nothing.
  expect(await p.evaluate(() => navigator.serviceWorker.controller !== null)).toBe(true);

  // Polled because the cache write rides event.waitUntil — it completes within the
  // fetch event's lifetime but not necessarily before the response reaches the page,
  // so the entry lands shortly after the asset finishes loading.
  await expect
    .poll(async () => (await cacheContents(p)).names.filter((n) => /^af-shell-[0-9a-f]{12}$/.test(n)).length)
    .toBeGreaterThan(0);
  // Two things must be cached, and they warm by different paths: the bundle as a
  // sub-resource (networkFirst), and the shell under /index.html as a NAVIGATION
  // (handleNavigation, warmed by the openControlledByWorker reload). The latter is
  // what a deep-route offline fallback resolves to, so assert it explicitly here.
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/af-web.js"))).toBe(true);
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/index.html"))).toBe(true);
  await ctx.close();
});

test("offline, a client-routed deep link serves the cached app shell, not a browser error", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);
  // The offline fallback resolves to the cached /index.html, so it must be warm before
  // we cut the network — otherwise this would fail for want of a cache rather than for
  // the behaviour under test.
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/index.html"))).toBe(true);

  await ctx.setOffline(true);

  // /tasks is a route the app never fetched or cached by name — serveSPA answers it
  // with the shell for client-side routing. Offline, the worker must do the same, or a
  // refresh/bookmark of a deep route shows the browser's network-error page instead of
  // the app booting and rendering its own daemon-unreachable state. Before the
  // navigation fallback existed, this passed straight through and failed offline.
  const resp = await p.goto("/tasks");
  expect(resp?.status(), "the deep-link navigation must be answered, not a network failure").toBe(200);
  expect(resp?.fromServiceWorker(), "offline, only the worker could have answered — proving the shell fallback").toBe(
    true,
  );
  // It is the app shell that came back (its module bundle tag), not some other cached
  // response. The bundle then boots from cache and takes over the route client-side.
  expect(await p.evaluate(() => !!document.querySelector('script[src="/af-web.js"]'))).toBe(true);

  await ctx.setOffline(false);
  await ctx.close();
});

test("the service worker NEVER caches /v1 — not the API, not the stream (the bypass allowlist)", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);

  // By now the app has made real /v1 traffic on this page: the auth-info probe and
  // the Snapshot fetch both ran to render the rail, and the events WS is open. That
  // matters — a worker that cached indiscriminately would have caught them, so the
  // absence below is a real result rather than an absence of opportunity.
  await expect(p.locator(".af-live-pip.af-live-open")).toBeVisible();
  await p.evaluate(() => fetch("/v1/auth-info"));

  const { urls } = await cacheContents(p);
  const v1 = urls.filter((u) => new URL(u).pathname.startsWith("/v1/"));
  expect(v1, "the worker must never cache anything under /v1").toEqual([]);
  await ctx.close();
});

test("the service worker does not SERVE /v1: offline, the shell survives from cache but /v1 fails", async ({
  browser,
}) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);
  // Warm the shell cache and make the /v1 requests the app makes on any load. The
  // offline assertions below are only meaningful once the shell is actually cached —
  // otherwise "the shell survives offline" could fail for want of a cache rather than
  // for the reason under test.
  await expect(p.locator(".af-live-pip.af-live-open")).toBeVisible();
  await expect.poll(async () => (await cacheContents(p)).urls.some((u) => u.endsWith("/af-web.js"))).toBe(true);

  // Offline is the sharpest discriminator available, and the reason this test exists
  // rather than relying on the cache enumeration alone: with the network gone, a path
  // the worker serves still resolves and a path it merely passes through cannot. So
  // this separates "we didn't happen to cache /v1" from "the worker will never answer
  // for /v1", which is the actual guarantee the PTY stream depends on.
  await ctx.setOffline(true);

  const shellOffline = await p.evaluate(async () => (await caches.match("/af-web.js")) !== undefined);
  expect(shellOffline, "the shell must still be answerable offline — proving the worker does serve it").toBe(true);

  const apiOffline = await p.evaluate(async () => {
    try {
      const res = await fetch("/v1/auth-info");
      return { served: true, status: res.status };
    } catch {
      return { served: false, status: 0 };
    }
  });
  expect(
    apiOffline.served,
    "/v1 resolved with the network offline — the worker is answering for the API, which would put the PTY stream and the whole data plane behind a cache",
  ).toBe(false);

  await ctx.setOffline(false);
  await ctx.close();
});

test("a live PTY stream and the events WS survive the service worker controlling the page", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await openControlledByWorker(p);

  // The functional half of the bypass, and the one that matters to a user: with the
  // worker in control, attach and watch real PTY frames arrive. A worker that
  // intercepted or delayed the stream would show up here as an empty pane.
  await expect(p.locator(".af-live-pip.af-live-open")).toBeVisible();
  await row(p, SESSION_A).click();
  await expect(p.locator(".af-term-host .xterm")).toBeVisible();
  await expect(p.locator(".af-term-host")).toContainText(READY_MARKER);
  await expect(p.locator(".af-term-meta")).toContainText("Live");
  await ctx.close();
});

/** Fires a synthetic beforeinstallprompt and reports whether our handler took it.
 *
 *  Headless Chromium will not fire the real event, so the flow is driven with a
 *  stand-in carrying the only surface install.ts touches: preventDefault, prompt(),
 *  and userChoice. That still exercises the real listener, the real visibility rule,
 *  and the real DOM — only the browser's own decision to offer is stubbed. */
async function fireInstallOffer(page: Page): Promise<void> {
  await page.evaluate(() => {
    const event = new Event("beforeinstallprompt", { cancelable: true }) as Event & {
      prompt: () => Promise<void>;
      userChoice: Promise<{ outcome: string; platform: string }>;
    };
    const w = window as unknown as { __afPromptCalls: number };
    w.__afPromptCalls = 0;
    event.prompt = () => {
      w.__afPromptCalls++;
      return Promise.resolve();
    };
    event.userChoice = Promise.resolve({ outcome: "accepted", platform: "web" });
    window.dispatchEvent(event);
  });
}

test("the install affordance stays hidden until the browser offers an install", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();

  // Nothing has offered an install, so there is nothing to offer the user. This is a
  // real assertion, not a stubbed one: headless Chromium genuinely does not fire
  // beforeinstallprompt here, which is the same reason the button correctly never
  // appears over plain-HTTP Tailscale — no event, no button (see install.ts).
  await expect(p.locator(".af-install")).toBeHidden();

  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeVisible();
  await expect(p.locator(".af-install__go")).toHaveText("Install app");
  await ctx.close();
});

test("the install button prompts once and then retires the spent offer", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);

  await p.locator(".af-install__go").click();
  // The stashed event's prompt() ran — the browser's install dialog was asked for.
  await expect
    .poll(() => p.evaluate(() => (window as unknown as { __afPromptCalls: number }).__afPromptCalls))
    .toBe(1);
  // A beforeinstallprompt event is single-use: prompt() throws if called twice. The
  // affordance retires with the offer it was showing, so there is no second click to
  // make.
  await expect(p.locator(".af-install")).toBeHidden();
  await ctx.close();
});

test("dismissing the install affordance sticks across reloads — it must never nag", async ({ browser }) => {
  const ctx = await browser.newContext();
  const p = await ctx.newPage();
  await p.goto("/");
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeVisible();

  await p.locator(".af-install__dismiss").click();
  await expect(p.locator(".af-install")).toBeHidden();

  // The whole point of persisting the dismissal: a reload that gets a FRESH offer
  // must still stay quiet. Without the localStorage flag this is exactly where the
  // button would come back and start nagging.
  await p.reload();
  await expect(p.locator(".af-app")).toBeVisible();
  await fireInstallOffer(p);
  await expect(p.locator(".af-install")).toBeHidden();
  await ctx.close();
});

test("vscode tab (feat): the ▾ menu creates a VS Code tab and the daemon serves it through the proxy", async () => {
  // End to end, with no seeded fixture: pick VS Code from the tab bar's kind menu,
  // and the daemon spawns a code-server (the FAKE one on PATH — no CI box has a
  // real one) on a 0600 unix socket it names, rooted at THIS session's worktree,
  // and reverse-proxies it into the pane. Nothing has spawned before this click:
  // the editor starts lazily on the first render.
  //
  // It runs near the end of the file on purpose: it leaves a tab on probe-a, and
  // the earlier tabs test asserts that session's exact tab count.
  await row(page, SESSION_A).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  const tabbar = page.locator(".af-tabbar");
  // + still creates a terminal in one click; the kind picker is the ▾ beside it.
  await expect(page.locator(".af-tab-new")).toHaveCount(1);
  const caret = page.locator(".af-tab-new-kind");
  await expect(caret).toHaveCount(1);
  await expect(page.locator(".af-tab-menu")).toBeHidden();

  await caret.click();
  const menu = page.locator(".af-tab-menu");
  await expect(menu).toBeVisible();
  await expect(menu.locator(".af-tab-menu-item")).toHaveText(["Terminal", "VS Code"]);

  await menu.locator(".af-tab-menu-item", { hasText: "VS Code" }).click();
  await expect(menu).toBeHidden();

  const editorTab = tabbar.locator(".af-tab", { hasText: "vscode" });
  await expect(editorTab).toHaveCount(1, { timeout: 30_000 });

  // Creating the tab must NOT strand the keyboard in terminal mode. createSessionTab
  // points the focused pane at the new tab and then attaches it — but a VS Code pane
  // is an iframe with no xterm, so there is nothing to attach to. Before the
  // SplitView.focus() boolean contract, focusTerminal() committed the app to
  // focus:"terminal" anyway and the xterm focus silently no-opped, leaving nav.ts
  // resolving every non-Escape key to {kind:"none"}: j/k/digits/t/w reached neither a
  // terminal nor the rail handler, so the user was stuck until they pressed Escape.
  // (The same trap predates VS Code — it applies to any web tab opened from the tab
  // bar — which is why the fallback lives in focusTerminal rather than this path.)
  await expect(
    page.locator(".af-app.af-kb-rail"),
    "creating a tab with no terminal must fall back to rail mode, not claim terminal mode",
  ).toBeVisible();

  // The real proof is that rail keys still DO something: j moves the selection.
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);
  await page.keyboard.press("j");
  await expect(
    row(page, SESSION_A),
    "j must still navigate the rail right after creating a VS Code tab",
  ).not.toHaveClass(/af-row-selected/);
  await expect(page.locator(".af-rail-list .af-row.af-row-selected")).toHaveCount(1);
  // k back to A so the editor assertions below run against this session.
  await page.keyboard.press("k");
  await expect(row(page, SESSION_A)).toHaveClass(/af-row-selected/);

  await editorTab.click();

  const frame = page.locator(".af-term-host .af-pane-host iframe.af-webframe");
  await expect(frame).toHaveCount(1, { timeout: 15_000 });
  // A vscode tab is ALWAYS daemon-proxied: its code-server listens on a unix socket
  // no browser can address, so the proxy path is the only route to it (#1873).
  await expect(frame).toHaveAttribute("src", /\/v1\/webtab\//);
  // It is an editor pane, not a terminal.
  await expect(page.locator(".af-term-host .af-pane-host .xterm")).toHaveCount(0);
  await expect(page.locator(".af-webpane-reload")).toHaveCount(1);
  // Unlike a web tab, the editor MUST have a real origin: VS Code needs
  // localStorage/workers, which an opaque origin denies. See mountWebPane.
  await expect(frame).toHaveAttribute("sandbox", /allow-same-origin/);

  // The daemon really spawned it and really relayed its content. A cold start shows
  // the self-refreshing "starting" notice first, so poll.
  const framed = page.frameLocator(".af-webframe");
  await expect(framed.locator("#marker")).toHaveText(VSCODE_MARKER, { timeout: 30_000 });
  // ...rooted at THIS session's worktree, not some other directory.
  await expect(framed.locator("#folder")).toContainText(SESSION_A, { timeout: 15_000 });

  // Escape closes the menu without creating anything (checked after, so a stray
  // tab from a mis-click can't be mistaken for the one above).
  await caret.click();
  await expect(menu).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
});

