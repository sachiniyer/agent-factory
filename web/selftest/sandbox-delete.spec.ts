import { test, expect } from "@playwright/test";
import { copyFile } from "node:fs/promises";

for (const theme of ["light", "dark"] as const) {
  test(`sandbox deletion warns about unpublished work in ${theme}`, async ({ page }, info) => {
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.route("**/v1/Snapshot", async route => {
      const response = await route.fetch();
      const body = await response.json();
      for (const session of body.data.instances) {
        session.backend_type = "docker";
        // Keep the rail fixture’s routing key, but no local ownership provenance.
        session.worktree = { repo_path: session.worktree?.repo_path };
      }
      await route.fulfill({ json: body });
    });
    await page.goto("/");
    await page.getByRole("button", { name: theme === "light" ? "Light" : "Dark", exact: true }).click();
    const row = page.locator(".af-row").first();
    await row.hover();
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: /^Delete session / }).click();
    const modal = page.getByRole("dialog");
    await expect(modal).toContainText("Unpushed commits and uncommitted changes are lost.");
    await expect(modal).toContainText("Archive publishes the branch first.");
    await expect(modal).not.toContainText("commits stay");
    for (const width of [360, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      await expect(modal.getByRole("button", { name: "Delete session", exact: true })).toBeInViewport();
      const name = `sandbox-delete-${width}-${theme}.png`;
      await expect(modal).toHaveScreenshot(name);
      await copyFile(info.snapshotPath(name), info.outputPath(name));
    }
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  });
}
