import { expect, type Page } from "@playwright/test";

/** Exercise xterm's user-input effects as well as the outgoing modifier bytes. */
export async function assertPhoneKeybarInputEffects(page: Page, stream: () => string): Promise<void> {
  const host = page.locator(".af-pane-host").first();
  const viewport = host.locator(".xterm-viewport");
  const bar = page.locator(".af-terminal-keybar:visible");
  for (let i = 0; i < 60; i++) {
    await page.keyboard.type(`keybar-history-${i}`);
    await page.keyboard.press("Enter");
  }
  await expect(host).toContainText("keybar-history-59");
  await bar.getByRole("button", { name: "Ctrl", exact: true }).click();
  await bar.getByRole("button", { name: "Arrows", exact: true }).click();
  await host.hover();
  await page.mouse.wheel(0, -5000);
  await expect(host).not.toContainText("keybar-history-59");
  const parked = await viewport.evaluate(el => el.scrollTop);
  const row = host.locator(".xterm-rows > div", { hasText: "keybar-history-" }).first();
  const box = await row.boundingBox();
  expect(box).toBeTruthy();
  await page.mouse.move(box!.x + 4, box!.y + box!.height / 2);
  await page.mouse.down();
  await page.mouse.move(box!.x + 120, box!.y + box!.height / 2, { steps: 8 });
  await page.mouse.up();
  const selection = host.locator(".xterm-selection > div");
  await expect(selection).not.toHaveCount(0);
  const before = stream();
  await bar.getByRole("button", { name: "↑", exact: true }).click();
  await expect.poll(stream).toBe(before + "\x1b[1;5A");
  await expect(selection, "bar input must clear xterm's selection").toHaveCount(0);
  await expect.poll(() => viewport.evaluate(el => el.scrollTop)).toBeGreaterThan(parked);
  await expect(host, "bar input must return from scrollback to the prompt").toContainText("keybar-history-59");
}
