import { expect, test } from "@playwright/test";
import { assertPhoneBarModifiers, phoneInputStream } from "./phone-keybar.js";

test("#4036 phone keybar applies and consumes modifiers before the next letter", async ({ page, request }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 812 });
  const stream = phoneInputStream(page);
  const response = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  const payload = await response.json();
  const selected = payload.data.instances.find((session: { title: string }) =>
    session.title === (process.env.AF_WEB_SESSION_A ?? "probe-a"));
  expect(selected?.id).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator(".af-pane-host .xterm").first().click();
  await expect(page.locator(".af-pane-host .xterm-helper-textarea").first()).toBeFocused();
  try {
    await assertPhoneBarModifiers(page, stream);
    // The demo repeats this flow after releasing terminal focus with Ctrl+].
    // Its keyup lands outside xterm; plain soft input must still work on return.
    await page.keyboard.press("Control+]");
    await expect(page.locator(".af-terminal-keybar")).toHaveCount(0);
    await page.locator(".af-pane-host .xterm-helper-textarea").first().focus();
    await assertPhoneBarModifiers(page, stream);
    const before = stream();
    await page.keyboard.type("xy");
    await expect.poll(stream).toBe(before + "xy");
  } finally {
    await page.screenshot({ path: testInfo.outputPath("phone-keybar.png") });
    console.log("#4036 PTY input:", JSON.stringify(stream()));
  }
});
