// P3 feedback is observable while the RPC is deliberately still unanswered.
// No elapsed-time sleeps and no recorder changes: these assert state transitions.
import { test, expect } from "@playwright/test";

for (const operation of ["create", "archive", "kill"] as const) {
  test(`optimistic ${operation} renders before RPC, then reverts with retained failure`, async ({ page }) => {
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.goto("/");
    await expect(page.locator(".af-app")).toBeVisible();
    const method = { create: "CreateSession", archive: "ArchiveSession", kill: "KillSession" }[operation];
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    await page.route(`**/v1/${method}`, async route => {
      await pending;
      await route.fulfill({ status: 503, json: {
        data: null, error: { message: "The daemon refused this operation. Your input is retained." },
      } });
    });
    const requested = page.waitForRequest(`**/v1/${method}`);
    let title = "Optimistic retained draft";
    if (operation === "create") {
      await page.locator(".af-rail-new").click();
      await page.getByRole("textbox", { name: "Session title", exact: true }).fill(title);
    } else {
      const row = page.locator(".af-row").first();
      title = await row.locator(".af-row-title").innerText();
      await row.hover();
      await row.getByRole("button", { name: /^Actions for / }).click();
      await row.getByRole("button", { name: new RegExp(`^${operation === "kill" ? "Kill" : "Archive"} session`) }).click();
    }
    await page.locator(".af-modal-card button[type=submit]").click();
    await requested;
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
    if (operation === "create") {
      await expect(page.locator(".af-row-creating").filter({ hasText: title })).toBeVisible();
    } else {
      await expect(page.locator(".af-row-title").filter({ hasText: `[deleting] ${title}` })).toBeVisible();
    }
    release();
    await expect(page.locator(".af-modal-error")).toContainText("The daemon refused this operation.");
    if (operation === "create") {
      await expect(page.locator(".af-row-creating").filter({ hasText: title })).toHaveCount(0);
      await expect(page.getByRole("textbox", { name: "Session title", exact: true })).toHaveValue(title);
    } else {
      await expect(page.locator(".af-row-title").filter({ hasText: `[deleting] ${title}` })).toHaveCount(0);
      await expect(page.locator(".af-row-title").filter({ hasText: title })).toBeVisible();
    }
  });
}
