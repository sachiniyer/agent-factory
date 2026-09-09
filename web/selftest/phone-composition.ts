import { expect, type Page } from "@playwright/test";

/** Exercise CompositionHelper with and without a post-composition insertText. */
export async function assertPhoneComposition(page: Page, stream: () => string): Promise<void> {
  const textarea = page.locator(".af-pane-host .xterm-helper-textarea").first();
  const ctrl = page.locator(".af-terminal-keybar:visible").getByRole("button", { name: "Ctrl", exact: true });
  for (const [postCommit, trailing, committed, eventData, inputData] of [
    [false, "", "字", "字", "字"], [false, "x", "字", "字", "字"],
    [true, "", "字", "字", "字"], [true, "x", "字", "字", "字"],
    [true, "x", "字", "字", null],
    [true, "x", "字", "", null],
    [true, "", "각", "가", "가"], [true, "x", "각", "가", "가"],
    [true, "x", "가나", "ᄀ", "ᄀ"], [true, "x", "ab", "b", "b"],
  ] as const) {
    const before = stream();
    await ctrl.click();
    await expect(ctrl).toHaveAttribute("data-state", "once");
    await textarea.evaluate(async (el, { postCommit, trailing, committed, eventData, inputData }) => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: eventData }));
      // Chrome commits and mutates before compositionend; Safari commits after
      // it. Keep the two orders separate so the boundary is browser-derived.
      if (!postCommit) {
        input.value = input.value.substring(0, start) + committed;
        input.dispatchEvent(new InputEvent("input", {
          bubbles: true, composed: true, data: eventData, inputType: "insertText", isComposing: true,
        }));
      }
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: eventData }));
      if (postCommit) {
        const keydown = new KeyboardEvent("keydown", { bubbles: true, cancelable: true, key: "Unidentified" });
        Object.defineProperty(keydown, "keyCode", { value: 229 });
        input.dispatchEvent(keydown);
      }
      if (postCommit) input.dispatchEvent(new InputEvent("beforeinput", {
        bubbles: true, composed: true, data: inputData, inputType: "insertText", isComposing: false,
      }));
      if (postCommit) {
        input.value = input.value.substring(0, start) + committed;
        input.dispatchEvent(new InputEvent("input", {
          bubbles: true, composed: true, data: inputData, inputType: "insertText", isComposing: false,
        }));
        const keyup = new KeyboardEvent("keyup", { bubbles: true, key: "Unidentified" });
        Object.defineProperty(keyup, "keyCode", { value: 229 });
        input.dispatchEvent(keyup);
      }
      if (trailing) {
        input.value += trailing;
        input.dispatchEvent(new InputEvent("input", {
          bubbles: true, composed: true, data: trailing, inputType: "insertText", isComposing: false,
        }));
      }
      // Let xterm's compositionend timer finish before checking for duplicates.
      await new Promise(resolve => setTimeout(resolve, 0));
    }, { postCommit, trailing, committed, eventData, inputData });
    if (!trailing) await expect.poll(stream).toBe(before + committed);
    await expect(ctrl).toHaveAttribute("data-state", trailing ? "off" : "once");
    await expect.poll(stream).toBe(before + committed + (trailing ? "\x18" : ""));
    await expect(ctrl).toHaveAttribute("data-state", trailing ? "off" : "once");
    await expect(textarea).toBeFocused();
    await page.keyboard.insertText("x");
    await expect.poll(stream).toBe(before + committed + "\x18" + (trailing ? "x" : ""));
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // A post-end textarea mutation can finish a provisional commit. Its live
  // boundary, not nullable InputEvent data, owns the complete composition.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: "ab" }));
      input.value = input.value.substring(0, start) + "a";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "a", inputType: "insertText", isComposing: true,
      }));
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "ab" }));
      input.value = input.value.substring(0, start) + "ab";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: null, inputType: "insertText", isComposing: false,
      }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "ab");
    await expect(ctrl).toHaveAttribute("data-state", "once");
    await page.keyboard.insertText("x");
    await expect.poll(stream).toBe(before + "ab\x18");
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // A stale compositionend payload cannot overrule an already-complete live
  // textarea boundary and claim the next ordinary character as commit growth.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.value = input.value.substring(0, start) + "ab";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "ab", inputType: "insertText", isComposing: true,
      }));
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "x" }));
      input.value += "x";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "x", inputType: "insertText", isComposing: false,
      }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "ab\x18");
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // Even when the stale payload is a strict extension of the complete live
  // value, compositionend data alone cannot make the boundary provisional.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.value = input.value.substring(0, start) + "a";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "a", inputType: "insertText", isComposing: true,
      }));
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "ab" }));
      input.value += "b";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "b", inputType: "insertText", isComposing: false,
      }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "a\x02");
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // A canceled IME owns no prefix; no-keydown x still consumes Ctrl.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "" }));
      input.dispatchEvent(new InputEvent("beforeinput", {
        bubbles: true, cancelable: true, composed: true, data: "x", inputType: "insertText",
      }));
      input.value += "x";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "x", inputType: "insertText",
      }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "\x18");
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // Cancellation is a textarea rollback even after composition updates.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: "字" }));
      input.value = input.value.substring(0, start) + "字";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "字", inputType: "insertText", isComposing: true,
      }));
      input.value = input.value.substring(0, start);
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "" }));
      input.dispatchEvent(new InputEvent("beforeinput", {
        bubbles: true, cancelable: true, composed: true, data: "x", inputType: "insertText",
      }));
      input.value += "x";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "x", inputType: "insertText", isComposing: false,
      }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "\x18");
    await expect(ctrl).toHaveAttribute("data-state", "off");
  }

  // A no-keydown soft character between A and still-active B must not be lost
  // when xterm finalizes A only through its copied end offset.
  {
    const before = stream();
    await ctrl.click();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const composition = (type: string, data: string) =>
        input.dispatchEvent(new CompositionEvent(type, { bubbles: true, data }));
      const tick = () => new Promise(resolve => setTimeout(resolve, 0));
      composition("compositionstart", "");
      composition("compositionupdate", "字");
      input.value += "字";
      await tick();
      composition("compositionend", "字");
      input.value += "x";
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: "x", inputType: "insertText", isComposing: false,
      }));
      composition("compositionstart", "");
      composition("compositionupdate", "文");
      input.value += "文";
      await tick();
      composition("compositionend", "文");
      await tick();
    });
    await expect.poll(stream).toBe(before + "字\x18文");
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

  // The composer owns bare Shift+Enter before CompositionHelper.keydown sees
  // it. Dispatch it in the compositionend turn so the custom LF must queue
  // behind xterm's zero-delay commit rather than overtaking it.
  {
    const before = stream();
    await textarea.evaluate(async el => {
      const input = el as HTMLTextAreaElement;
      const start = input.value.length;
      input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
      input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: "字" }));
      input.value = input.value.substring(0, start) + "字";
      await new Promise(resolve => setTimeout(resolve, 0));
      input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "字" }));
      const keydown = new KeyboardEvent("keydown", {
        bubbles: true, cancelable: true, key: "Enter", shiftKey: true,
      });
      Object.defineProperty(keydown, "keyCode", { value: 13 });
      input.dispatchEvent(keydown);
      input.dispatchEvent(new KeyboardEvent("keyup", { bubbles: true, key: "Enter", shiftKey: true }));
      await new Promise(resolve => setTimeout(resolve, 0));
    });
    await expect.poll(stream).toBe(before + "字\n");
  }
}
