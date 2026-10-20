import { assertPhoneComposition } from "./phone-composition.js";
import { expect, test } from "@playwright/test";
import { assertPhoneBarModifiers, assertPhoneCustomInterruptIdentity, assertPhoneStaleRecovery, leavePageAndCleanup,
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
    await assertPhoneCustomInterruptIdentity(page, stream);
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

test("#5426 phone keybar Paste reads the clipboard inside the tap's activation-bearing event, once", async ({
  browser, request,
}, testInfo) => {
  // A phone, not a narrow desktop window: hasTouch puts Chromium into real
  // touch emulation, and for a finger the tap's activation-bearing event is
  // pointerup — pointerdown grants transient activation only to a mouse. A
  // stubbed readText records the activation state it was invoked under and the
  // event dispatch it ran inside; a real clipboard can answer neither.
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 812 },
    hasTouch: true,
    isMobile: true,
  });
  const page = await ctx.newPage();
  const stream = phoneInputStream(page);
  const token = "af-5426-keybar-paste";
  type PasteCall = { active: boolean; during: string | null };
  const calls = () => page.evaluate(() =>
    (window as unknown as { __pasteCalls: PasteCall[] }).__pasteCalls);
  await page.addInitScript((t) => {
    const readCalls: { active: boolean; during: string | null }[] = [];
    const w = window as unknown as { __pasteCalls: typeof readCalls; __keybarEvent?: string };
    w.__pasteCalls = readCalls;
    // The capture-phase listener runs ahead of the keybar's own target-phase
    // handlers, so the name stashed here is the event a readText call was
    // invoked under; the bubble-phase one clears it when dispatch ends.
    for (const type of ["pointerdown", "pointerup", "click", "touchstart", "touchend"]) {
      window.addEventListener(type, () => { w.__keybarEvent = type; }, true);
      window.addEventListener(type, () => { w.__keybarEvent = undefined; });
    }
    Object.defineProperty(navigator, "clipboard", {
      configurable: true,
      value: {
        readText: () => {
          readCalls.push({ active: navigator.userActivation.isActive, during: w.__keybarEvent ?? null });
          return Promise.resolve(t);
        },
      },
    });
  }, token);

  const response = await request.post("/v1/CreateSession", { data: {
    repo_path: process.env.AF_MOCK_REPO, title_base: "probe-keybar-paste",
  } });
  const created = await response.json();
  expect(created.error).toBeFalsy();
  const id = created.data.instance.id;
  expect(id).toBeTruthy();
  try {
    await page.goto(`/#/session/${encodeURIComponent(id)}`);
    await page.locator(".af-pane-host .xterm").first().click();
    await expect(page.locator(".af-pane-host .xterm-helper-textarea").first()).toBeFocused();
    const bar = page.locator(".af-terminal-keybar:visible");
    await expect(bar).toBeVisible();
    const paste = bar.getByRole("button", { name: "Paste", exact: true });

    // A single finger tap. The read must happen inside pointerup — the event
    // whose dispatch grants a touch its transient activation — not pointerdown,
    // where #5426 ran it and activation-strict engines always refused it.
    await paste.tap();
    await expect.poll(() => calls().then(c => c.length),
      { message: "one tap must read the clipboard exactly once — its click must not read again" }).toBe(1);
    const readCalls = await calls();
    expect(readCalls).toHaveLength(1);
    expect(readCalls[0].during, "the read must run inside the tap's activation-bearing event").toBe("pointerup");
    expect(readCalls[0].active, "the read must see transient user activation").toBe(true);
    await expect.poll(stream).toContain(token);
    expect(stream().split(token).length - 1).toBe(1);
    // No fallback sheet, and the canceled press never stole the terminal's focus.
    await expect(page.locator(".af-modal-backdrop")).toHaveCount(0);
    await expect(page.locator(".af-pane-host .xterm-helper-textarea").first()).toBeFocused();

    // Assistive activation is a detail:0 click no pointer produced — no
    // pointerdown or pointerup precedes it — and it must still reach readText.
    await paste.evaluate(el =>
      el.dispatchEvent(new PointerEvent("click", { bubbles: true, detail: 0, pointerType: "" })));
    await expect.poll(() => calls().then(c => c.length)).toBe(2);
    expect((await calls())[1]?.during).toBe("click");

    // A tap that turns into a scroll — a drag starting on Paste — must not
    // paste: the release pairs to its press by pointerId and must land on the
    // key, and a pointercancel (the browser claiming the gesture) withdraws it.
    const cdp = await ctx.newCDPSession(page);
    const box = (await paste.boundingBox())!;
    const px = box.x + box.width / 2;
    const py = box.y + box.height / 2;
    await cdp.send("Input.dispatchTouchEvent", { type: "touchStart", touchPoints: [{ x: px, y: py }] });
    for (const step of [1, 2, 3, 4]) {
      await cdp.send("Input.dispatchTouchEvent",
        { type: "touchMove", touchPoints: [{ x: px, y: py - step * 30 }] });
    }
    await cdp.send("Input.dispatchTouchEvent", { type: "touchEnd", touchPoints: [] });
    await page.waitForTimeout(250);
    expect(await calls(), "a drag off the Paste key must not read the clipboard").toHaveLength(2);
    expect(stream().split(token).length - 1, "the drag must not paste either").toBe(2);

    // A key that is not Paste still sends at pointerdown — and its tap still
    // sends once, even though the canceled press zeroes its click's detail too.
    // Esc is the harmless leg; ^C is the one the review named (it also ends the
    // cat fixture, so it stays last).
    const before = stream();
    await bar.getByRole("button", { name: "Esc", exact: true }).tap();
    await expect.poll(stream).toBe(before + "\x1b");
    await page.waitForTimeout(250);
    expect(stream(), "a tap's pointer-derived click must not send a second byte").toBe(before + "\x1b");
    const beforeInterrupt = stream();
    await bar.getByRole("button", { name: "Interrupt (^C)", exact: true }).tap();
    await expect.poll(stream).toBe(beforeInterrupt + "\x03");
    await page.waitForTimeout(250);
    expect(stream(), "^C must reach the PTY exactly once per tap").toBe(beforeInterrupt + "\x03");
  } finally {
    try {
      await page.screenshot({ path: testInfo.outputPath("phone-keybar-paste.png") });
      console.log("#5426 PTY input:", JSON.stringify(stream()));
    } finally {
      await leavePageAndCleanup(page, async () => {
        const killed = await request.post("/v1/KillSession", { data: { id } });
        expect((await killed.json()).error).toBeFalsy();
        await ctx.close();
      });
    }
  }
});
