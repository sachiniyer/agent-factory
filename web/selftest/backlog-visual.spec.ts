// The Backlog view's stills at desktop and phone width, with the backlog served
// at the transport edge so the scene does not depend on the daemon's state.
import { writeFile } from "node:fs/promises";
import { test, expect, type Page, type TestInfo } from "@playwright/test";

const envelope = (data: unknown) => ({ data, error: null });
const ROOT = "/tmp/backlog-visual";
const AT = "2000-01-01T00:00:00Z";

const item = (id: number, title: string, over: Record<string, unknown> = {}) => ({
  id, title, status: "draft", created_at: AT, updated_at: AT, ...over,
});

const VIEW = {
  backlog: {
    schema_version: 1,
    next_id: 7,
    settings: { runner: true, max_running: 2, on_done: "keep" },
    items: [
      item(1, "Ship the backlog view", { status: "queued", sequential: true }),
      item(2, "Runner bar", { parent: 1, status: "queued", body: "Read-only: state, slots and budget." }),
      item(3, "Item panel", { parent: 1, status: "queued", waits_on: [2] }),
      item(4, "Pick a default budget", {
        status: "queued",
        thread: [{ id: 1, kind: "question", author: "agent:s1", text: "Should the share default to 30%?", at: AT }],
      }),
      item(5, "Phone layout"),
      item(6, "Old spike", { status: "done" }),
    ],
  },
  states: {
    1: { state: "queued" },
    2: { state: "queued" },
    3: { state: "waiting", reason: "Waiting on B-2" },
    4: { state: "needs_you", reason: "Question from the agent" },
    5: { state: "draft" },
    6: { state: "done" },
  },
  runner: { state: "idle", reason: "Nothing queued is ready.", used: 0, max: 2 },
};

async function serve(page: Page): Promise<void> {
  await page.clock.setFixedTime(new Date("2000-01-01T00:05:00Z"));
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({ instances: [] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: [{ root: ROOT, name: "Backlog visual" }] }) }));
  await page.route("**/v1/GetBacklog", route => route.fulfill({ json: envelope(VIEW) }));
  await page.route("**/v1/GetBudget", route => route.fulfill({ json: envelope({
    budget: { schema_version: 1, share: 30, stop_at: 80, max_per_run: 10, usd_limit: 0, usd_spent: 0 },
  }) }));
}

async function openBacklog(page: Page): Promise<void> {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  const tab = page.locator('.af-viewtab[data-view="backlog"]');
  if (!await tab.isVisible()) await page.getByRole("button", { name: "More app controls", exact: true }).click();
  await tab.click();
  await expect(page.locator(".af-backlog-row")).toHaveCount(5);
}

async function still(page: Page, info: TestInfo, name: string): Promise<void> {
  await page.evaluate(() => document.fonts.ready);
  const pixels = await page.screenshot({ animations: "disabled", caret: "hide" });
  await writeFile(info.outputPath(`golden-${name}`), pixels);
  expect(pixels).toMatchSnapshot(name);
}

test.describe("backlog view stills", () => {
  test("desktop list and item panel", async ({ page }, info) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await serve(page);
    await openBacklog(page);
    await page.locator('.af-backlog-row[data-item-id="3"]').click();
    const panel = page.getByRole("complementary", { name: "Item B-3" });
    await expect(panel).toBeVisible();
    await expect(panel.locator(".af-backlog-body-input")).toBeVisible();
    await expect(panel.getByRole("button", { name: "Close item" })).toBeFocused();
    await still(page, info, "backlog-desktop.png");
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(page.locator('.af-backlog-row[data-item-id="3"]')).toBeFocused();
  });

  test("phone list and full-screen item panel", async ({ page }, info) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await serve(page);
    await openBacklog(page);
    await page.locator(".af-backlog-row").first().blur();
    await still(page, info, "backlog-390.png");
    await page.locator('.af-backlog-row[data-item-id="4"]').click();
    const panel = page.getByRole("complementary", { name: "Item B-4" });
    await expect(panel.locator(".af-backlog-body-input")).toBeVisible();
    const box = await panel.boundingBox();
    expect(box?.x).toBe(0);
    expect(box?.width).toBe(390);
    await still(page, info, "backlog-390-panel.png");
  });
});
