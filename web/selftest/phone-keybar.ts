import { expect, type Page } from "@playwright/test";
import { decode, Op } from "../src/frame.js";

interface NavigablePage {
  goto(url: string): Promise<unknown>;
}

export async function leavePageAndCleanup(page: NavigablePage, cleanup: () => Promise<void>): Promise<void> {
  try {
    await page.goto("about:blank");
  } finally {
    await cleanup();
  }
}

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

/** Reproduce stale xterm keydown state and verify its textarea-based 229 deletion. */
export async function assertPhoneStaleRecovery(page: Page, stream: () => string): Promise<void> {
  const textarea = page.locator(".af-pane-host .xterm-helper-textarea").first();
  const ctrl = page.locator(".af-terminal-keybar:visible").getByRole("button", { name: "Ctrl", exact: true });
  const before = stream();
  const recovered = await textarea.evaluate(el => {
    const input = el as HTMLTextAreaElement;
    const key = (type: string, keyCode: number, name: string) => {
      const event = new KeyboardEvent(type, { bubbles: true, cancelable: true, key: name });
      Object.defineProperty(event, "keyCode", { value: keyCode });
      input.dispatchEvent(event);
    };
    // Reproduce xterm's stale _keyDownSeen after focus is lost without keyup.
    key("keydown", 17, "Control");
    input.blur();
    input.focus();
    for (const [letter, beforeData, inputData] of [
      ["a", null, "a"], ["b", "b", "b"], ["字", null, null],
    ] as const) {
      const beforeInput = new InputEvent("beforeinput", {
        bubbles: true, cancelable: true, composed: true, data: beforeData,
        inputType: "insertText", isComposing: true,
      });
      if (input.dispatchEvent(beforeInput)) input.value += letter;
      input.dispatchEvent(new InputEvent("input", {
        bubbles: true, composed: true, data: inputData, inputType: "insertText", isComposing: true,
      }));
    }
    return input.value;
  });
  expect(recovered).toBe("ab字");
  await expect.poll(stream).toBe(before + "ab字");

  await ctrl.click();
  await expect(ctrl).toHaveAttribute("data-state", "once");
  await textarea.evaluate(async el => {
    const input = el as HTMLTextAreaElement;
    const key = (type: string, keyCode: number, name: string) => {
      const event = new KeyboardEvent(type, { bubbles: true, cancelable: true, key: name });
      Object.defineProperty(event, "keyCode", { value: keyCode });
      input.dispatchEvent(event);
    };
    key("keydown", 229, "Unidentified");
    input.value = input.value.slice(0, -1);
    await new Promise(resolve => setTimeout(resolve, 0));
    key("keyup", 229, "Unidentified");
  });
  await expect.poll(stream).toBe(before + "ab字\x08");
  await expect(ctrl).toHaveAttribute("data-state", "off");
  await page.keyboard.insertText("a");
  await expect.poll(stream).toBe(before + "ab字\x08a");
}

