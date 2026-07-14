// web-driver-selftest (#1592 Phase 5 PR6) — the acceptance proof for the embedded
// browser web client, the browser analogue of tui-driver-selftest.sh.
//
// It drives a headless Chromium against a REAL af daemon (a throwaway home on a
// loopback plain-HTTP listener, brought up by web-selftest-entry.sh) and asserts the core
// v1 loop end to end — assertions are the gate, not screenshots:
//
//   1. tokenless open  loopback ⇒ no token required (#1696): the SPA auto-connects
//                      with NO credential and NEVER shows the paste-token login
//   2. sidebar         the rail lists the sessions from the Snapshot/events plane
//   3. attach          click-to-attach opens the xterm terminal + shows live output
//   4. keyboard (#1694) j/k navigate the rail, Enter attaches, Escape returns to rail
//   5. create          the + New modal creates a session; its row appears
//   6. kill            the kill confirm removes the session's row
//   7. archive         the archive confirm moves a session to the archived group
//
// The daemon binds 127.0.0.1, so under #1696 the browser is a LOOPBACK peer and the
// daemon exempts it from the bearer token — the SPA's /v1/auth-info probe reports
// auth_required=false, the login screen is skipped, and every core action
// (create/kill/archive/send-prompt/attach) runs on the empty-token credential. That
// makes this harness the end-to-end regression guard that tokenless authorization
// works for ALL actions, not just the read path. (The token-PASTE UI path is not
// reachable here — a loopback container is always exempt — so it stays covered by
// the Go handler tests: daemon/httpauth_test.go network-peer → 401 + spoof-resistance.)
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
const WEBTAB_LOCAL_MARKER = process.env.AF_WEBTAB_LOCAL_MARKER ?? "AF_WEBTAB_LOCAL_OK";
const WEBTAB_EXTERNAL_URL = process.env.AF_WEBTAB_EXTERNAL_URL ?? "https://blocked.example.test/";

/** A rail row by its session title. */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
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

// The flows share one daemon and mutate its session set (create/kill/archive), so
// they must run in order against a single page.
test.describe.configure({ mode: "serial" });

let page: Page;
// The title of the session the create flow makes, handed to the kill flow.
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

test("the #1694 keyboard model: [ / ] cycle the top-level view (sessions ⇄ tasks)", async () => {
  // Rail mode from the previous flow. [ / ] cycle the top-level view; they fire in
  // rail mode only (a modal or focused terminal would swallow them). After Escape
  // the active element is document.body, so the document-level capture-phase keydown
  // listener (index.ts) handles the press. The Projects view is gone (redesign PR2):
  // the cycle is just sessions ⇄ tasks now.
  const active = (view: string) =>
    expect(page.locator(`.af-viewtab[data-view="${view}"]`)).toHaveClass(/af-viewtab-active/);
  await active("sessions");
  // ] advances sessions -> tasks; ] again wraps tasks -> sessions.
  await page.keyboard.press("]");
  await active("tasks");
  await page.keyboard.press("]");
  await active("sessions");
  // [ steps the other way, wrapping sessions -> tasks, then back to the start view so
  // the following rail-driven flows still see the sessions rail.
  await page.keyboard.press("[");
  await active("tasks");
  await page.keyboard.press("[");
  await active("sessions");
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
        body: JSON.stringify({ data: null, error: "simulated tab-close failure" }),
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
  // The CLI/API refuse to create a URL-less web tab, so this malformed/older-record
  // case is injected by rewriting the Snapshot to append a web tab (kind 3) with an
  // empty url to the web-tab session. The pane must render the fallback, not blank.
  await page.route("**/v1/Snapshot", async (route) => {
    const resp = await route.fetch();
    const body = await resp.json();
    // The Snapshot envelope is { data: { instances: SessionData[] } }.
    const snap = body?.data as { instances?: Array<{ title: string; tabs?: Array<{ name: string; kind: number; url?: string }> }> };
    const web = snap?.instances?.find((s) => s.title === SESSION_WEB);
    if (web) {
      web.tabs = web.tabs ?? [];
      web.tabs.push({ name: "nourl", kind: 3, url: "" });
    }
    // Fulfill with a freshly-serialized body — the fetched APIResponse has already
    // been consumed by .json(), so it can't be reused as `response`.
    await route.fulfill({ status: resp.status(), contentType: "application/json", body: JSON.stringify(body) });
  });
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
  await expect(row(page, SESSION_WEB)).toBeVisible({ timeout: 15_000 });

  await row(page, SESSION_WEB).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
  const tabbar = page.locator(".af-tabbar");
  const nourlTab = tabbar.locator(".af-tab", { hasText: "nourl" });
  await expect(nourlTab).toHaveCount(1, { timeout: 15_000 });
  await nourlTab.click();

  // A clean fallback (no broken iframe), not a blank pane.
  const fallback = page.locator(".af-term-host .af-pane-host .af-webpane-fallback");
  await expect(fallback).toBeVisible({ timeout: 10_000 });
  await expect(fallback).toContainText("no URL");

  await page.unroute("**/v1/Snapshot");
  await page.reload();
  await expect(page.locator(".af-app")).toBeVisible();
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

  // Its rail is the clean empty state (no sessions), not a blank rail.
  const empty = page.locator(".af-rail-empty-project");
  await expect(empty).toContainText("No sessions in");
  await expect(empty).toContainText("mock-repo-3");
  await expect(empty.locator(".af-rail-empty-new")).toBeVisible();

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

test("archive: the archive confirm moves a session to the archived group", async () => {
  // Archive session B. Select it (click attaches, which is fine), then archive +
  // confirm.
  await row(page, SESSION_B).click();
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();

  // The Archive action is the ghost button labeled "Archive" in the pane header.
  await page.locator(".af-term-head button", { hasText: "Archive" }).click();
  const modal = page.locator(".af-modal-card");
  await expect(modal).toBeVisible();
  await modal.locator("button.af-primary").click();

  // B is not killed — it stays in the rail, but in the archived group (rendered
  // with the af-row-archived modifier and sorted last).
  await expect(row(page, SESSION_B)).toHaveClass(/af-row-archived/, { timeout: 30_000 });
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
