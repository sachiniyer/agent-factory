import { expect, test } from "@playwright/test";

for (const theme of ["light", "dark"] as const) {
  test(`visual polish: shared buttons remain legible and usable in ${theme}`, async ({ page }) => {
    await page.emulateMedia({ colorScheme: theme });
    await page.goto("/");
    await expect(page.locator(".af-app")).toBeVisible();
    await page.locator(".af-rail-new").click();
    const dialog = page.getByRole("dialog", { name: "New session", exact: true });
    const cancel = dialog.getByRole("button", { name: "Cancel", exact: true });
    for (const width of [1440, 430, 390, 375, 360]) {
      await page.setViewportSize({ width, height: 900 });
      await expect(cancel).toBeInViewport();
      const style = await cancel.evaluate(button => {
        const css = getComputedStyle(button);
        const rgb = (s: string) => s.match(/[\d.]+/g)!.slice(0, 3).map(Number);
        const luminance = (s: string) => rgb(s).map(c => {
          const n = c / 255;
          return n <= 0.04045 ? n / 12.92 : ((n + 0.055) / 1.055) ** 2.4;
        }).reduce((sum, n, i) => sum + n * [0.2126, 0.7152, 0.0722][i], 0);
        const contrast = (a: string, b: string) => {
          const x = luminance(a), y = luminance(b);
          return (Math.max(x, y) + 0.05) / (Math.min(x, y) + 0.05);
        };
        const probe = document.createElement("span");
        probe.style.color = "var(--af-border)";
        button.append(probe);
        const border = getComputedStyle(probe).color;
        probe.remove();
        return { borderMatchesToken: css.borderColor === border, weight: css.fontWeight,
          label: contrast(css.color, css.backgroundColor), height: button.getBoundingClientRect().height,
          padding: parseFloat(css.paddingInlineStart) };
      });
      expect(style.borderMatchesToken).toBe(true);
      expect(style.weight).toBe("600");
      expect(style.label).toBeGreaterThanOrEqual(4.5);
      expect(style.height).toBeGreaterThanOrEqual(44);
      expect(style.padding).toBeGreaterThanOrEqual(16);
    }
    await cancel.hover();
    await expect(cancel).toHaveCSS("text-decoration-line", "none");
    await cancel.focus();
    await page.keyboard.press("Tab");
    await page.keyboard.press("Shift+Tab");
    await expect(cancel).toHaveCSS("outline-width", "2px");
    await cancel.click();
    await expect(dialog).toHaveCount(0);
  });
}
