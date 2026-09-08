import { test, expect } from "@playwright/test";

for (const surface of ["drawer", "rebuilt header"] as const) {
  test(`delete consent returns focus to the ${surface}`, async ({ page, request }) => {
    if (surface === "drawer") await page.setViewportSize({ width: 360, height: 740 });
    const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
    const original = envelope.data.instances.find((s: { title: string }) => s.title === "probe-a");
    let target = { ...original, is_root: surface === "drawer" };
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: {
      ...envelope, data: { ...envelope.data, instances: [target] },
    } }));
    let resync!: () => void;
    await page.routeWebSocket("**/v1/events*", socket => {
      resync = () => socket.send(JSON.stringify({ type: "session.restored", data: { id: target.id } }));
    });
    await page.routeWebSocket("**/stream*", () => {});
    await page.goto(`/#/session/${target.id}`);
    await page.locator("#app[data-af-resync-settled]").waitFor();
    const modal = page.locator(".af-modal-card");
    if (surface === "drawer") {
      const toggle = page.locator(".af-nav-toggle");
      await toggle.click();
      const row = page.locator(".af-row").first();
      await row.getByRole("button", { name: /^Actions for / }).click();
      const action = row.getByRole("button", { name: /^(Kill|Delete) session/ });
      await action.focus();
      await page.keyboard.press("Enter");
      await expect(page.locator(".af-app")).not.toHaveClass(/af-nav-open/);
      await expect(modal.getByRole("checkbox")).toBeFocused();
      await page.keyboard.press("Escape");
      await expect(modal).toHaveCount(0);
      await expect(toggle).toBeVisible();
      await expect(toggle).toBeFocused();
    } else {
      // Keep selection while filtering its row out, exposing header management.
      for (const kind of ["needs-you", "working", "waiting-limit", "broken"]) {
        await page.locator(".af-rail-filter").click();
        const item = page.locator(`.af-filter-item[data-kind="${kind}"]`);
        if (await item.getAttribute("aria-checked") === "true") await item.click();
        await page.locator(".af-rail-title").click();
      }
      await expect(page.locator(".af-row")).toHaveCount(0);
      await page.getByRole("button", { name: "Session actions", exact: true }).click();
      const action = page.locator(".af-term-head").getByRole("button", { name: /^(Kill|Delete) session/ });
      await action.focus();
      const originalAction = await action.elementHandle();
      await page.keyboard.press("Enter");
      await expect(modal).toBeVisible();
      await expect(modal.getByRole("checkbox")).toHaveCount(0);
      // Same id/title: only root identity changes, rebuilding the header buttons
      // before the open generic dialog upgrades to the acknowledgment dialog.
      target = { ...target, is_root: true };
      resync();
      await expect(modal.getByRole("checkbox")).toBeFocused();
      expect(await originalAction!.evaluate(el => el.isConnected)).toBe(false);
      await page.keyboard.press("Escape");
      await expect(modal).toHaveCount(0);
      // The action's popover may close during reconciliation; its current
      // disclosure is the visible header fallback used by other confirmations.
      await expect.poll(() => page.evaluate(() => {
        const focused = document.activeElement;
        return focused instanceof HTMLElement && !!focused.closest(".af-term-head") &&
          (focused.matches(".af-term-more") || /^(Kill|Delete) session/.test(focused.getAttribute("aria-label") ?? "")) &&
          getComputedStyle(focused).visibility === "visible" && focused.getClientRects().length > 0;
      })).toBe(true);
    }
  });
}
