// The task switch's pending state (#6284 theme C), against the REAL seeded daemon:
// it shows the requested value and stays busy while UpdateTask is in flight, and a
// refused request puts the old value back. Each test owns its page.

import { expect, test, type Locator, type Page } from "@playwright/test";

const SEEDED_TASK = process.env.AF_WEB_TASK_NAME ?? "probe-task";

/** The task list rebuilds on refetch, so a screenshot can race a detach: retry once. */
async function shot(target: Locator, path: string) {
  for (let attempt = 0; ; attempt++) {
    try { await target.screenshot({ path }); return; } catch (err) { if (attempt > 2) throw err; }
  }
}

async function openTasks(page: Page) {
  await page.goto("/");
  await expect(page.locator(".af-app")).toBeVisible();
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const row = page.locator(".af-tasks .af-task-row", { hasText: SEEDED_TASK });
  await expect(row).toHaveCount(1);
  return row.getByRole("switch");
}

test("task switch: shows the requested value and stays busy while UpdateTask is in flight", async ({ page }, info) => {
  const sw = await openTasks(page);
  const row = page.locator(".af-tasks .af-task-row", { hasText: SEEDED_TASK });
  await shot(row, info.outputPath("switch-before.png"));
  const was = (await sw.getAttribute("aria-checked")) === "true";
  let release!: () => void;
  const gate = new Promise<void>((resolve) => { release = resolve; });
  let updates = 0;
  await page.route("**/v1/UpdateTask", async (route) => {
    updates++;
    await gate;
    await route.continue();
  });

  await sw.click();
  await expect(sw).toHaveAttribute("aria-checked", String(!was));
  await expect(sw).toHaveAttribute("aria-busy", "true");
  await expect(sw).toHaveAttribute("aria-disabled", "true");
  // aria-disabled, not disabled: the switch keeps keyboard focus while busy.
  await expect(sw).not.toHaveAttribute("disabled", /.*/);
  await shot(row, info.outputPath("switch-busy.png"));
  await sw.click({ force: true });
  expect(updates, "a second click while in flight sends nothing").toBe(1);

  release();
  await expect(sw).not.toHaveAttribute("aria-busy", "true");
  await expect(sw).toHaveAttribute("aria-checked", String(!was));
  await shot(row, info.outputPath("switch-settled.png"));
  // The daemon applied it: a fresh load reads the new value.
  await page.unroute("**/v1/UpdateTask");
  await page.reload();
  await page.locator('.af-viewtab[data-view="tasks"]').click();
  const reloaded = page.locator(".af-tasks .af-task-row", { hasText: SEEDED_TASK }).getByRole("switch");
  await expect(reloaded).toHaveAttribute("aria-checked", String(!was));
  // Put the fixture back.
  await reloaded.click();
  await expect(reloaded).toHaveAttribute("aria-checked", String(was));
  await expect(reloaded).not.toHaveAttribute("aria-busy", "true");
});

test("task switch: a refused UpdateTask puts the old value back", async ({ page }) => {
  const sw = await openTasks(page);
  const was = await sw.getAttribute("aria-checked");
  await page.route("**/v1/UpdateTask", (route) => route.fulfill({
    status: 503,
    json: { data: null, error: { message: "The daemon refused this operation. Your input is retained.", daemon_rejected: true } },
  }));
  await sw.click();
  await expect(sw).not.toHaveAttribute("aria-busy", "true");
  await expect(sw).toHaveAttribute("aria-checked", was!);
  await expect(sw).not.toHaveAttribute("aria-disabled", "true");
});
