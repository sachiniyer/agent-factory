import { test, expect } from "@playwright/test";

for (const state of ["pending", "failed", "loaded-empty"] as const) {
  test(`new-session account policy: ${state}`, async ({ page }) => {
    let release!: () => void;
    const waiting = new Promise<void>(resolve => { release = resolve; });
    await page.route("**/v1/ListAccounts", async route => {
      if (state === "pending") await waiting;
      await route.fulfill(state === "failed"
        ? { status: 503, json: { data: null, error: { message: "Registry unavailable" } } }
        : { json: { data: { agents: ["claude"], entries: [], defaults: {} }, error: null } });
    });
    try {
      await page.goto("/");
      await page.locator(".af-rail-new").click();
      const modal = page.getByRole("dialog");
      await modal.getByLabel("Session title", { exact: true }).fill("account-policy-probe");
      const label = state === "pending" ? "Loading accounts…"
        : state === "failed" ? "Accounts unavailable" : "Use agent login (no default)";
      await expect(modal.getByLabel("Account", { exact: true }).locator("option:checked")).toHaveText(label);
      const create = modal.getByRole("button", { name: "Create", exact: true });
      if (state === "pending") {
        await expect(create).toBeDisabled();
        release();
        await expect(modal.getByLabel("Account", { exact: true }).locator("option:checked")).toHaveText("Use agent login (no default)");
        await expect(create).toBeEnabled();
      } else {
        await expect(create).toBeEnabled();
        if (state === "failed") await expect(modal).toContainText("daemon default, if any, applies");
      }
      await modal.getByRole("button", { name: "Cancel", exact: true }).click();
    } finally {
      release();
    }
  });
}
