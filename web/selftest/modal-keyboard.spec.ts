import { expect, test, type Page } from "@playwright/test";

// #6431: with a soft keyboard up on a phone, a form's primary action (Create) stays
// inside the visual viewport and no field sits under the pinned footer.
//
// Chromium cannot open a real soft keyboard, so it is approximated the way a phone
// reports one: an init script shrinks visualViewport.height (the layout viewport
// stays full height, as on Chrome Android with interactive-widget=resizes-visual and
// on iOS Safari) and fires resize on visualViewport and window.

async function installKeyboard(page: Page) {
  await page.addInitScript(() => {
    const vv = window.visualViewport!;
    const w = window as unknown as { __kb: number; __setKb: (n: number) => void };
    w.__kb = 0;
    Object.defineProperty(vv, "height", { get: () => window.innerHeight - w.__kb, configurable: true });
    Object.defineProperty(vv, "offsetTop", { get: () => 0, configurable: true });
    w.__setKb = (n: number) => {
      w.__kb = n;
      vv.dispatchEvent(new Event("resize"));
      window.dispatchEvent(new Event("resize"));
      vv.dispatchEvent(new Event("scroll"));
    };
  });
}
const setKb = (page: Page, n: number) => page.evaluate((v) => (window as unknown as { __setKb: (n: number) => void }).__setKb(v), n);
const rect = (page: Page, sel: string) => page.locator(sel).first().evaluate((el) => { const r = el.getBoundingClientRect(); return { top: r.top, bottom: r.bottom }; });

async function sessionId(request: import("@playwright/test").APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((i: { title: string }) => i.title === "probe-b").id as string;
}

for (const [width, height, kb] of [[390, 844, 336], [320, 568, 260]] as const) {
  test(`#6431 New session keeps Create inside the visual viewport with the keyboard up at ${width}x${height}`, async ({ browser, request }, testInfo) => {
    const ctx = await browser.newContext({ viewport: { width, height }, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    try {
      await installKeyboard(page);
      await page.goto(`/#/session/${encodeURIComponent(await sessionId(request))}`);
      await expect(page.locator(".af-term-title")).toHaveText("probe-b");
      await page.getByRole("button", { name: /^Toggle sessions/ }).click();
      await page.locator(".af-rail-new").click();
      const card = page.locator(".af-modal-card");
      await expect(card).toBeVisible();
      const create = page.getByRole("button", { name: "Create", exact: true });
      const vvHeight = height - kb;

      await setKb(page, kb);
      await page.waitForTimeout(500);
      await page.getByLabel("Title").focus();
      // Open the taller form and focus its last field, the hardest case for a
      // pinned footer: the field must scroll above it, not under it.
      await page.locator("summary", { hasText: "Defaults" }).click().catch(() => {});
      await page.getByLabel("Prompt").focus();
      await page.waitForTimeout(400);

      const c = await rect(page, ".af-modal-card");
      expect(c.top, "card starts inside the visual viewport").toBeGreaterThanOrEqual(-0.5);
      expect(c.bottom, "card ends above the keyboard").toBeLessThanOrEqual(vvHeight + 0.5);
      const cb = (await create.boundingBox())!;
      expect(cb.y, "Create is inside the visual viewport").toBeGreaterThanOrEqual(-0.5);
      expect(cb.y + cb.height, "Create is above the keyboard").toBeLessThanOrEqual(vvHeight + 0.5);
      const foot = await rect(page, ".af-modal-foot");
      const prompt = await rect(page, "textarea");
      expect(prompt.bottom, "the focused Prompt is not under the pinned footer").toBeLessThanOrEqual(foot.top + 0.5);
      await page.screenshot({ path: testInfo.outputPath(`modal-kb-${width}.png`) });

      // Create is tappable where it sits.
      await expect(create).toBeEnabled();
      await create.click({ trial: true });

      // The published insets follow the keyboard.
      expect(await page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--af-vv-bottom"))).toBe(`${kb}px`);
      // Keyboard closes: the layout returns to the full viewport.
      await setKb(page, 0);
      await expect.poll(() => page.evaluate(() => getComputedStyle(document.documentElement).getPropertyValue("--af-vv-bottom"))).toBe("0px");
      const closed = await rect(page, ".af-modal-backdrop");
      expect(Math.round(closed.bottom)).toBe(height);
    } finally {
      await ctx.close();
    }
  });
}
