import { expect, type Page } from "@playwright/test";
import { decode, Op } from "../src/frame.js";

/** Observe the actual outgoing binary PTY stream, not echoed terminal content. */
export function phoneInputStream(page: Page): () => string {
  let input = "";
  page.on("websocket", socket => {
    if (!new URL(socket.url()).pathname.endsWith("/stream")) return;
    socket.on("framesent", ({ payload }) => {
      if (typeof payload === "string") return;
      const frame = decode(payload);
      if (frame.op === Op.Input) input += new TextDecoder().decode(frame.data);
    });
  });
  return () => input;
}

export async function assertPhoneKeybar(page: Page, stream: () => string): Promise<void> {
  const bar = page.locator(".af-terminal-keybar:visible");
  const textarea = page.locator(".af-pane-host .xterm-helper-textarea").first();
  await expect(bar).toBeVisible();
  const geometry = await bar.evaluate(el => {
    const rect = el.getBoundingClientRect();
    return { fits: rect.left >= 0 && rect.right <= innerWidth,
      lastFits: [...el.querySelectorAll("button")].filter(b => b.offsetHeight > 0).at(-1)!.getBoundingClientRect().right <= innerWidth,
      oneRow: rect.height === 44,
      targets: [...el.querySelectorAll("button")].filter(b => b.offsetHeight > 0).every(b => b.offsetWidth >= 44 && b.offsetHeight >= 44),
      aboveKeyboard: Math.abs(rect.bottom - (visualViewport!.offsetTop + visualViewport!.height)) < 2 };
  });
  expect(geometry).toEqual({ fits: true, lastFits: true, oneRow: true, targets: true, aboveKeyboard: true });
  let before = stream();
  await bar.getByRole("button", { name: "Arrows", exact: true }).click();
  await expect(textarea).toBeFocused();
  await bar.getByRole("button", { name: "↑", exact: true }).click();
  await expect.poll(stream).toBe(before + "\x1b[A");
  await bar.getByRole("button", { name: "More keys", exact: true }).click();
  await expect(textarea).toBeFocused();
  before = stream();
  await bar.getByRole("button", { name: "Interrupt (^C)", exact: true }).click();
  await expect(textarea).toBeFocused();
  await expect.poll(stream).toBe(before + "\x03");
  before = stream();
  await bar.getByRole("button", { name: "Ctrl", exact: true }).click();
  await page.keyboard.insertText("c"); // input/beforeinput, without keydown
  await expect.poll(stream).toBe(before + "\x03");
  await expect(bar.getByRole("button", { name: "Ctrl", exact: true })).toHaveAttribute("aria-pressed", "false");
  before = stream();
  await bar.getByRole("button", { name: "Ctrl", exact: true }).click();
  await textarea.evaluate(el => {
    const input = el as HTMLTextAreaElement;
    input.dispatchEvent(new CompositionEvent("compositionstart", { bubbles: true, data: "" }));
    input.dispatchEvent(new InputEvent("beforeinput", { bubbles: true, data: "c", inputType: "insertCompositionText", isComposing: true }));
    input.value += "c";
    input.dispatchEvent(new CompositionEvent("compositionupdate", { bubbles: true, data: "c" }));
    input.dispatchEvent(new InputEvent("input", { bubbles: true, data: "c", inputType: "insertCompositionText", isComposing: true }));
    input.dispatchEvent(new CompositionEvent("compositionend", { bubbles: true, data: "c" }));
  });
  await expect.poll(stream).toBe(before + "\x03");
  await expect(textarea).toBeFocused();
  // Locked modifiers must neither double-send physical keypress + input pairs,
  // nor release on a soft-keyboard character.
  const ctrl = bar.getByRole("button", { name: "Ctrl", exact: true });
  await ctrl.dblclick({ delay: 80 });
  await expect(ctrl).toHaveAttribute("data-state", "locked");
  before = stream();
  await page.keyboard.type("c");
  await page.keyboard.insertText("c");
  await expect.poll(stream).toBe(before + "\x03\x03");
  await expect(ctrl).toHaveAttribute("data-state", "locked");
  await ctrl.click();
  // Browser emulation does not open an OS keyboard. Exercise both visual viewport
  // signals with the keyboard-reduced height and a panned viewport explicitly.
  await page.evaluate(() => {
    Object.defineProperty(visualViewport, "height", { configurable: true, value: 500 });
    Object.defineProperty(visualViewport, "offsetTop", { configurable: true, value: 20 });
    visualViewport!.dispatchEvent(new Event("resize"));
    visualViewport!.dispatchEvent(new Event("scroll"));
  });
  await expect.poll(() => page.evaluate(() => {
    const bar = document.querySelector(".af-terminal-keybar")!.getBoundingClientRect();
    const screen = document.querySelector(".af-pane-host .xterm-screen")!.getBoundingClientRect();
    return Math.abs(bar.bottom - 520) < 2 && screen.bottom <= bar.top + 1;
  })).toBe(true);
  await page.evaluate(() => {
    Reflect.deleteProperty(visualViewport!, "height");
    Reflect.deleteProperty(visualViewport!, "offsetTop");
    visualViewport!.dispatchEvent(new Event("resize"));
  });
  await page.keyboard.press("Control+]");
  await expect(page.locator(".af-terminal-keybar")).toHaveCount(0);
  await textarea.focus();
  await expect(bar).toBeVisible();
}
