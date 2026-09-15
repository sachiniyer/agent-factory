import { test, expect } from "@playwright/test";

for (const [backend, outcome] of [["local", "refused"], ["docker", "refused"], ["local", "dismissed"], ["local", "success"], ["local", "warning"], ["local", "uncertain"], ["local", "duplicate"]]) {
  test(`4017: ${backend} restore consent and ${outcome}`, async ({ page, request }, info) => {
    const response = await request.post("/v1/Snapshot", { data: {} });
    const snapshot = await response.json();
    const original = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved"));
    expect(original?.id).toBeTruthy();
    const session = { ...original, backend_type: backend };
    await page.routeWebSocket("**/v1/events*", () => {});
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: { data: { instances: [session] } } }));
    let calls = 0;
    let body: Record<string, unknown> | undefined;
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    await page.route("**/v1/RestoreSession", async route => {
      calls++;
      body = route.request().postDataJSON();
      await pending;
      if (outcome === "success" || outcome === "warning" || outcome === "duplicate") {
        session.liveness = 1;
        session.lifecycle_action = "archive";
        await route.fulfill({ json: { data: outcome === "warning" ? { warning: "restore completed with test warning" } : {} } });
      } else if (outcome === "uncertain") {
        await route.abort("connectionfailed");
      } else {
        await route.fulfill({ json: { data: null, error: { message: "restore refused for test", daemon_rejected: true } } });
      }
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator(".af-term-title")).toHaveText(session.title);
    const row = page.locator(".af-row-selected");
    await row.getByRole("button", { name: `Actions for ${session.title}`, exact: true }).click();
    await row.getByRole("button", { name: `Restore session “${session.title}”`, exact: true }).click();
    const dialog = page.getByRole("dialog");
    if (backend !== "local") {
      await expect(dialog.getByRole("button", { name: "Restore", exact: true })).toBeEnabled();
      expect(calls).toBe(0);
      await dialog.getByRole("button", { name: "Restore", exact: true }).click();
    }
    await expect.poll(() => calls).toBe(1);
    expect(body?.id).toBe(session.id);
    expect(body).not.toHaveProperty("force_reap");
    await expect(dialog).toHaveClass(/af-modal-busy/);
    await page.screenshot({ path: info.outputPath(`${backend}-restore-progress.png`) });
    if (backend === "local") await expect(dialog.getByRole("button", { name: "Restoring…", exact: true })).toBeDisabled();
    if (outcome === "dismissed" || outcome === "duplicate") await page.keyboard.press("Escape");
    if (outcome === "duplicate") {
      await row.getByRole("button", { name: `Actions for ${session.title}`, exact: true }).click();
      const restore = row.getByRole("button", { name: `Restore session “${session.title}”`, exact: true });
      // A second native click must be inert even without an events connection.
      await restore.evaluate((button: HTMLButtonElement) => button.click());
      await expect(restore).toBeDisabled();
      expect(calls).toBe(1);
      await expect(dialog).toBeHidden();
      // Filtering the row moves its actions to the header; the fence follows it.
      await page.locator(".af-rail-filter").click();
      await page.locator('.af-filter-item[data-kind="archived"]').click();
      await page.locator(".af-rail-title").click();
      await expect(row).toHaveCount(0);
      await page.getByRole("button", { name: "Session actions", exact: true }).click();
      const headerRestore = page.locator(".af-term-actions").getByRole("button", { name: `Restore session “${session.title}”`, exact: true });
      await expect(headerRestore).toBeVisible();
      await expect(headerRestore).toBeDisabled();
      await headerRestore.evaluate((button: HTMLButtonElement) => button.click());
      expect(calls).toBe(1);
      await expect(dialog).toBeHidden();
    }
    release();
    if (outcome === "refused") {
      await expect(dialog).toContainText("restore refused for test");
      await expect(dialog).not.toHaveClass(/af-modal-busy/);
      await expect(page.locator(".af-row-selected")).toHaveClass(/af-row-archived/);
    } else {
      await expect(dialog).toBeHidden();
      if (outcome === "duplicate") {
        expect(calls).toBe(1);
        await expect(page.getByText("Operation failed", { exact: true })).toBeHidden();
      }
      if (outcome === "success" || outcome === "duplicate") await expect(page.locator(".af-row-selected")).not.toHaveClass(/af-row-archived/);
      if (outcome === "warning") await expect(page.getByText("Operation completed", { exact: true })).toBeVisible();
      if (outcome === "uncertain") await expect(page.getByText("Outcome not confirmed", { exact: true })).toBeVisible();
      if (outcome === "dismissed") await expect(page.getByText("restore refused for test", { exact: true })).toBeVisible();
    }
  });
}
