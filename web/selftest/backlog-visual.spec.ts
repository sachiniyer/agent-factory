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
      item(2, "Runner bar", {
        parent: 1, status: "running", body: "State, slots and plan use.",
        runs: [{ session_id: "s2", title: "B-2 Runner bar", state: "started", prev_status: "queued", started_by: "runner", started_at: AT }],
      }),
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
    1: { state: "working" },
    2: { state: "working" },
    3: { state: "waiting", reason: "Waiting on B-2" },
    4: { state: "needs_you", reason: "Question from the agent" },
    5: { state: "draft" },
    6: { state: "done" },
  },
  runner: { state: "stopped", gate: "share", reason: "Backlog used its 30% of the week · resumes Sat 09:00", used: 1, max: 2 },
};

const BUDGET = {
  budget: { schema_version: 1, share: 30, stop_at: 80, max_per_run: 10, usd_limit: 0, usd_spent: 0 },
  windows: {
    five_hour: { plan: 41, backlog: 12, resets_at: "2000-01-01T04:00:00Z", all_cost: 20 },
    seven_day: { plan: 52, backlog: 31, resets_at: "2000-01-08T09:00:00Z", all_cost: 120 },
  },
  account: "default",
  reading_at: "2000-01-01T00:03:00Z",
  runs: { s2: 1.2 },
  run_window: { s2: 0.5 },
  unmeterable: {},
};

async function serve(page: Page): Promise<void> {
  await page.clock.setFixedTime(new Date("2000-01-01T00:05:00Z"));
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({ instances: [] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: [{ root: ROOT, name: "Backlog visual" }] }) }));
  await page.route("**/v1/GetBacklog", route => route.fulfill({ json: envelope(VIEW) }));
  await page.route("**/v1/GetBudget", route => route.fulfill({ json: envelope(BUDGET) }));
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
    await expect(page.locator(".af-backlog-plan")).toHaveText("Backlog 31% of week 30% · You 21%");
    await page.locator('.af-backlog-row[data-item-id="2"]').click();
    const panel = page.getByRole("complementary", { name: "Item B-2" });
    await expect(panel).toBeVisible();
    await expect(panel.locator(".af-backlog-body-input")).toBeVisible();
    await expect(panel.getByRole("button", { name: "Close item" })).toBeFocused();
    // The run's session is looked up before it is called Gone; the still waits for the answer.
    await expect(panel.locator(".af-backlog-card-head")).not.toContainText("Checking…");
    // The run's plan use arrives with the budget read, after the first paint.
    await expect(panel.locator(".af-backlog-card-head")).toContainText("$1.20 at API prices");
    await still(page, info, "backlog-desktop.png");
    await page.keyboard.press("Escape");
    await expect(panel).toBeHidden();
    await expect(page.locator('.af-backlog-row[data-item-id="2"]')).toBeFocused();
  });

  test("desktop runner and budget settings", async ({ page }, info) => {
    await page.setViewportSize({ width: 1280, height: 800 });
    await serve(page);
    await openBacklog(page);
    await page.getByRole("button", { name: "Change share" }).click();
    await expect(page.getByRole("spinbutton", { name: "Backlog share" })).toBeFocused();
    await expect(page.getByRole("spinbutton", { name: "Backlog share" })).toHaveValue("30");
    await still(page, info, "backlog-settings.png");
  });

  test("phone list and full-screen item panel", async ({ page }, info) => {
    await page.setViewportSize({ width: 390, height: 844 });
    await serve(page);
    await openBacklog(page);
    await page.locator(".af-backlog-row").first().blur();
    const plan = page.locator(".af-backlog-plan");
    const planBox = await plan.boundingBox();
    expect((planBox?.x ?? 0) + (planBox?.width ?? 0)).toBeLessThanOrEqual(390);
    expect(await page.evaluate(() => document.documentElement.scrollWidth)).toBeLessThanOrEqual(390);
    await plan.click();
    await expect(page.locator(".af-backlog-plan-detail")).toContainText("read 2 min ago · all projects");
    await still(page, info, "backlog-390.png");
    await page.locator('.af-backlog-row[data-item-id="4"]').click();
    const panel = page.getByRole("complementary", { name: "Item B-4" });
    await expect(panel.locator(".af-backlog-body-input")).toBeVisible();
    await expect(panel.getByRole("textbox", { name: "Answer" })).toHaveAttribute("placeholder", "Answer B-4…");
    await panel.getByRole("textbox", { name: "Answer" }).fill("Keep 50%");
    const box = await panel.boundingBox();
    expect(box?.x).toBe(0);
    expect(box?.width).toBe(390);
    await still(page, info, "backlog-390-panel.png");
  });
});
