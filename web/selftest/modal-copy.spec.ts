import { expect, test } from "@playwright/test";
import { Liveness } from "../src/types.js";

for (const theme of ["light", "dark"] as const) {
  test(`session deletion warns about work loss in ${theme}`, async ({ page }, info) => {
    await page.emulateMedia({ colorScheme: theme });
    // Pin this visual fixture to an af-owned worktree; external copy has separate coverage.
    await page.route("**/v1/Snapshot", async route => {
      const response = await route.fetch();
      const body = await response.json();
      for (const session of body.data.instances) {
        if (session.worktree) session.worktree.external_worktree = false;
      }
      await route.fulfill({ json: body });
    });
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.goto("/");
    const row = page.locator(".af-row").first();
    await row.hover();
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: /^Delete session / }).click();
    const modal = page.getByRole("dialog");
    await expect(modal).toContainText("Uncommitted changes and unpushed commits are lost.");
    await expect(modal).toContainText("Archive to keep them.");
    await expect(modal).not.toContainText("User-owned work stays");
    for (const width of [1440, 375]) {
      await page.setViewportSize({ width, height: 900 });
      await expect(modal.getByRole("button", { name: "Delete session", exact: true })).toBeInViewport();
      await page.screenshot({ path: info.outputPath(`delete-${width}-${theme}.png`) });
    }
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  });
}

for (const [archived, tasks] of [[false, false], [true, false], [false, true], [true, true]]) {
  test(`zero-live project copy: archived=${archived}, tasks=${tasks}`, async ({ page }, info) => {
    const root = "/tmp/modal-copy-project";
    const envelope = (data: unknown) => ({ data, error: null });
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({ instances: archived ? [
      { id: "saved", title: "Saved work", branch: "saved", liveness: Liveness.Archived, worktree: { repo_path: root } },
    ] : [] }) }));
    await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: [{ root }] }) }));
    await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: tasks ? [
      { id: "task", name: "Review", project_path: root, prompt: "Review", enabled: true, program: "" },
    ] : [] }) }));
    await page.goto("/");
    await page.locator(".af-project-switch").click();
    await page.locator(".af-project-delete").click();
    const modal = page.getByRole("dialog");
    await expect(modal).toContainText("No live sessions to archive.");
    await expect(modal).not.toContainText("empty project");
    await expect(modal).toContainText("Archived sessions and tasks stay.");
    await expect(modal).toContainText("Tasks keep the project in the switcher; otherwise, add it again to see archives.");
    await page.screenshot({ path: info.outputPath("project-removal.png") });
    await modal.getByRole("button", { name: "Cancel", exact: true }).click();
  });
}
