// web-driver-selftest (#1592 Phase 5 PR6) — the acceptance proof for the embedded
// browser web client, the browser analogue of tui-driver-selftest.sh.
//
// It drives a headless Chromium against a REAL af daemon (a throwaway home on a
// loopback TLS+token listener, brought up by web-selftest-entry.sh) and asserts
// the core v1 loop end to end — assertions are the gate, not screenshots:
//
//   1. login          paste the daemon token → the authed app renders
//   2. sidebar         the rail lists the sessions from the Snapshot/events plane
//   3. attach          click-to-attach opens the xterm terminal + shows live output
//   4. keyboard (#1694) j/k navigate the rail, Enter attaches, Escape returns to rail
//   5. create          the + New modal creates a session; its row appears
//   6. kill            the kill confirm removes the session's row
//   7. archive         the archive confirm moves a session to the archived group
//
// Everything the test needs is handed in via env by the entry script (see
// playwright.config.ts): AF_WEB_BASE_URL, AF_WEB_TOKEN, and the two seeded
// session titles AF_WEB_SESSION_A / AF_WEB_SESSION_B.

import { expect, type Locator, type Page, test } from "@playwright/test";

const TOKEN = requireEnv("AF_WEB_TOKEN");
const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";
const SESSION_B = process.env.AF_WEB_SESSION_B ?? "probe-b";
// The marker the seeded fake agent prints on launch (web-selftest-entry.sh), so
// "the terminal shows live output" is a deterministic string assertion.
const READY_MARKER = process.env.AF_WEB_READY_MARKER ?? "AF_SELFTEST_READY";

function requireEnv(name: string): string {
  const v = process.env[name];
  if (!v) {
    throw new Error(`${name} is unset — run via \`make web-selftest-container\`, which boots the daemon.`);
  }
  return v;
}

/** A rail row by its session title. */
function row(page: Page, title: string): Locator {
  return page.locator(".af-rail-list .af-row", { hasText: title });
}

/** Logs in by pasting the daemon token and waits for the authed shell + the rail
 *  to be populated by the Snapshot the probe returns. */
async function login(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator("#af-token")).toBeVisible();
  await page.locator("#af-token").fill(TOKEN);
  await page.locator("form.af-login-form button.af-primary").click();
  await expect(page.locator(".af-app")).toBeVisible();
}

// The flows share one daemon and mutate its session set (create/kill/archive), so
// they must run in order against a single page.
test.describe.configure({ mode: "serial" });

let page: Page;
// The title of the session the create flow makes, handed to the kill flow.
let createdTitle = "";

test.beforeAll(async ({ browser }) => {
  page = await browser.newPage();
  await login(page);
});

test.afterAll(async () => {
  await page.close();
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

test("create: the + New modal creates a session and its row appears", async () => {
  const created = `probe-created-${Date.now().toString(36)}`;

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
