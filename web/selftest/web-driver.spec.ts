// web-driver-selftest (#1592 Phase 5 PR6) — the acceptance proof for the embedded
// browser web client, the browser analogue of tui-driver-selftest.sh.
//
// It drives a headless Chromium against a REAL af daemon (a throwaway home on a
// loopback TLS listener, brought up by web-selftest-entry.sh) and asserts the core
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
// The name of the task the harness seeds (web-selftest-entry.sh) so the tasks list
// is non-empty on load.
const SEEDED_TASK = process.env.AF_WEB_TASK_NAME ?? "probe-task";
// The marker the seeded fake agent prints on launch (web-selftest-entry.sh), so
// "the terminal shows live output" is a deterministic string assertion.
const READY_MARKER = process.env.AF_WEB_READY_MARKER ?? "AF_SELFTEST_READY";

/** A rail row by its session title. */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
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
  // projection, not a static list.
  await expect(row(page, SESSION_A)).toBeVisible();
  await expect(row(page, SESSION_B)).toBeVisible();
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

test("projects view (#1592 PR8): the seeded repo groups its sessions; a row jumps to it", async () => {
  // Switch to the projects view via its appbar tab (the [ / ] keyboard path is
  // covered by the nav unit tests). The projects pane replaces the sessions body.
  await page.locator('.af-viewtab[data-view="projects"]').click();
  await expect(page.locator('.af-viewtab[data-view="projects"]')).toHaveClass(/af-viewtab-active/);
  const projects = page.locator(".af-projects");
  await expect(projects).toBeVisible();

  // The two seeded sessions live in ONE mock repo, so exactly one project section
  // renders — proof the grouping is derived from the live projection's repo roots
  // (not an invented client id). Its friendly label + full repo path both show.
  const project = projects.locator(".af-project");
  await expect(project).toHaveCount(1);
  await expect(project.locator(".af-project-name")).toContainText("mock-repo");
  await expect(project.locator(".af-project-path")).toContainText("mock-repo");

  // Both seeded sessions are grouped under the project.
  const grouped = project.locator(".af-project-sessions .af-row");
  await expect(grouped.filter({ hasText: SESSION_A })).toHaveCount(1);
  await expect(grouped.filter({ hasText: SESSION_B })).toHaveCount(1);

  // Clicking a project's session row is the jump-to-session affordance: it returns
  // to the sessions view AND attaches that session's terminal.
  await project.locator(".af-project-sessions .af-row", { hasText: SESSION_A }).click();
  await expect(page.locator('.af-viewtab[data-view="sessions"]')).toHaveClass(/af-viewtab-active/);
  await expect(page.locator(".af-main.af-main-term")).toBeVisible();
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

  // Add a cron task via the + Add modal. The project picker auto-selects the only
  // project; a cron task requires a prompt (the daemon rejects an empty one).
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

  // Title is required; the project picker auto-selects the only project (the mock
  // repo the seeded sessions live in). Program is left at "Repo default" (claude →
  // the fake agent). Submit with the modal's Create button.
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

// NOTE on #1675 PR4 (ended PTY → "exited", not a reconnect loop): this is already
// wired end-to-end — the daemon emits a MsgExit control frame on session-end
// (daemon/ws_pty.go, covered by the Go handler tests), and terminal.ts settles to an
// "exited" state + stops reconnecting on it (see onControl's "exit" arm and the
// TerminalStatus="exited" pane header). It is NOT browser-tested here: a real
// mid-stream exit can't be forced without killing the session (which removes the row
// and disposes the terminal before the exit renders), and mocking the per-session WS
// against the loopback TLS daemon proved unreliable in this harness. The Go side is
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
});
