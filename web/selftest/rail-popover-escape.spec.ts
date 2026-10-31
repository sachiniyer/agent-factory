import { test, expect, type Page } from "@playwright/test";

// #6397: the rail's filter menu and the project switcher are disclosures. Escape
// must close the open one and return focus to its button (the model the app
// controls already follow, components.ts), and only one may be open at a time.

async function open(page: Page): Promise<void> {
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.goto("/");
  await page.locator("#app[data-af-resync-settled]").waitFor();
}

test("6397: Escape closes the rail filter menu and returns focus to its button", async ({ page }) => {
  await open(page);
  const funnel = page.locator(".af-rail-filter");
  await funnel.click();
  await expect(page.locator(".af-filter-menu")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-filter-menu"), "Escape dismisses the filter menu").toBeHidden();
  await expect(funnel).toHaveAttribute("aria-expanded", "false");
  await expect(funnel, "focus returns to the funnel").toBeFocused();
});

test("6397: Escape closes the project switcher and returns focus to its button", async ({ page }) => {
  await open(page);
  const switcher = page.locator(".af-project-switch");
  await switcher.click();
  await expect(page.locator(".af-project-menu")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".af-project-menu"), "Escape dismisses the project menu").toBeHidden();
  await expect(switcher).toHaveAttribute("aria-expanded", "false");
  await expect(switcher, "focus returns to the switcher").toBeFocused();
});

test("6397: opening one rail popover closes the other", async ({ page }) => {
  await open(page);
  await page.locator(".af-rail-filter").click();
  await expect(page.locator(".af-filter-menu")).toBeVisible();
  await page.locator(".af-project-switch").click();
  await expect(page.locator(".af-project-menu")).toBeVisible();
  await expect(page.locator(".af-filter-menu"), "the filter menu closes when the project menu opens").toBeHidden();
  await page.locator(".af-rail-filter").click();
  await expect(page.locator(".af-filter-menu")).toBeVisible();
  await expect(page.locator(".af-project-menu"), "the project menu closes when the filter opens").toBeHidden();
});
