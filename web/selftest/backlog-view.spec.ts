// The Backlog view against the REAL seeded daemon (#6284 theme D): add, nest, queue
// and note through the UI, and the frame rule from the #6288 addendum: a
// backlog.changed frame names only the ids a write touched, never a parent whose
// derived state moved, so the view re-reads GetBacklog on every frame instead of
// patching from the ids. Each test owns its page and its own uniquely titled items.

import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";

/** The seeded project's repo root, the way the web names a project. */
async function projectRoot(request: APIRequestContext): Promise<string> {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const a = snapshot.data.instances.find((s: { title: string }) => s.title === SESSION_A);
  expect(a?.worktree?.repo_path).toBeTruthy();
  return a.worktree.repo_path as string;
}

async function rpc(request: APIRequestContext, name: string, data: Record<string, unknown>) {
  const resp = await (await request.post(`/v1/${name}`, { data })).json();
  expect(resp.error, `${name}: ${JSON.stringify(resp.error)}`).toBeNull();
  return resp.data;
}

async function add(request: APIRequestContext, root: string, title: string, parent?: number): Promise<number> {
  const resp = await (await request.post("/v1/AddBacklogItems", {
    data: { repo_path: root, items: [{ key: "k", title, parent }] },
  })).json();
  expect(resp.error).toBeNull();
  return resp.data.ids.k as number;
}

async function openBacklog(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  const tab = page.locator('.af-viewtab[data-view="backlog"]');
  if (!(await tab.isVisible())) await page.getByRole("button", { name: "More app controls", exact: true }).click();
  await tab.click();
  await expect(page.locator(".af-backlog")).toBeVisible();
}

const row = (page: Page, title: string) => page.locator(".af-backlog-row", { hasText: title });

test("add, nest, queue and note an item through the UI", async ({ page }, info) => {
  const stamp = Date.now().toString(36);
  const epic = `epic-${stamp}`;
  const child = `child-${stamp}`;
  await openBacklog(page);
  await page.locator("input.af-backlog-entry").first().fill(epic);
  await page.keyboard.press("Enter");
  await expect(row(page, epic)).toBeVisible();
  await page.locator("input.af-backlog-entry").first().fill(child);
  await page.keyboard.press("Enter");
  await expect(row(page, child)).toBeVisible();
  await page.screenshot({ path: info.outputPath("added.png") });

  // Nest: Move… > Into… the epic.
  const epicId = await row(page, epic).getAttribute("data-item-id");
  await row(page, child).click();
  const panel = page.locator("aside.af-backlog-panel");
  await expect(panel).toBeVisible();
  await panel.getByText("Move…", { exact: true }).click();
  await panel.getByLabel("Into…").selectOption({ label: `B-${epicId} ${epic}` });
  await expect(row(page, child)).toHaveAttribute("aria-level", "2");
  await page.screenshot({ path: info.outputPath("nested.png") });

  // Queue the child: it reads Queued, and the parent's roll-up follows without being asked.
  await panel.getByRole("button", { name: "Queue", exact: true }).click();
  await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Queued");
  await expect(row(page, epic).locator(".af-backlog-chip")).toContainText("Queued");

  // A note lands on the thread.
  await panel.getByRole("textbox", { name: "Note", exact: true }).fill("a note from the walkthrough");
  await panel.getByRole("button", { name: "Add note", exact: true }).click();
  await expect(panel.locator(".af-backlog-msg-text", { hasText: "a note from the walkthrough" })).toBeVisible();
  await page.screenshot({ path: info.outputPath("queued-noted.png") });
});

test("a backlog.changed frame is a re-read: a child queued elsewhere moves the parent that no frame names", async ({ page, request }) => {
  const root = await projectRoot(request);
  const stamp = Date.now().toString(36);
  const epic = `frame-epic-${stamp}`;
  const child = `frame-child-${stamp}`;
  const epicId = await add(request, root, epic);
  const childId = await add(request, root, child, epicId);
  await openBacklog(page);
  await expect(row(page, epic)).toBeVisible();
  await expect(row(page, epic).locator(".af-backlog-chip")).toContainText("Draft");

  // Frames name ids [childId] only; the epic's state must still move.
  const frames: string[] = [];
  await page.routeWebSocket("**/v1/events*", (socket) => {
    const server = socket.connectToServer();
    server.onMessage((m) => { if (typeof m === "string" && m.includes("backlog.changed")) frames.push(m); socket.send(m); });
    socket.onMessage((m) => server.send(m));
  });
  await page.reload();
  await page.locator('.af-viewtab[data-view="backlog"]').click();
  await expect(row(page, epic)).toBeVisible();
  const queued = await (await request.post("/v1/SetBacklogItemStatus", {
    data: { repo_path: root, id: childId, status: "queued" },
  })).json();
  expect(queued.error).toBeNull();
  await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Queued");
  await expect(row(page, epic).locator(".af-backlog-chip")).toContainText("Queued");
  expect(frames.join("")).toContain(`"ids":[${childId}]`);
  expect(frames.join(""), "the frame did not name the parent").not.toContain(`${epicId},`);

  // A new item added elsewhere appears with no reload (the frame triggers the re-read).
  const late = `late-${stamp}`;
  await add(request, root, late);
  await expect(row(page, late)).toBeVisible();
});

test("at 390 px an item's action row fits and its primary and ghost buttons line up", async ({ page, request }, info) => {
  await page.setViewportSize({ width: 390, height: 844 });
  const root = await projectRoot(request);
  const title = `phone-${Date.now().toString(36)}`;
  await add(request, root, title);
  // A draft item offers Queue, the primary action, beside ghost buttons.
  await openBacklog(page);
  await row(page, title).click();
  const panel = page.locator("aside.af-backlog-panel");
  await expect(panel).toBeVisible();
  const rects = await panel.locator(".af-backlog-actions > button").evaluateAll((els) =>
    els.map((e) => { const r = e.getBoundingClientRect(); return { top: r.top, height: r.height, bottom: r.bottom, right: r.right }; }));
  expect(rects.length).toBeGreaterThanOrEqual(3);
  for (const a of rects) {
    expect(a.right, "no button overflows the viewport").toBeLessThanOrEqual(390);
    for (const b of rects) {
      if (Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1) {
        expect(Math.abs(a.top - b.top), "same-row buttons share a top").toBeLessThan(1);
        expect(Math.abs(a.height - b.height), "same-row buttons share a height").toBeLessThan(1);
      }
    }
  }
  await page.screenshot({ path: info.outputPath("phone-actions.png") });
});
