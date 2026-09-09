import { assertPhoneComposition } from "./phone-composition.js";
import { expect, test } from "@playwright/test";
import { assertPhoneBarModifiers, assertPhoneStaleRecovery, leavePageAndCleanup,
  phoneInputStream } from "./phone-keybar.js";
import { assertPhoneKeybarInputEffects } from "./phone-keybar-effects.js";

test("#4036 phone keybar applies and consumes modifiers before the next letter", async ({ page, request }, testInfo) => {
  await page.setViewportSize({ width: 390, height: 812 });
  const stream = phoneInputStream(page);
  // Cursor sequences echoed by cat can overwrite prior output when a later
  // test submits its input buffer. Never send these gestures to shared probe-a.
  const response = await request.post("/v1/CreateSession", { data: {
    repo_path: process.env.AF_MOCK_REPO, title_base: "probe-phone-keybar",
  } });
  const created = await response.json();
  expect(created.error).toBeFalsy();
  const id = created.data.instance.id;
  expect(id).toBeTruthy();
  try {
    await page.goto(`/#/session/${encodeURIComponent(id)}`);
    await page.locator(".af-pane-host .xterm").first().click();
    await expect(page.locator(".af-pane-host .xterm-helper-textarea").first()).toBeFocused();
    await assertPhoneComposition(page, stream);
    await assertPhoneStaleRecovery(page, stream);
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
    await assertPhoneKeybarInputEffects(page, stream);
  } finally {
    try {
      await page.screenshot({ path: testInfo.outputPath("phone-keybar.png") });
      console.log("#4036 PTY input:", JSON.stringify(stream()));
    } finally {
      await leavePageAndCleanup(page, async () => {
        const killed = await request.post("/v1/KillSession", { data: { id } });
        expect((await killed.json()).error).toBeFalsy();
      });
    }
  }
});
