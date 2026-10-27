import { test, expect } from "@playwright/test";

/** A project switcher menu item by its EXACT repo basename (redesign PR2). Filters on
 *  the name span with an anchored regex so "mock-repo" never also matches
 *  "mock-repo-2" / "mock-repo-3" (they share the prefix). Mirrors the helper in
 *  web-driver.spec.ts so this file stays self-contained. */
function projectItem(page: import("@playwright/test").Page, name: string): import("@playwright/test").Locator {
  return page
    .locator(".af-project-menu .af-project-item")
    .filter({ has: page.locator(".af-project-item-name", { hasText: new RegExp(`^${name}$`) }) });
}

// The shared modalChrome focus-containment gate (#fix): the immediate-restore
// confirmModal variant in modals.ts was the only modal that moved focus off a
// footer button before disabling it and refocused inside on error, so it was the
// only one whose Tab trap held during busy/error. Every other modal — including
// delete-project, remove-task, add/edit-task, mark-delivered, handoff, and the
// add/rebind project picker — rides the shared chrome, which disabled a focused
// footer button and dropped document.activeElement to <body>, escaping the
// card-level Tab trap (a body-targeted keydown never bubbles through the card),
// so Tab free-walked the page behind the overlay and Enter could activate a
// background rail Kill button while the user believed they were inside the modal.
//
// delete-project is the cleanest shared-chrome probe: it is confirm-only (no
// inputs, no directory picker), opens from the project switcher on a project that
// already exists, and the delete RPC can be held then refused WITHOUT mutating
// shared state (a refusal leaves the project intact, so later selftests still see
// it). Clicking the confirm button focuses it in Chromium — the most common
// everyday state the report names — so setBusy(true) disabling it is the exact
// path that used to escape focus to <body>.
test("shared chrome owns keyboard focus through a busy then refused delete-project", async ({ page, request }) => {
  // Seed sanity: the snapshot reachable, mock-repo present (the default project).
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  assertHasMockRepo(snapshot);
  await page.routeWebSocket("**/v1/events*", () => {});

  // Hold the delete RPC so the modal sits in its busy state, then refuse it so the
  // modal stays open with an error — the two states focus used to escape in.
  let release!: () => void;
  const held = new Promise<void>(resolve => { release = resolve; });
  let deletes = 0;
  await page.route("**/v1/DeleteProject", async route => {
    deletes++;
    await held;
    await route.fulfill({ json: { data: null, error: { message: "delete refused for focus test", daemon_rejected: true } } });
  });

  await page.goto(`/#/`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // Select mock-repo explicitly — order-independent across the serial selftest run.
  await page.locator(".af-project-switch").click();
  await projectItem(page, "mock-repo").click();
  await expect(page.locator(".af-project-switch-name")).toHaveText("mock-repo");

  // Open the delete-project confirm modal (shared chrome, confirm-only).
  await page.locator(".af-project-switch").click();
  const del = page.locator(".af-project-menu .af-project-delete");
  await expect(del).toBeEnabled();
  await del.click();
  const dialog = page.locator(".af-modal-card");
  await expect(dialog).toBeVisible();

  // Click the confirm button. Chromium focuses it on click; the form's submit
  // handler then runs setBusy(true) synchronously, disabling both footer buttons
  // while the (held) RPC is in flight.
  const confirm = dialog.getByRole("button", { name: "Delete project", exact: true });
  const inDialog = () => page.evaluate(() => !!document.activeElement?.closest(".af-modal-card"));
  const onBody = () => page.evaluate(() => document.activeElement === document.body);
  await confirm.click();
  await expect.poll(() => deletes).toBe(1);
  await expect(dialog).toHaveClass(/af-modal-busy/);

  // During busy: focus must be INSIDE the dialog (the fix moved it off the
  // now-disabled confirm button to the card), never on <body>.
  await expect.poll(inDialog).toBe(true);
  await expect.poll(onBody).toBe(false);
  // Tab cycles within the dialog and never reaches the page behind the overlay.
  for (let i = 0; i < 4; i++) {
    await page.keyboard.press("Tab");
    await expect.poll(inDialog).toBe(true);
    await expect.poll(onBody).toBe(false);
  }

  // Release the refusal. The modal stays open with an inline error (the
  // refused-attempt path that previously left focus on <body> indefinitely).
  release();
  await expect(dialog.locator(".af-modal-error")).toBeVisible();
  await expect(dialog).toContainText("delete refused for focus test");
  await expect(dialog).toBeVisible();

  // After the error: focus is still inside the dialog.
  await expect.poll(inDialog).toBe(true);
  await expect.poll(onBody).toBe(false);
  for (let i = 0; i < 4; i++) {
    await page.keyboard.press("Tab");
    await expect.poll(inDialog).toBe(true);
    await expect.poll(onBody).toBe(false);
  }

  // Escape closes the modal (the global document-capture handler still owns it).
  await page.keyboard.press("Escape");
  await expect(dialog).toBeHidden();
  // mock-repo is still there — the refusal mutated nothing.
  await page.locator(".af-project-switch").click();
  await expect(projectItem(page, "mock-repo")).toBeVisible();
});

function assertHasMockRepo(snapshot: { data?: { instances?: { title: string }[] } }): void {
  const titles = snapshot.data?.instances?.map((s) => s.title) ?? [];
  expect(titles.length, "the snapshot must seed sessions").toBeGreaterThan(0);
}
