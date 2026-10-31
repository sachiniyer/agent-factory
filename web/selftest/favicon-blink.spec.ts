import { test, expect, type APIRequestContext, type Page } from "@playwright/test";

// The needs-attention blink on the favicon (#6284 theme A), against the real
// daemon the harness boots: while a session in the selected project asks for the
// user the tab icon swaps its tile and mark colors every second, it holds still
// under reduced motion, and it returns to the plain mark when the request is
// dismissed. Runs inside the sanctioned web testbox.

const SESSION = process.env.AF_WEB_SESSION_A ?? "probe-a";

async function target(request: APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) => s.title === SESSION);
  expect(session?.id, `seeded session ${SESSION}`).toBeTruthy();
  return session.id as string;
}

// The stand-in agent echoes terminal replies, which a Running edge after idle
// turns into an answer; let the attach settle before the agent asks.
async function openSettled(page: Page, id: string) {
  await page.goto(`/#/session/${encodeURIComponent(id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(SESSION);
  await page.waitForTimeout(2000);
}

const iconHref = (page: Page) => page.evaluate(() => document.querySelector<HTMLLinkElement>('link[rel="icon"][type="image/svg+xml"]')?.getAttribute("href") ?? "");

// Distinct hrefs seen across window ms, polled fast.
async function hrefsOver(page: Page, window: number) {
  const seen = new Set<string>();
  const end = Date.now() + window;
  while (Date.now() < end) {
    seen.add(await iconHref(page));
    await page.waitForTimeout(100);
  }
  return seen;
}

test.describe.configure({ mode: "serial" });

test.afterEach(async ({ request }) => {
  const id = await target(request);
  await request.post("/v1/SetAttention", { data: { id, clear: true } });
});

test("the favicon blinks while a session needs attention, and stops when it is dismissed", async ({ page, request }) => {
  await page.emulateMedia({ reducedMotion: "no-preference" });
  const id = await target(request);
  await openSettled(page, id);
  const quiet = await hrefsOver(page, 2500);
  expect(quiet.size, "no request, one steady icon").toBe(1);

  const res = await (await request.post("/v1/SetAttention", { data: { id, reason: "Pick A or B" } })).json();
  expect(res.error).toBeFalsy();
  await expect(page.locator(".af-term-attention")).toBeVisible();
  const blinking = await hrefsOver(page, 3500);
  expect(blinking.size, "the tile and mark colors swap").toBeGreaterThanOrEqual(2);

  await page.getByRole("button", { name: "Dismiss attention request", exact: true }).click();
  await expect(page.locator(".af-term-attention")).toBeHidden();
  const after = await hrefsOver(page, 2500);
  expect(after.size, "dismissed, one steady icon again").toBe(1);
  expect(blinking.has([...after][0]), "and it is one of the two frames the blink alternated, the plain mark").toBe(true);
});

test("under reduced motion the favicon holds one still frame", async ({ page, request }) => {
  await page.emulateMedia({ reducedMotion: "reduce" });
  const id = await target(request);
  await openSettled(page, id);
  const plain = await iconHref(page);
  const res = await (await request.post("/v1/SetAttention", { data: { id, reason: "Look" } })).json();
  expect(res.error).toBeFalsy();
  await expect(page.locator(".af-term-attention")).toBeVisible();
  const still = await hrefsOver(page, 3500);
  expect(still.size, "no blink").toBe(1);
  expect(still.has(plain), "the still frame is the attention frame, not the plain mark").toBe(false);

  // Reduced motion turned off mid-request: the blink starts without a reload.
  await page.emulateMedia({ reducedMotion: "no-preference" });
  const moving = await hrefsOver(page, 3500);
  expect(moving.size).toBeGreaterThanOrEqual(2);
});
