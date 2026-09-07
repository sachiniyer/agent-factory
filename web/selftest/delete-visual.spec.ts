import { test, expect } from "@playwright/test";

for (const theme of ["light", "dark"]) {
  test(`session deletion has destructive confirmation in ${theme}`, async ({ page }, info) => {
    await page.goto("/");
    await page.evaluate(theme => document.documentElement.setAttribute("data-theme", theme), theme);
    const row = page.locator(".af-row").first();
    await row.hover();
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: /^(Kill|Delete) session/ }).click();
    const button = page.locator(".af-modal-card button[type=submit]");
    await page.screenshot({ path: info.outputPath("delete-confirmation.png") });
    await expect(button).toHaveClass("af-danger");
    const colors = await button.evaluate(el => ({
      foreground: getComputedStyle(el).color,
      border: getComputedStyle(el).borderTopColor,
      cancel: getComputedStyle(document.querySelector(".af-modal-foot .af-ghost")!).color,
    }));
    expect(colors.foreground).not.toBe(colors.cancel);
    expect(colors.border).toBe(colors.foreground);
    await button.hover();
    expect(await button.evaluate(el => getComputedStyle(el).color)).toBe(colors.foreground);
    await page.getByRole("button", { name: "Cancel", exact: true }).click();
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
  });
}