/** The focused regression also runs with the ordinary selftest cat fixture. */
export async function assertPhoneBarModifiers(page: Page, stream: () => string): Promise<void> {
  const bar = page.locator(".af-terminal-keybar:visible");
  const ctrl = bar.getByRole("button", { name: "Ctrl", exact: true });
  let before = stream();
  await ctrl.click();
  await page.keyboard.press("Enter");
  await expect.poll(stream).toBe(before + "\r");
  await expect(ctrl).toHaveAttribute("data-state", "off");
  await page.keyboard.insertText("a");
  await expect.poll(stream).toBe(before + "\ra");
  for (const [modifier, bytes] of [["Ctrl", "\n"], ["Alt", "\x1b\n"]] as const) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await page.keyboard.press("Shift+Enter");
    await expect.poll(stream).toBe(before + bytes);
    await expect(button).toHaveAttribute("data-state", "off");
    await page.keyboard.insertText("a");
    await expect.poll(stream).toBe(before + bytes + "a");
  }
  for (const [modifier, chord, bytes] of [
    ["Alt", "Control+x", "\x1b\x18"],
    ["Ctrl", "Alt+x", "\x1b\x18"],
  ] as const) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await page.keyboard.press(chord);
    await expect.poll(stream, { message: `${modifier} + ${chord} reaches the PTY` }).toBe(before + bytes);
    await expect(button).toHaveAttribute("data-state", "off");
    await page.keyboard.insertText("a");
    await expect.poll(stream).toBe(before + bytes + "a");
  }
  for (const [modifier, key, bytes] of [
    ["Alt", "Enter", "\x1b\r"],
    ["Alt", "Backspace", "\x1b\x7f"],
    ["Ctrl", "Backspace", "\x08"],
    ["Ctrl", "Shift+Tab", "\x1b[Z"],
    ["Alt", "Shift+Tab", "\x1b[Z"],
  ] as const) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await page.keyboard.press(key);
    await expect.poll(stream).toBe(before + bytes);
    await expect(button).toHaveAttribute("data-state", "off");
    await page.keyboard.insertText("a");
    await expect.poll(stream).toBe(before + bytes + "a");
  }
  for (const [modifier, chord, bytes] of [
    ["Ctrl", "Alt+ArrowUp", "\x1b[1;7A"],
    ["Alt", "Control+ArrowRight", "\x1b[1;7C"],
  ] as const) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await page.keyboard.press(chord);
    await expect.poll(stream).toBe(before + bytes);
    await expect(button).toHaveAttribute("data-state", "off");
  }
  before = stream();
  await ctrl.click();
  await page.keyboard.press("ArrowUp");
  await expect.poll(stream).toBe(before + "\x1b[1;5A");
  await expect(ctrl).toHaveAttribute("data-state", "off");
  await page.keyboard.insertText("a");
  await expect.poll(stream).toBe(before + "\x1b[1;5Aa");
  before = stream();
  await ctrl.click();
  await page.keyboard.press("Shift+ArrowUp");
  await expect.poll(stream).toBe(before + "\x1b[1;6A");
  await expect(ctrl).toHaveAttribute("data-state", "off");
  await page.keyboard.insertText("a");
  await expect.poll(stream).toBe(before + "\x1b[1;6Aa");
  before = stream();
  const alt = bar.getByRole("button", { name: "Alt", exact: true });
  await alt.click();
  await page.keyboard.press("Escape");
  await expect.poll(stream).toBe(before + "\x1b\x1b");
  await expect(alt).toHaveAttribute("data-state", "off");
  await page.keyboard.insertText("a");
  await expect.poll(stream).toBe(before + "\x1b\x1ba");
  for (const [modifier, key, bytes] of [
    ["Ctrl", "Home", "\x1b[1;5H"],
    ["Alt", "End", "\x1b[1;3F"],
    ["Ctrl", "Delete", "\x1b[3;5~"],
    ["Alt", "F1", "\x1b[1;3P"],
  ] as const) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await page.keyboard.press(key);
    await expect.poll(stream).toBe(before + bytes);
    await expect(button).toHaveAttribute("data-state", "off");
  }
  // Navigation between rows is not a keypress and must retain the one-shot.
  for (const [modifier, arrow, bytes] of [
    ["Ctrl", "↑", "\x1b[1;5A"], ["Alt", "←", "\x1b[1;3D"],
  ]) {
    before = stream();
    const button = bar.getByRole("button", { name: modifier, exact: true });
    await button.click();
    await expect(button).toHaveAttribute("data-state", "once");
    await bar.getByRole("button", { name: "Arrows", exact: true }).click();
    await bar.getByRole("button", { name: arrow, exact: true }).click();
    await bar.getByRole("button", { name: "More keys", exact: true }).click();
    await expect(button).toHaveAttribute("data-state", "off");
    await expect(button).toHaveAttribute("aria-description", "Double tap to lock");
    await expect(page.locator(".af-pane-host .xterm-helper-textarea").first()).toBeFocused();
    await page.keyboard.type("ls");
    await expect.poll(stream).toBe(before + bytes + "ls");
  }
  await ctrl.dblclick({ delay: 80 });
  before = stream();
  await bar.getByRole("button", { name: "Arrows", exact: true }).click();
  await bar.getByRole("button", { name: "↑", exact: true }).click();
  await bar.getByRole("button", { name: "More keys", exact: true }).click();
  await expect.poll(stream).toBe(before + "\x1b[1;5A");
  await expect(ctrl).toHaveAttribute("data-state", "locked");
  await page.keyboard.insertText("x");
  await expect.poll(stream).toBe(before + "\x1b[1;5A\x18");
  await expect(ctrl).toHaveAttribute("data-state", "locked");
  await ctrl.click();
  await bar.getByRole("button", { name: "Alt", exact: true }).click();
  await bar.getByRole("button", { name: "Tab", exact: true }).click();
  await page.keyboard.insertText("z");
  await expect.poll(stream).toBe(before + "\x1b[1;5A\x18\x1b\tz");
  await expect(bar.getByRole("button", { name: "Alt", exact: true })).toHaveAttribute("data-state", "off");
}

/** Exercise the custom interrupt only after every probe that needs the cat fixture. */
export async function assertPhoneCustomInterruptIdentity(page: Page, stream: () => string): Promise<void> {
  const bar = page.locator(".af-terminal-keybar:visible");
  const ctrl = bar.getByRole("button", { name: "Ctrl", exact: true });
  // The input-effects witness deliberately finishes on the arrows row.
  await bar.getByRole("button", { name: "More keys", exact: true }).click();
  const before = stream();
  await ctrl.click();
  await page.keyboard.press("Control+c");
  await expect.poll(stream, { message: "sticky Ctrl + physical Ctrl+C reaches the PTY" }).toBe(before + "\x03");
  // Ctrl+C is custom-handled before xterm can fire onKey. Its explicit marker
  // must retain the physically held Ctrl identity, so redundant sticky Ctrl
  // remains armed. Sending ETX kills the cat fixture, hence this is the final
  // stream assertion in the spec rather than part of the reusable matrix.
  await expect(ctrl).toHaveAttribute("data-state", "once");
}
