import { expect, test } from "@playwright/test";
import { Liveness } from "../src/types.js";

// #7201: the delete-project confirmation states its session counts as the terms
// of a destructive action, so it never states one the session projection did not
// observe. While the projection is stale (the events stream is down, or its
// re-read after opening has not landed) or incomplete (the daemon skipped a
// repo), the dialog reads "Could not check live sessions" and its Delete project
// button is disabled; it restates the real counts once they are current.

const root = "/tmp/project-delete-unchecked";
const envelope = (data: unknown) => ({ data, error: null });
const liveSession = {
  id: "live", title: "Live work", branch: "live", liveness: Liveness.Ready, worktree: { repo_path: root },
};

test("project delete states no counts until the session list is re-read, then the real ones (#7201)", async ({ page }) => {
  // The events socket opens (mocked), and every Snapshot after the login probe —
  // the re-read that open triggers — is held, so the projection stays unsettled
  // until the test releases it.
  let snapshots = 0;
  let release!: () => void;
  const held = new Promise<void>(resolve => { release = resolve; });
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", async route => {
    if (snapshots++ > 0) await held;
    await route.fulfill({ json: envelope({ instances: [liveSession] }) });
  });
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: [{ root }] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.goto("/");
  await expect(page.locator(".af-row")).toHaveCount(1);

  await page.locator(".af-project-switch").click();
  await page.locator(".af-project-delete").click();
  const modal = page.getByRole("dialog");
  const confirm = modal.getByRole("button", { name: "Delete project", exact: true });
  await expect(modal).toContainText("Could not check live sessions");
  await expect(modal).not.toContainText("Archive 1 session");
  await expect(modal).not.toContainText("No live sessions");
  await expect(confirm).toBeDisabled();

  // The re-read lands: the same dialog now states the observed count.
  release();
  await expect(modal).toContainText("Archive 1 session and remove the project.");
  await expect(modal).not.toContainText("Could not check live sessions");
  await expect(confirm).toBeEnabled();
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
});

test("project delete states no counts while the daemon skipped a repo (#7201)", async ({ page }) => {
  // A skipped repo's sessions are missing from the list, so every count may be
  // hiding sessions the delete would still archive or end (the TUI's #6986 rule).
  await page.routeWebSocket("**/v1/events*", () => {});
  await page.route("**/v1/Snapshot", route => route.fulfill({ json: envelope({
    instances: [liveSession],
    skipped_repos: [{ repo_id: "0123456789ab", reason: "corrupted-instances-json" }],
  }) }));
  await page.route("**/v1/ListProjects", route => route.fulfill({ json: envelope({ projects: [{ root }] }) }));
  await page.route("**/v1/ListTasks", route => route.fulfill({ json: envelope({ tasks: [] }) }));
  await page.goto("/");
  await page.locator("#app[data-af-resync-settled]").waitFor({ state: "attached" });

  await page.locator(".af-project-switch").click();
  await page.locator(".af-project-delete").click();
  const modal = page.getByRole("dialog");
  await expect(modal).toContainText("Could not check live sessions");
  await expect(modal).toContainText("could not load 1 repo");
  await expect(modal).not.toContainText("Archive 1 session");
  await expect(modal.getByRole("button", { name: "Delete project", exact: true })).toBeDisabled();
  await modal.getByRole("button", { name: "Cancel", exact: true }).click();
});
