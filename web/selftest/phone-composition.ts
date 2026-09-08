import { expect, type Page } from "@playwright/test";

/** Exercise CompositionHelper with and without a post-composition insertText. */
export async function assertPhoneComposition(page: Page, stream: () => string): Promise<void> {
  const textarea = page.locator(".af-pane-host .xterm-helper-textarea").first();
  const ctrl = page.locator(".af-terminal-keybar:visible").getByRole("button", { name: "Ctrl", exact: true });
  for (const [postCommit, trailing, committed, eventData] of [
    [false, "", "字", "字"], [false, "x", "字", "字"], [true, "", "字", "字"], [true, "x", "字", "字"],
    [true, "", "각", "가"], [true, "x", "각", "가"],
    [true, "x", "가나", "ᄀ"], [true, "x", "ab", "b"],
  ] as const) {
    const before = stream();
    await ctrl.click();
    await expect(ctrl).toHaveAttribute("data-state", "once");
    await textarea.evaluate(async (el, { postCommit, trailing, committed, eventData }) => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: eventData }));
      if (committed === "가나") input.value += "ᄀ"; // shorter provisional value
      if (committed === "ab") input.value += "a"; // append-shaped final commit
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: eventData }));
      if (postCommit || trailing) input.dispatchEvent(new InputEvent("beforeinput", {
        bubbles: true, composed: true, data: eventData, inputType: "insertText", isComposing: false,
      }));
      input.value = input.value.substring(0, start) + committed;
      if (postCommit || trailing) input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: eventData, inputType: "insertText", isComposing: false,
      }));
      if (trailing) {
        input.value += trailing;
        input.dispatchEvent(new InputEvent("input", {
          bubbles: true, composed: true, data: trailing, inputType: "insertText", isComposing: false,
        }));
      }
      // Let xterm's compositionend timer finish before checking for duplicates.
      await new Promise(resolve => setTimeout(resolve, 0));
    }, { postCommit, trailing, committed, eventData });
    if (!trailing) await expect.poll(stream).toBe(before + committed);
    await expect(ctrl).toHaveAttribute("data-state", trailing ? "off" : "once");
    await expect.poll(stream).toBe(before + committed + (trailing ? "\x18" : ""));
    await expect(ctrl).toHaveAttribute("data-state", trailing ? "off" : "once");
    await expect(textarea).toBeFocused();
    await page.keyboard.insertText("x");
    await expect.poll(stream).toBe(before + committed + "\x18" + (trailing ? "x" : ""));
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  const before = stream();
  await ctrl.click();
  await expect(ctrl).toHaveAttribute("data-state", "once");
  await textarea.evaluate(async el => {
    const input = el as HTMLTextAreaElement;
    const dispatch = (type: string, data: string) =>
      input.dispatchEvent(new CompositionEvent(type, { bubbles: true, data }));
    const tick = () => new Promise(resolve => setTimeout(resolve, 0));
    dispatch("compositionstart", "");
    dispatch("compositionupdate", "字");
    input.value += "字";
    await tick(); // xterm captures A's end before copying the pending range.
    dispatch("compositionend", "字");
    dispatch("compositionstart", ""); // B starts before A's delayed send.
    dispatch("compositionupdate", "文");
    input.value += "文";
    await tick();
    dispatch("compositionend", "文");
    await tick();
  });
  await expect.poll(stream).toBe(before + "字文");
  await expect(ctrl).toHaveAttribute("data-state", "once");
  await page.keyboard.insertText("x");
  await expect.poll(stream).toBe(before + "字文\x18");
  await expect(ctrl).toHaveAttribute("data-state", "off");
}
