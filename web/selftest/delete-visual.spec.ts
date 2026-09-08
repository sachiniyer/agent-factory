import { test, expect } from "@playwright/test";

for (const theme of ["light", "dark"]) {
  test(`session deletion has destructive confirmation in ${theme}`, async ({ page }, info) => {
    await page.goto("/");
    await page.getByRole("button", { name: theme === "light" ? "Light" : "Dark", exact: true }).click();
    await expect(page.locator("html")).toHaveAttribute("data-af-theme", theme);
    const row = page.locator(".af-row").first();
    await row.hover();
    await row.getByRole("button", { name: /^Actions for / }).click();
    await row.getByRole("button", { name: /^(Kill|Delete) session/ }).click();
    const button = page.locator(".af-modal-card button[type=submit]");
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
    const cancel = page.getByRole("button", { name: "Cancel", exact: true });
    await cancel.focus();
    await page.keyboard.press("Tab");
    await expect(button).toBeFocused();
    expect(await button.evaluate(el => el.matches(":focus-visible"))).toBe(true);
    const focus = await button.evaluate(el => {
      const probe = document.createElement("span");
      el.append(probe);
      const tokenColor = (token: string) => {
        probe.style.color = `var(${token})`;
        return getComputedStyle(probe).color;
      };
      const accent = tokenColor("--af-accent");
      const danger = tokenColor("--af-dead");
      probe.remove();
      const style = getComputedStyle(el);
      return { accent, danger, outline: style.outlineColor, text: style.color, border: style.borderTopColor };
    });
    await page.screenshot({ path: info.outputPath("delete-confirmation.png") });
    expect(focus.outline).toBe(focus.accent);
    expect(focus.outline).not.toBe(focus.danger);
    expect(focus.text).toBe(focus.danger);
    expect(focus.border).toBe(focus.danger);
    await cancel.click();
    await expect(page.locator(".af-modal-card")).toHaveCount(0);
  });
}
