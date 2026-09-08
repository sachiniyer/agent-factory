import { expect, test } from "@playwright/test";

for (const colorScheme of ["light", "dark"] as const) {
  test(`#4065: expressive session identity and quiet controls · ${colorScheme}`, async ({ page }) => {
    await page.emulateMedia({ colorScheme });
    // No input reaches an agent; the shell still mounts the real terminal.
    await page.routeWebSocket(url => url.pathname.endsWith("/stream"), () => {});
    await page.goto("/");
    const title = process.env.AF_WEB_SESSION_A ?? "probe-a";
    await page.locator(".af-row").filter({ has: page.locator(".af-row-title", { hasText: title }) }).click();
    await expect(page.locator(".af-term-host .xterm")).toBeVisible();
    const filter = page.getByRole("button", { name: "Filter sessions", exact: true });
    await expect(page.locator(".af-session-identity")).toContainText("Default account");
    await expect(page.locator(".af-row-identity").first()).toBeVisible();
    await expect(filter).toHaveCSS("border-top-color", "rgba(0, 0, 0, 0)");
    await expect(page.locator(".af-term-title")).toHaveCSS("font-size", "28px");
    await expect(page.locator(".af-row-branch").first()).toHaveCSS("font-size", "13px");
    await page.keyboard.press("Control+]");
    await filter.focus();
    await expect(filter).toHaveCSS("outline-style", "solid");
    await expect(filter).toHaveCSS("outline-width", "2px");

    await page.setViewportSize({ width: 360, height: 812 });
    await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);
    await expect(page.locator(".af-term-title")).toHaveCSS("font-size", "16px");
    const toggle = page.locator(".af-nav-toggle");
    await expect(toggle).toHaveCSS("border-top-color", "rgba(0, 0, 0, 0)");
    const box = await toggle.boundingBox();
    expect(box!.width).toBeGreaterThanOrEqual(44);
    expect(box!.height).toBeGreaterThanOrEqual(44);
    const header = await page.locator(".af-appbar").boundingBox();
    expect(header!.height).toBe(48);
    await page.locator(".af-term-host .xterm-helper-textarea").focus();
    await expect(page.locator(".af-keybar-row:not([hidden]) button").first()).toHaveCSS("border-top-color", "rgba(0, 0, 0, 0)");
  });
}
