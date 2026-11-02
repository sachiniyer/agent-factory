import { test, expect, type APIRequestContext, type Page } from "@playwright/test";
import { quietSession } from "./quiet-session";

// The agent-set attention request, end to end against the REAL daemon the harness
// boots on a throwaway home (#6284 theme A): the agent raises it over the same
// SetAttention route `af sessions attention --self` calls, the rail shows the dot,
// the session header shows the chip, and only the chip's × (or a submitted line)
// clears it. This runs against the embedded app only inside the sanctioned web
// testbox.

// The two specs that WANT pane activity to answer the request (a submitted line, a
// send-prompt) run on the shared `cat` session, where the stimulus is the thing under
// test. Every other spec raises a request and then interacts with it, so it runs on a
// quiet session where nothing but that interaction can clear it (#4217, #6465): on
// `cat`, the echo of the browser's terminal replies cleared the request under load
// before the Dismiss click.
const SESSION = process.env.AF_WEB_SESSION_A ?? "probe-a";
const quiet = quietSession("probe-quiet-attention", { perTest: true });

async function target(request: APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) => s.title === SESSION);
  expect(session?.id, `seeded session ${SESSION}`).toBeTruthy();
  return session.id as string;
}

async function attentionOf(request: APIRequestContext, id: string): Promise<{ set_at: string; reason?: string } | null> {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((s: { id: string }) => s.id === id)?.attention ?? null;
}

async function raise(request: APIRequestContext, id: string, reason: string) {
  const res = await (await request.post("/v1/SetAttention", { data: { id, reason } })).json();
  expect(res.error, "the agent's SetAttention is accepted").toBeFalsy();
}

// The harness's stand-in agent is `cat`, so the terminal replies the browser sends
// while a pane attaches echo back as pane activity, and a Running edge after idle
// clears a request by design. Let that settle before the agent raises one.
async function openSettled(page: Page, id: string) {
  await page.goto(`/#/session/${encodeURIComponent(id)}`);
  await expect(page.locator(".af-term-title")).toHaveText(SESSION);
  await page.waitForTimeout(2000);
}

test.describe.configure({ mode: "serial" });

test.afterEach(async ({ request }) => {
  // Leave no request behind for the specs that run after this one.
  // The quiet session is killed after each test, which takes its request with it.
  const id = await target(request);
  await request.post("/v1/SetAttention", { data: { id, clear: true } });
});

for (const width of [1280, 390]) {
  test(`an agent's request shows the dot and the header chip, and only the × dismisses it, at ${width}px`, async ({ page, request }) => {
    await page.setViewportSize({ width, height: 844 });
    const id = await quiet.id(request);
    await quiet.open(page, id);
    const chip = page.locator(".af-term-attention");
    await expect(chip, "no request, no chip").toBeHidden();

    await raise(request, id, "Pick option A or B");
    await expect(chip).toBeVisible();
    await expect(chip).toHaveAttribute("title", "Needs attention: Pick option A or B");
    // The rail's own dot is the glyph-only state; the row keeps no state text.
    const row = page.locator(".af-rail-list .af-row").filter({ hasText: quiet.name });
    await expect(row.locator(".af-dot-attention")).toHaveCount(1);

    // Looking at the session does not clear it. (Input into the pane is covered by
    // the daemon's isSubmittedInput test: a live stand-in agent reports Running on
    // any pane activity, and a Running edge after idle clears a request by design,
    // so a live pane cannot isolate the input rule.)
    await page.waitForTimeout(1000);
    await expect(chip).toBeVisible();
    expect(await attentionOf(request, id), "viewing the session leaves the request").not.toBeNull();
    // Nor does a click on the chip body.
    await chip.click({ position: { x: 3, y: 3 } });
    await expect(chip).toBeVisible();

    await page.getByRole("button", { name: "Dismiss attention request", exact: true }).click();
    await expect(chip).toBeHidden();
    await expect(row.locator(".af-dot-attention")).toHaveCount(0);
    expect(await attentionOf(request, id), "the × clears it on the daemon").toBeNull();
  });
}

