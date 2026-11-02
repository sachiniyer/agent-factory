import { expect, test, type Page } from "@playwright/test";
import { seededTabs } from "./seeded-tabs.js";

// #6588: on WebKit an uncaught `TypeError: undefined is not an object
// (evaluating 'this._renderer.value.dimensions')` fires on session switch,
// off-screen tab activation and band-crossing refits — xterm is asked for its
// render dimensions after its renderer has already been disposed. Never seen
// in Chromium runs of the same flows: WebKit's event ordering around element
// teardown is what exposes it.
//
// probe-a's tab roster is shared with the whole suite: the helper closes
// whatever this spec adds (#4217).

test.use({ browserName: "webkit", launchOptions: { args: [] } });

const sessionA = process.env.AF_WEB_SESSION_A ?? "probe-a";
const sessionB = process.env.AF_WEB_SESSION_B ?? "probe-b";
const roster = seededTabs();

async function sessionId(request: Page["request"], title: string): Promise<string> {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const found = snap.data.instances.find((s: { title: string }) => s.title === title);
  expect(found?.id, `seeded session ${title}`).toBeTruthy();
  return found.id as string;
}

async function openSession(page: Page, id: string): Promise<void> {
  await page.goto(`/#/session/${encodeURIComponent(id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await page.locator(".af-term-host .xterm").first().waitFor();
}

/** In-page session switch through the hash router — the same teardown a rail
 *  click drives, without depending on the shared roster's rail state. */
async function routeTo(page: Page, id: string): Promise<void> {
  await page.evaluate((sid) => {
    window.location.hash = `#/session/${sid}`;
  }, id);
}

test("#6588: refit/dispose orderings produce no uncaught error", async ({ page, request }) => {
  expect(page.context().browser()?.browserType().name(), "this spec must run WebKit").toBe("webkit");
  const idA = await sessionId(request, sessionA);
  const idB = await sessionId(request, sessionB);

  const pageErrors: string[] = [];
  page.on("pageerror", (e) => pageErrors.push(`${e.message}\n${e.stack}`));

  await page.setViewportSize({ width: 1280, height: 800 });
  await openSession(page, idA);
  const base = await roster.enter(page);

  // Several tabs so the strip can overflow and a switch rebinds panes.
  for (let i = 0; i < 3; i++) {
    const tabs = page.locator(".af-tabbar .af-tab");
    const have = await tabs.count();
    await page.locator(".af-term-new-shell").filter({ visible: true }).first().click();
    await expect(tabs).toHaveCount(have + 1);
  }

  // 1. In-app session switching: each route change tears down this session's
  // terminals and mounts the other's inside the same page.
  for (let i = 0; i < 8; i++) {
    await routeTo(page, idB);
    await page.locator(".af-term-host .xterm").first().waitFor();
    await routeTo(page, idA);
    await page.locator(".af-term-host .xterm").first().waitFor();
  }

  // 2. Band crossings: live-resize a desktop window through the 768px phone
  // band while panes hide/show and terminals are torn down/rebound.
  for (const width of [700, 900, 400, 1200, 760, 820, 500, 1280]) {
    await page.setViewportSize({ width, height: 800 });
    await page.waitForTimeout(100);
  }

  // 3. Rapid activation churn: click through every tab back-to-back inside one
  // task — each activation disposes the previous terminal while its socket is
  // still connecting, the exact teardown the sightings follow.
  await page.evaluate(() => {
    const tabs = Array.from(document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab"));
    for (let round = 0; round < 4; round++) {
      for (const tab of tabs) {
        tab.click();
      }
    }
  });
  await page.waitForTimeout(500);

  // 4. The same churn against the router: session A↔B with no settle between.
  // A task between assignments keeps each flip a real navigation — two writes
  // inside one task can coalesce into a single hashchange.
  for (let i = 0; i < 10; i++) {
    await routeTo(page, idB);
    await routeTo(page, idA);
  }
  await page.waitForTimeout(500);

  // 5. A band crossing DURING the churn: resize through 768 while activations
  // are still landing.
  await Promise.all([
    page.evaluate(() => {
      const tabs = Array.from(document.querySelectorAll<HTMLElement>(".af-tabbar .af-tab"));
      for (const tab of tabs) tab.click();
    }),
    page.setViewportSize({ width: 390, height: 844 }),
  ]);
  await page.waitForTimeout(300);
  await page.setViewportSize({ width: 1280, height: 800 });
  await page.waitForTimeout(300);

  // Off-screen tab activation through the whole roster one at a time.
  const tabCount = await page.locator(".af-tabbar .af-tab").count();
  for (let i = 0; i < tabCount; i++) {
    const tab = page.locator(".af-tabbar .af-tab").nth(i);
    await tab.scrollIntoViewIfNeeded();
    await tab.click();
    await page.waitForTimeout(60);
  }

  console.log("PAGEERRORS", JSON.stringify(pageErrors, null, 2));
  expect(base).toBeGreaterThanOrEqual(1);
  expect(pageErrors, "no terminal may be fitted or resized after it is disposed").toEqual([]);
});
