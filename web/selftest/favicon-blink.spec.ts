import { test, expect, type Page } from "@playwright/test";
import { quietSession } from "./quiet-session";

// The needs-attention blink on the favicon (#6284 theme A), against the real
// daemon the harness boots: while a session in the selected project asks for the
// user the tab icon swaps its tile and mark colors every second, it holds still
// under reduced motion, and it returns to the plain mark when the request is
// dismissed. Runs inside the sanctioned web testbox.

// A dedicated session whose agent prints nothing after its ready marker (see the
// probe-quiet branch of fake-agent.sh in web-selftest-entry.sh). The shared probe-a
// session runs `cat`, which echoes whatever the browser terminal sends back; that
// output is a Running edge, and a Running edge after the agent went idle is exactly
// what clears an attention request (session/attention.go) — so under load it cleared
// the request in the middle of the blink sample and the Dismiss click found nothing
// to click (#6465). With no stray output possible, only the dismiss can clear it.
const quiet = quietSession("probe-quiet");
const target = quiet.id;
const openSettled = quiet.open;

const iconHref = (page: Page) => page.evaluate(() => document.querySelector<HTMLLinkElement>('link[rel="icon"][type="image/svg+xml"]')?.getAttribute("href") ?? "");

// Distinct hrefs seen across window ms, polled fast. With attentionHeld, every poll
// also asserts the request is still showing, so a clear that is not the dismiss fails
// HERE, naming itself, instead of surfacing later as a Dismiss click that times out.
async function hrefsOver(page: Page, window: number, opts: { attentionHeld?: boolean } = {}) {
  const seen = new Set<string>();
  const end = Date.now() + window;
  while (Date.now() < end) {
    seen.add(await iconHref(page));
    if (opts.attentionHeld && (await page.locator(".af-term-attention").count()) === 0) {
      throw new Error("the attention request was cleared by something other than Dismiss during the sample (stray agent output?)");
    }
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
  const blinking = await hrefsOver(page, 3500, { attentionHeld: true });
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
  const still = await hrefsOver(page, 3500, { attentionHeld: true });
  expect(still.size, "no blink").toBe(1);
  expect(still.has(plain), "the still frame is the attention frame, not the plain mark").toBe(false);

  // Reduced motion turned off mid-request: the blink starts without a reload.
  await page.emulateMedia({ reducedMotion: "no-preference" });
  const moving = await hrefsOver(page, 3500);
  expect(moving.size).toBeGreaterThanOrEqual(2);
});

// Control: the guard is live and names what it catches. Whatever takes the chip away
// other than the Dismiss button -- the stray pane activity the issue suspects (a Running
// edge after idle, session/attention.go), a prompt, a clear from elsewhere -- must end the
// sample with the error below rather than leave a Dismiss click to time out. The daemon-side
// causes depend on the pane's idle history and cannot be summoned deterministically in this
// fixture (driving output, keystrokes, send-prompt and an API clear at it did not remove the
// chip reliably), so the control removes the chip from the page mid-sample: the observable
// the guard watches is the same.
test("control: a clear that is not the dismiss fails the sample for the right reason", async ({ page, request }) => {
  await page.emulateMedia({ reducedMotion: "no-preference" });
  const id = await target(request);
  await openSettled(page, id);
  const res = await (await request.post("/v1/SetAttention", { data: { id, reason: "Pick A or B" } })).json();
  expect(res.error).toBeFalsy();
  await expect(page.locator(".af-term-attention")).toBeVisible();
  const sampling = hrefsOver(page, 10_000, { attentionHeld: true });
  const assertion = expect(sampling).rejects.toThrow(/cleared by something other than Dismiss/);
  await page.waitForTimeout(1500);
  await page.evaluate(() => document.querySelector(".af-term-attention")?.remove());
  await assertion;
});
