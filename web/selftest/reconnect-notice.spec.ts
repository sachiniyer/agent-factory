import { expect, test, type Page } from "@playwright/test";

// #6430: when the event or PTY socket stays down past a short grace, the session
// header (and the rail on desktop) say "Reconnecting…" with the time of the next
// retry, statically, and clear on reconnect. What the client does with keystrokes
// typed while disconnected: it QUEUES them (PendingInput, 64 KiB, ordered) and
// delivers on reconnect — asserted here so a regression to silent dropping fails.
//
// The drop is staged at the WebSocket constructor in an init script: CDP offline
// and setBlockedURLs do not close sockets that are already open.

async function stage(page: Page) {
  await page.addInitScript(() => {
    const Native = window.WebSocket;
    const socks: WebSocket[] = [];
    const w = window as unknown as { __wsBlock: boolean; __wsDrop: () => number };
    w.__wsBlock = false;
    class Staged extends Native {
      constructor(url: string | URL, protocols?: string | string[]) {
        super(url, protocols);
        if (w.__wsBlock) queueMicrotask(() => { try { this.close(); } catch { /* closed */ } });
        socks.push(this);
      }
    }
    window.WebSocket = Staged as unknown as typeof WebSocket;
    w.__wsDrop = () => { let n = 0; for (const s of socks) if (s.readyState === 1) { s.close(4000, "staged"); n++; } return n; };
  });
}
const block = (page: Page, on: boolean) => page.evaluate((v) => { (window as unknown as { __wsBlock: boolean }).__wsBlock = v; }, on);
const drop = (page: Page) => page.evaluate(() => (window as unknown as { __wsDrop: () => number }).__wsDrop());

async function sessionId(request: import("@playwright/test").APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((i: { title: string }) => i.title === "probe-b").id as string;
}

const CASES = [
  { name: "phone 390x844", viewport: { width: 390, height: 844 }, phone: true },
  { name: "phone 320x568", viewport: { width: 320, height: 568 }, phone: true },
  { name: "desktop 1280x800", viewport: { width: 1280, height: 800 }, phone: false },
];

for (const c of CASES) {
  test(`#6430 a dropped socket shows a static Reconnecting… with a retry time, clears on reconnect, and typed input survives (${c.name})`, async ({ browser, request }, testInfo) => {
    const ctx = await browser.newContext({ viewport: c.viewport, hasTouch: c.phone, isMobile: c.phone, reducedMotion: "reduce" });
    const page = await ctx.newPage();
    try {
      await stage(page);
      await page.goto(`/#/session/${encodeURIComponent(await sessionId(request))}`);
      await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");
      const term = page.locator(".af-pane-host .xterm").first();
      await term.click();
      const chip = page.locator(".af-term-conn");
      await expect(chip, "connected: no chip").toBeHidden();

      await block(page, true);
      expect(await drop(page)).toBeGreaterThan(0);
      await expect(page.locator(".af-app")).toHaveAttribute("data-live", "reconnecting");
      // Inside the grace period nothing is drawn: a reconnect that finishes in a
      // blink must not flash a chip.
      await page.waitForTimeout(600);
      await expect(chip, "inside the 2s grace").toBeHidden();

      // Typed while disconnected: the client queues it (never silently dropped).
      await page.keyboard.type("typed-while-down\n");

      await expect(chip).toBeVisible({ timeout: 5000 });
      await expect(chip).toContainText("Reconnecting…");
      // The time of the next retry appears (the backoff has a scheduled attempt).
      await expect.poll(async () => (await chip.textContent()) ?? "", { timeout: 12_000 }).toMatch(/Reconnecting….*\d+s/);
      await expect(chip).toHaveAttribute("aria-label", /^Reconnecting…( retry in \d+s)?$/);
      await page.screenshot({ path: testInfo.outputPath(`reconnecting-${c.viewport.width}.png`) });
      // Words, not a hover tooltip; and it sits in the header, not over the terminal.
      const box = (await chip.boundingBox())!;
      const term0 = (await term.boundingBox())!;
      expect(box.y + box.height, "chip is in the header, above the terminal").toBeLessThanOrEqual(term0.y + 1);
      if (c.phone) {
        expect(box.x + box.width).toBeLessThanOrEqual(c.viewport.width);
        // The compact header does not grow (that would shift the terminal under the
        // keybar), and the words are not ellipsised away.
        expect((await page.locator(".af-appbar").boundingBox())!.height).toBeLessThanOrEqual(60);
        expect(await chip.evaluate((el) => `${el.scrollWidth}<=${el.clientWidth}`)).toMatch(/^(\d+)<=(\d+)$/);
        const [sw, cw] = (await chip.evaluate((el) => [el.scrollWidth, el.clientWidth]));
        expect(sw, `chip text (${sw}px) fits its slot (${cw}px)`).toBeLessThanOrEqual(cw);
      }
      else await expect(page.locator(".af-rail-conn")).toHaveText(/^Reconnecting…/);
      // Static under reduced motion: no animation on the chip or anything in it.
      const animations = await chip.evaluate((el) => el.getAnimations({ subtree: true }).length);
      expect(animations, "no animation on the chip").toBe(0);

      await block(page, false);
      await expect(chip, "clears on reconnect").toBeHidden({ timeout: 25_000 });
      await expect(page.locator(".af-app")).toHaveAttribute("data-live", "open");
      if (!c.phone) await expect(page.locator(".af-rail-conn")).toBeHidden();
      // The queued keystrokes reached the PTY: the stand-in `cat` echoes them back.
      await expect.poll(() => page.locator(".af-pane-host .xterm-rows").first().innerText(), { timeout: 15_000 })
        .toContain("typed-while-down");
    } finally {
      await ctx.close();
    }
  });
}
