import { test, expect } from "@playwright/test";

// This runs against the embedded app only inside the sanctioned web testbox.
test("4017: t opens the button's New tab picker without creating a tab", async ({ page, request }, info) => {
  const response = await request.post("/v1/Snapshot", { data: {} });
  const snapshot = await response.json();
  const session = snapshot.data.instances.find((s: { title: string }) =>
    s.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(session?.id).toBeTruthy();
  let creates = 0;
  await page.route("**/v1/CreateTab", route => {
    creates++;
    return route.fulfill({ json: { data: null, error: { message: "picker test refusal", daemon_rejected: true } } });
  });
  await page.goto(`/#/session/${encodeURIComponent(session.id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(session.title);
  await page.keyboard.press("Control+]");
  const trigger = page.getByRole("button", { name: "New tab · Terminal or VS Code", exact: true });
  const menu = page.getByRole("menu", { name: "Tab type", exact: true });
  const tabs = await page.locator(".af-tabbar .af-tab").count();
  await page.keyboard.press("t");
  await expect(menu).toBeVisible();
  await expect(trigger).toHaveAttribute("aria-expanded", "true");
  await expect(menu.getByRole("menuitem", { name: "Terminal", exact: true })).toBeFocused();
  expect(creates).toBe(0);
  await expect(page.locator(".af-tabbar .af-tab")).toHaveCount(tabs);
  await page.screenshot({ path: info.outputPath("new-tab-picker.png") });
  await page.keyboard.press("Escape");
  await expect(menu).toBeHidden();
  await expect(trigger).toBeFocused();
  expect(creates).toBe(0);
  await trigger.click();
  await expect(menu).toBeVisible();
  await menu.getByRole("menuitem", { name: "Terminal", exact: true }).click();
  await expect.poll(() => creates).toBe(1);
  await expect(page.locator(".af-toast")).toContainText("picker test refusal");
});