test("a submitted line in the agent pane clears the request", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await target(request);
  await openSettled(page, id);
  await raise(request, id, "Answer in the pane");
  await expect(page.locator(".af-term-attention")).toBeVisible();
  await page.locator(".af-term-host").click();
  // A submitted line answers the request (the Running edge it causes would too;
  // the input rule itself is pinned by the daemon's isSubmittedInput test).
  await page.keyboard.type("ok");
  await page.keyboard.press("Enter");
  await expect(page.locator(".af-term-attention")).toBeHidden({ timeout: 15_000 });
  expect(await attentionOf(request, id)).toBeNull();
});

test("a user's send-prompt answers the request, with the chip and dot following", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await target(request);
  await openSettled(page, id);
  await raise(request, id, "Which port?");
  await expect(page.locator(".af-term-attention")).toBeVisible();
  // The route the web client and API callers use. A task delivery cannot be sent
  // this way (its provenance is daemon-internal), so that half is pinned by the
  // daemon's TestAttentionIsAnsweredByAPromptTheUserSubmits. The stand-in agent's
  // echo also produces a Running edge, so this proves the surface wiring, not
  // that the prompt alone did it.
  const sent = await (await request.post("/v1/SendPrompt", { data: { id, title: SESSION, prompt: "8080" } })).json();
  expect(sent.error, "the prompt is accepted").toBeFalsy();
  await expect(page.locator(".af-term-attention")).toBeHidden({ timeout: 15_000 });
  expect(await attentionOf(request, id)).toBeNull();
});

test("a dismissal names the request the chip showed, so a newer request survives it", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await quiet.id(request);
  await quiet.open(page, id);
  await raise(request, id, "First question");
  const first = await attentionOf(request, id);
  expect(first).not.toBeNull();
  await expect(page.locator(".af-term-attention")).toBeVisible();
  // The agent raises a newer request before the click lands.
  await new Promise((r) => setTimeout(r, 20));
  await raise(request, id, "Second question");
  const stale = await (await request.post("/v1/SetAttention", { data: { id, clear: true, set_at: first!.set_at } })).json();
  expect(stale.error).toBeFalsy();
  const kept = await attentionOf(request, id);
  expect(kept?.reason, "a stale clear leaves the newer request").toBe("Second question");
});

test("reduced motion stills the blink and keeps the dot a ring", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await quiet.id(request);
  await quiet.open(page, id);
  await raise(request, id, "Look at this");
  const dot = page.locator(".af-rail-list .af-row .af-dot-attention").first();
  await expect(dot).toBeVisible();
  // One evaluate for the whole read (#4217): locator.evaluate resolves the
  // element THEN evaluates — a row rebuild in between hands getComputedStyle a
  // detached node, which answers "" forever. Querying inside the page makes the
  // read atomic, and polling rides out the rebuild window like #6509's badge.
  const anim = () =>
    page.evaluate(() => {
      const d = document.querySelector(".af-rail-list .af-row .af-dot-attention");
      const b = document.querySelector(".af-term-attention-bell");
      return {
        dot: d ? getComputedStyle(d).animationName : null,
        bell: b ? getComputedStyle(b).animationName : null,
        ring: d ? getComputedStyle(d.querySelector(".af-icon") ?? d).outlineStyle : null,
      };
    });

  await page.emulateMedia({ reducedMotion: "no-preference" });
  await expect.poll(async () => (await anim()).dot).toBe("af-attention-blink");
  await expect.poll(async () => (await anim()).bell).toBe("af-attention-blink");

  await page.emulateMedia({ reducedMotion: "reduce" });
  await expect.poll(async () => (await anim()).dot, { message: "the rail dot holds still" }).toBe("none");
  await expect.poll(async () => (await anim()).bell, { message: "the chip bell holds still" }).toBe("none");
  expect((await anim()).ring, "a static ring keeps attention distinct from working").toBe("solid");
});
