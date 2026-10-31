import { expect, test, type Locator } from "@playwright/test";

// #6407: at 320px the phone keybar keeps 44px touch targets and no label is
// truncated. Before, the narrow state shrank keys to a 40px floor and ellipsised
// "Paste", "Arrows" and "More keys". Real daemon, touch emulation.

async function sessionId(request: import("@playwright/test").APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((i: { title: string }) => i.title === "probe-b").id as string;
}

async function assertRow(row: Locator, viewportWidth: number) {
  const buttons = row.locator("button");
  const n = await buttons.count();
  expect(n).toBe(7);
  for (let i = 0; i < n; i++) {
    const b = buttons.nth(i);
    const label = ((await b.textContent()) ?? "").trim();
    const box = (await b.boundingBox())!;
    expect(box.width, `${label} is at least 44px wide`).toBeGreaterThanOrEqual(43.99);
    expect(box.height, `${label} is at least 44px tall`).toBeGreaterThanOrEqual(43.99);
    expect(box.x + box.width, `${label} stays inside the viewport`).toBeLessThanOrEqual(viewportWidth + 0.01);
    const clipped = await b.evaluate((el) => el.scrollWidth > el.clientWidth);
    expect(clipped, `${label} is not truncated`).toBe(false);
  }
}

for (const width of [320, 390]) {
  test(`#6407 phone keybar keeps 44px targets and untruncated labels at ${width}px`, async ({ browser, request }, testInfo) => {
    const ctx = await browser.newContext({ viewport: { width, height: 568 }, hasTouch: true, isMobile: true });
    const page = await ctx.newPage();
    try {
      await page.goto(`/#/session/${encodeURIComponent(await sessionId(request))}`);
      await page.locator(".af-pane-host .xterm").first().click();
      const bar = page.locator(".af-terminal-keybar");
      await expect(bar).toBeVisible();
      await assertRow(bar.locator(".af-keybar-row").nth(0), width);
      await bar.getByRole("button", { name: "Arrows", exact: true }).click();
      await assertRow(bar.locator(".af-keybar-row").nth(1), width);
      // A locked modifier (double tap) must not push its key out of the row.
      await bar.getByRole("button", { name: "More keys", exact: true }).click();
      const ctrl = bar.getByRole("button", { name: "Ctrl", exact: true });
      await ctrl.dblclick();
      await assertRow(bar.locator(".af-keybar-row").nth(0), width);
      await page.screenshot({ path: testInfo.outputPath(`keybar-${width}.png`) });
    } finally {
      await ctx.close();
    }
  });
}
