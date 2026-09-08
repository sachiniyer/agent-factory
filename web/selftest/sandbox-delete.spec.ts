import { test, expect } from "@playwright/test";
import { stopPolledRoutes } from "./polled-route.js";
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
    await stopPolledRoutes(page.context());
  });
}

for (const backend of ["docker", "local"]) {
  test(`archived ${backend} deletion names Restore`, async ({ page }, info) => {
    let root = "";
    const title = process.env.AF_WEB_SESSION_A ?? "probe-a";
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.route("**/v1/Snapshot", async route => {
      const response = await route.fetch();
      const body = await response.json();
      for (const session of body.data.instances) {
        if (session.title !== title) continue;
        root = session.worktree.repo_path;
        session.backend_type = backend;
        session.liveness = 5; // LiveArchived
        session.lifecycle_action = "restore";
        session.can_kill = true;
        session.worktree = { repo_path: session.worktree?.repo_path, branch_created_by_us: false };
      }
      await route.fulfill({ json: body });
    });
    await page.goto("/");
    await expect.poll(() => root).toBeTruthy();
    await page.getByRole("button", { name: "Switch project", exact: true }).click();
    await page.locator(".af-project-item").filter({ has: page.getByText(root, { exact: true }) }).click();
    await page.locator(".af-rail-filter").click();
    await page.locator('.af-filter-item[data-kind="archived"]').click();
    await page.locator(".af-rail-title").click();
    const row = page.locator(".af-row", { hasText: title });
    await expect(row).toBeVisible();
    await row.hover();
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: /^Delete session / }).click();
    const modal = page.getByRole("dialog");
    await expect(modal).toContainText("Restore instead");
    await expect(modal).not.toContainText("Archive publishes");
    await expect(modal).toContainText(backend === "docker" ? "branch stays published" : "archived worktree");
    for (const width of [360, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      await expect(modal.getByRole("button", { name: "Delete session", exact: true })).toBeInViewport();
      await modal.screenshot({ path: info.outputPath(`archived-delete-${backend}-${width}.png`) });
    }
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
    await stopPolledRoutes(page.context());
  });
}
