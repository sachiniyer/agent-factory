// The Backlog walkthrough against the REAL seeded daemon (#6284 theme D): add, nest,
// queue, Start now (a real session behind the fake agent), a question arriving from
// that session and being answered in the UI, and done. It runs at 1280 and at 390 px
// and keeps a screenshot of each step. Each run owns its page and its own uniquely
// titled items.

import { expect, test, type APIRequestContext, type Page } from "@playwright/test";

const SESSION_A = process.env.AF_WEB_SESSION_A ?? "probe-a";

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

async function openBacklog(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  const tab = page.locator('.af-viewtab[data-view="backlog"]');
  if (!(await tab.isVisible())) await page.getByRole("button", { name: "More app controls", exact: true }).click();
  await tab.click();
  await expect(page.locator(".af-backlog")).toBeVisible();
}

const row = (page: Page, title: string) => page.locator(".af-backlog-row", { hasText: title });

for (const width of [1280, 390]) {
  test(`walkthrough at ${width}px: add, nest, queue, start, a question answered, done`, async ({ page, request }, info) => {
    test.setTimeout(120_000);
    await page.setViewportSize({ width, height: width === 390 ? 844 : 800 });
    const shot = (name: string) => page.screenshot({ path: info.outputPath(`${width}-${name}.png`) });
    const root = await projectRoot(request);
    const stamp = `${width}-${Date.now().toString(36)}`;
    const epic = `epic-${stamp}`;
    const child = `work-${stamp}`;

    await openBacklog(page);
    await page.locator("input.af-backlog-entry").first().fill(epic);
    await page.keyboard.press("Enter");
    await expect(row(page, epic)).toBeVisible();
    await page.locator("input.af-backlog-entry").first().fill(child);
    await page.keyboard.press("Enter");
    await expect(row(page, child)).toBeVisible();
    await shot("1-added");

    // Nest the second item under the first.
    const epicId = await row(page, epic).getAttribute("data-item-id");
    const childId = Number(await row(page, child).getAttribute("data-item-id"));
    await row(page, child).click();
    const panel = page.locator("aside.af-backlog-panel");
    await expect(panel).toBeVisible();
    await panel.getByText("Move…", { exact: true }).click();
    await panel.getByLabel("Into…").selectOption({ label: `B-${epicId} ${epic}` });
    await expect(row(page, child)).toHaveAttribute("aria-level", "2");
    await shot("2-nested");

    // Queue it.
    await panel.getByRole("button", { name: "Queue", exact: true }).click();
    await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Queued");
    // The action row lines up at this width: buttons that share a row share its top and height.
    const rects = await panel.locator(".af-backlog-actions > button").evaluateAll((els) =>
      els.map((e) => { const r = e.getBoundingClientRect(); return { top: r.top, height: r.height, bottom: r.bottom, right: r.right }; }));
    expect(rects.length).toBeGreaterThanOrEqual(3);
    for (const a of rects) {
      expect(a.right, "no button overflows the viewport").toBeLessThanOrEqual(width);
      for (const b of rects) {
        if (Math.min(a.bottom, b.bottom) - Math.max(a.top, b.top) > 1) {
          expect(Math.abs(a.top - b.top), "same-row buttons share a top").toBeLessThan(1);
          expect(Math.abs(a.height - b.height), "same-row buttons share a height").toBeLessThan(1);
        }
      }
    }
    await shot("3-queued");

    // Start now: a session is created for the item; the row reads Starting, then Working.
    await panel.getByRole("button", { name: "Start now", exact: true }).click();
    await expect(row(page, child).locator(".af-backlog-chip")).toContainText(/Starting|Working/, { timeout: 60_000 });
    await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Working", { timeout: 60_000 });
    // Nor while it is Working or Starting: the daemon refuses an active run.
    await expect(panel.getByRole("button", { name: "Start now", exact: true })).toHaveCount(0);
    await shot("4-started");

    // A question arrives from the item's session (as `af backlog ask` would send it).
    const view = await rpc(request, "GetBacklog", { repo_path: root });
    const item = view.backlog.items.find((i: { id: number }) => i.id === childId);
    const sessionId = item.runs[item.runs.length - 1].session_id as string;
    await rpc(request, "PostBacklogMessage", { repo_path: root, id: childId, kind: "question", text: "Which branch should this target?", session_id: sessionId });
    await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Needs you");
    await expect(page.locator('.af-viewtab[data-view="backlog"] .af-viewtab-badge')).toBeVisible();
    await shot("5-question");

    // Start now is not offered while the item Needs you: the answer is the next step.
    await expect(panel.getByRole("button", { name: "Start now", exact: true })).toHaveCount(0);

    // Answer it in the UI.
    const answer = panel.getByRole("textbox", { name: "Answer" });
    await expect(answer).toBeVisible();
    await answer.fill("Target master");
    await panel.getByRole("button", { name: "Send answer", exact: true }).click();
    await expect(panel.locator(".af-backlog-msg-text", { hasText: "Target master" })).toBeVisible();
    await expect(row(page, child).locator(".af-backlog-chip")).not.toContainText("Needs you");
    await shot("6-answered");

    // Done.
    await panel.getByRole("button", { name: "Mark done", exact: true }).click();
    // Done items leave the list until "Show done" is on. The panel covers the list at
    // phone width, so close it first.
    await panel.getByRole("button", { name: "Close item" }).click();
    await expect(row(page, child)).toHaveCount(0);
    await page.getByRole("button", { name: "Show done", exact: true }).click();
    await expect(row(page, child).locator(".af-backlog-chip")).toContainText("Done");
    await shot("7-done");
  });
}
