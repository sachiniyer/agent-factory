import { test, expect } from "@playwright/test";

for (const outcome of ["refused", "success", "warning", "failed-snapshot"]) {
  test(`restore fence: ${outcome} after disconnect`, async ({ page, request }) => {
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved"));
    expect(session?.id).toBeTruthy();
    session.backend_type = "local";
    await page.routeWebSocket("**/v1/events*", () => {});
    let snapshots = 0;
    let failSnapshot = false;
    await page.route("**/v1/Snapshot", route => {
      snapshots++;
      return failSnapshot ? route.abort("connectionfailed") : route.fulfill({ json: { data: { instances: [session] } } });
    });
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    let restores = 0;
    await page.route("**/v1/RestoreSession", async route => {
      restores++;
      await pending;
      await route.fulfill({ json: outcome === "refused"
        ? { data: null, error: { message: "old connection restore refused", daemon_rejected: true } }
        : { data: outcome === "warning" ? { warning: "old connection restore warning" } : {} } });
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    const row = page.locator(".af-row-selected");
    await row.getByRole("button", { name: `Actions for ${session.title}`, exact: true }).click();
    await row.getByRole("button", { name: `Restore session “${session.title}”`, exact: true }).click();
    await expect.poll(() => restores).toBe(1);
    await page.keyboard.press("Escape");
    if (outcome !== "failed-snapshot") {
      // Reconnect with the SAME tokenless credential: token equality is insufficient.
      await page.locator(".af-appbar button", { hasText: "Disconnect" }).evaluate((button: HTMLButtonElement) => button.click());
      await expect(page.locator(".af-login")).toBeVisible();
      await page.locator(".af-login button.af-primary").click();
      await expect(page.locator(".af-app")).toBeVisible();
      await expect(page.locator(".af-login")).toBeHidden();
      // Exclude the new connection's own scheduled Snapshot from completion effects.
      await expect(page.locator("#app")).toHaveAttribute("data-af-resync-settled", "");
    } else {
      failSnapshot = true;
    }
    const before = snapshots;
    const completed = page.waitForResponse("**/v1/RestoreSession");
    release();
    await completed;
    // Allow the 150ms resync debounce to fire, including stale completion side effects.
    await page.waitForTimeout(400);
    await expect(page.getByRole("dialog")).toBeHidden();
    await expect(page.getByText(/old connection restore/)).toBeHidden();
    if (outcome === "failed-snapshot") {
      expect(snapshots).toBeGreaterThan(before);
      await row.getByRole("button", { name: `Actions for ${session.title}`, exact: true }).click();
      const restore = row.getByRole("button", { name: `Restore session “${session.title}”`, exact: true });
      await expect(restore).toBeDisabled();
      await restore.evaluate((button: HTMLButtonElement) => button.click());
      expect(restores).toBe(1);
      await expect(page.getByRole("dialog")).toBeHidden();
    } else {
      expect(snapshots).toBe(before);
    }
  });
}

for (const outcome of ["committed", "uncertain", "lost", "dead"]) {
  test(`restore fence retains ${outcome} when the follow-up Snapshot fails`, async ({ page, request }) => {
    const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
    const session = snapshot.data.instances.find((s: { title: string }) =>
      s.title === (process.env.AF_WEB_SESSION_WEB_SHELVED ?? "probe-shelved"));
    expect(session?.id).toBeTruthy();
    session.backend_type = "local";
    if (outcome === "lost" || outcome === "dead") {
      session.liveness = outcome === "lost" ? 3 : 4;
      session.status = outcome === "lost" ? 5 : 4;
    }
    await page.routeWebSocket("**/v1/events*", () => {});
    let failSnapshot = false;
    let snapshots = 0;
    await page.route("**/v1/Snapshot", route => {
      snapshots++;
      return failSnapshot ? route.abort("connectionfailed") : route.fulfill({ json: { data: { instances: [session] } } });
    });
    let release!: () => void;
    const pending = new Promise<void>(resolve => { release = resolve; });
    let restores = 0;
    await page.route("**/v1/RestoreSession", async route => {
      restores++;
      await pending;
      if (outcome === "uncertain") return route.abort("connectionfailed");
      await route.fulfill({ json: { data: outcome === "committed" ? { warning: "restore completed with warning" } : {} } });
    });
    await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
    await expect(page.locator("#app")).toHaveAttribute("data-af-resync-settled", "");
    const row = page.locator(".af-row-selected");
    const actions = row.getByRole("button", { name: `Actions for ${session.title}`, exact: true });
    const restore = row.getByRole("button", { name: `Restore session “${session.title}”`, exact: true });
    await actions.click();
    await restore.click();
    await expect.poll(() => restores).toBe(1);
    await page.keyboard.press("Escape");
    failSnapshot = true;
    const before = snapshots;
    release();
    await expect.poll(() => snapshots).toBeGreaterThan(before);
    if (outcome === "committed") await expect(page.getByText("Operation completed", { exact: true })).toBeVisible();
    if (outcome === "uncertain") await expect(page.getByText("Outcome not confirmed", { exact: true })).toBeVisible();
    await expect(page.getByRole("dialog")).toBeHidden();
    await actions.click();
    await expect(restore).toBeDisabled();
    await restore.evaluate((button: HTMLButtonElement) => button.click());
    expect(restores).toBe(1);
    await expect(page.getByRole("dialog")).toBeHidden();
  });
}
