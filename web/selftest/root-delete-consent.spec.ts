import { test, expect } from "@playwright/test";

for (const isRoot of [true, false]) {
  test(`root deletion consent follows daemon identity ${isRoot}`, async ({ page, request }, info) => {
    const envelope = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
    const original = envelope.data.instances.find((s: { title: string }) => s.title === "probe-a");
    // Neither a special title nor the currently selected row establishes root ownership.
    const target = { ...original, is_root: isRoot, title: isRoot ? "project coordinator" : "root" };
    await page.route("**/v1/Snapshot", route => route.fulfill({ json: {
      ...envelope, data: { ...envelope.data, instances: [target] },
    } }));
    await page.routeWebSocket("**/v1/events*", () => {});
    let calls = 0;
    await page.route("**/v1/KillSession", route => {
      calls++;
      expect(route.request().postDataJSON().id).toBe(target.id);
      return route.fulfill({ status: 503, json: { error: { message: "Deletion refused", daemon_rejected: true } } });
    });
    await page.goto("/");
    const open = async () => {
      const row = page.locator(".af-row").first();
      await row.hover();
      await row.getByRole("button", { name: /^Actions for / }).click();
      await row.getByRole("button", { name: /^(Kill|Delete) session/ }).click();
    };
    await open();
    const modal = page.locator(".af-modal-card");
    await page.screenshot({ path: info.outputPath("root-delete-consent.png") });
    const acknowledgment = modal.getByRole("checkbox");
    if (isRoot) {
      await expect(modal).toContainText("scheduled and watch-task delivery");
      await expect(acknowledgment).not.toBeChecked();
      await modal.locator("form").evaluate(form => form.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true })));
      await expect(modal.locator(".af-modal-error")).toBeVisible();
      expect(calls).toBe(0);
      await acknowledgment.check();
      await acknowledgment.uncheck();
      await modal.getByRole("button", { name: "Cancel", exact: true }).click();
      expect(calls).toBe(0);
      await open();
      await expect(acknowledgment).not.toBeChecked();
      await acknowledgment.check();
    } else {
      await expect(acknowledgment).toHaveCount(0);
    }
    await modal.locator("button[type=submit]").click();
    await expect(modal.locator(".af-modal-error")).toContainText("Deletion refused");
    expect(calls).toBe(1);
    if (isRoot) await expect(acknowledgment).toBeChecked();
  });
}
