import { expect, test, type APIRequestContext, type Page } from "@playwright/test";
import { decode, Op } from "../src/frame.js";

// #5500 — the adjustable, persisted terminal font size, proven end-to-end:
// the A−/A+ controls step the xterm pitch (and only that pitch), the daemon's
// own OpResize frames show the grid refitting, and the value the debounced save
// writes is what the FIRST resize of a reloaded page reports — no
// paint-at-default-then-jump attach.

/** The outgoing frames of every terminal /stream socket — the wire witness for
 *  grid changes and input, so the assertions read what the PTY is told rather
 *  than what the page believes it rendered. */
function frameStream(page: Page) {
  const frames: Array<ReturnType<typeof decode>> = [];
  page.on("websocket", socket => {
    if (!new URL(socket.url()).pathname.endsWith("/stream")) return;
    socket.on("framesent", ({ payload }) => {
      if (typeof payload === "string") return;
      frames.push(decode(payload));
    });
  });
  return {
    resizes: () => frames.filter(f => f.op === Op.Resize).map(f => ({ rows: f.rows, cols: f.cols })),
    inputs: () => frames.filter(f => f.op === Op.Input).map(f => Array.from(f.data)),
  };
}

async function setFont(request: APIRequestContext, key: string, value: string): Promise<void> {
  const resp = await request.post("/v1/SetConfigValue", { data: { key, value } });
  const body = await resp.json().catch(() => null);
  expect(resp.ok() && body?.error == null, `SetConfigValue ${key}=${value} → ${JSON.stringify(body)}`).toBe(true);
}

/** The leaf value as the daemon currently serves it — the same compact JSON
 *  the connect-time read hands the font controller. */
async function fontLeaf(request: APIRequestContext, leaf: string): Promise<number | null> {
  const resp = await request.post("/v1/GetConfig", { data: {} });
  const body = await resp.json();
  const web = (body?.data?.entries ?? []).find((e: { key: string }) => e.key === "web");
  if (!web) return null;
  const table = JSON.parse(web.value) as Record<string, number>;
  return typeof table[leaf] === "number" ? table[leaf] : null;
}

/** Attaches the seeded session's terminal through its deep link — the route
 *  a reload restores — rather than the rail row, which at phone width sits in
 *  a drawer that first has to be opened. */
async function attachSession(page: Page, request: APIRequestContext): Promise<void> {
  const resp = await request.post("/v1/Snapshot", { data: { repo_id: "" } });
  const body = await resp.json();
  const title = process.env.AF_WEB_SESSION_A ?? "probe-a";
  const found = (body?.data?.instances ?? []).find((s: { title: string }) => s.title === title);
  expect(found?.id, `session ${title} must be in the Snapshot`).toBeTruthy();
  await page.goto(`/#/session/${encodeURIComponent(found.id)}`);
  await expect(page.locator(".af-term-host .xterm").first()).toBeVisible();
}

test.afterEach(async ({ request }) => {
  // The fixture daemon's config is shared across specs — leave it on the
  // defaults so a later spec never inherits a stepped pitch.
  await request.post("/v1/SetConfigValue", { data: { key: "web.terminal_font_size_phone", value: "6.5" } });
  await request.post("/v1/SetConfigValue", { data: { key: "web.terminal_font_size_desktop", value: "13" } });
});

test("#5500 a configured desktop pitch is the FIRST OpResize's grid, never a jumped-to one", async ({ page, request }) => {
  await setFont(request, "web.terminal_font_size_desktop", "15");
  const stream = frameStream(page);
  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);
  const configured = stream.resizes().at(-1)!;
  // A paint-at-default-then-jump attach would send the default grid first and
  // a corrective one after the config lands — two different dims. Settle and
  // require every resize to carry the same grid.
  await page.waitForTimeout(800);
  const dims = [...new Set(stream.resizes().map(r => `${r.cols}x${r.rows}`))];
  expect(dims, "the attach must not paint at the default then resize again").toHaveLength(1);

  // Control: the same attach at the default pitch must report a DIFFERENT
  // first grid — proving the 15px attach's first resize was genuinely the
  // configured pitch and not a coincidence of identical geometry.
  await setFont(request, "web.terminal_font_size_desktop", "13");
  const before = stream.resizes().length;
  await page.reload();
  await expect(page.locator(".af-term-host .xterm").first()).toBeVisible();
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(before);
  const defaulted = stream.resizes().at(-1)!;
  expect(defaulted.cols, "the 15px attach's first grid must differ from the default's").not.toEqual(configured.cols);
});

test("#5500 desktop A+ refits into a smaller grid, persists, and survives a reload", async ({ page, request }) => {
  const stream = frameStream(page);
  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);
  const initial = stream.resizes().at(-1)!;

  const zoom = page.locator(".af-term-head").first()
    .getByRole("button", { name: "Increase terminal text size", exact: true });
  await expect(zoom).toBeVisible();
  const inputsBefore = stream.inputs().length;
  await zoom.click();
  await expect.poll(() => stream.resizes().length,
    { message: "A+ must refit the terminal into a coarser grid" }).toBeGreaterThan(1);
  const stepped = stream.resizes().at(-1)!;
  expect(stepped.cols).toBeLessThan(initial.cols);
  expect(stepped.rows).toBeLessThanOrEqual(initial.rows);
  // The button is display state, not input — no PTY bytes may leave for it.
  expect(stream.inputs().length).toBe(inputsBefore);

  // The debounced save carries the settled 14 (default 13 + one step).
  await expect.poll(() => fontLeaf(request, "terminal_font_size_desktop"),
    { message: "the stepped desktop size must persist through SetConfigValue" }).toBe(14);
  expect(await fontLeaf(request, "terminal_font_size_phone"),
    "a desktop step must not touch the phone leaf").toBe(6.5);

  // The reload attach's first resize already carries the saved pitch.
  const before = stream.resizes().length;
  await page.reload();
  await expect(page.locator(".af-term-host .xterm").first()).toBeVisible();
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(before);
  expect(stream.resizes()[before].cols, "reload's first OpResize is the persisted pitch").toBe(stepped.cols);
});

test("#5500 a config read landing behind a queued save must not stomp the stepped size", async ({ page, request }) => {
  // Codex P2 on #5501: the stale-read guard used to end with the debounce
  // timer, but the save it armed can still be queued or in flight — and the
  // first save's own refetch then delivers the PRE-second-write value. The
  // guard must cover a class until its saves complete.
  //
  // The wire choreography makes that window deterministic: every manifest read
  // after attach is held at the route, and the SECOND desktop write is held at
  // the route too — so when the first save's refetch is released, the file
  // provably still serves the first stepped value while the second write is
  // still outstanding. A guard that ended with the debounce would refit the
  // terminal back down here, emitting an OpResize this spec counts.
  const stream = frameStream(page);
  let holdGets = false;
  const heldGets: Array<() => void> = [];
  const heldPosts: Array<() => void> = [];
  let fontPosts = 0;
  await page.route("**/v1/GetConfig", async route => {
    if (holdGets) {
      await new Promise<void>(res => heldGets.push(res));
    }
    await route.fallback();
  });
  await page.route("**/v1/SetConfigValue", async route => {
    const key = (route.request().postDataJSON() as { key?: string } | null)?.key;
    if (key === "web.terminal_font_size_desktop" && ++fontPosts >= 2) {
      await new Promise<void>(res => heldPosts.push(res));
    }
    await route.fallback();
  });

  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);
  holdGets = true;

  const zoom = page.locator(".af-term-head").first()
    .getByRole("button", { name: "Increase terminal text size", exact: true });
  const before = stream.resizes().length;

  // First step (13→14): its save completes the POST, then its refetch parks at
  // the held route — the queue entry stays open around it.
  await zoom.click();
  await expect.poll(() => heldGets.length,
    { message: "the first save's refetch must reach the held route" }).toBeGreaterThanOrEqual(1);

  // Second step (14→15): its debounce fires while the first save's round trip
  // is still open, so the write queues behind it (or, on a build without the
  // guard, stalls at the held route). Either way the daemon still serves 14.
  await zoom.click();
  await expect.poll(() => stream.resizes().length,
    { message: "both steps must refit before the stale read is released" }).toBe(before + 2);
  await page.waitForTimeout(600); // the second debounce fires — its save is queued
  const resizesAfterSteps = stream.resizes().length;

  // The first save's refetch lands the pre-second-write 14 — and must be
  // ignored, because the second save is still outstanding.
  heldGets.forEach(release => release());
  holdGets = false;
  await expect.poll(() => heldPosts.length,
    { message: "the queued second save must reach the route once the first save settles" }).toBe(1);
  await page.waitForTimeout(700); // give a wrong refit time to emit its OpResize
  expect(stream.resizes().length,
    "the stale read must not refit the terminal back to the pre-step grid").toBe(resizesAfterSteps);

  // Now the queued write lands: the daemon ends at 15 and still no refit fires
  // — the settled size and the served size already agree.
  heldPosts.forEach(release => release());
  await expect.poll(() => fontLeaf(request, "terminal_font_size_desktop"),
    { message: "the queued save must still write the settled 15" }).toBe(15);
  await page.waitForTimeout(1200);
  expect(stream.resizes().length,
    "the echo of the queued save must not refit either").toBe(resizesAfterSteps);
});

test("#5500 a failed font save surfaces on the toast, not silently", async ({ page, request }) => {
  // Codex P2 on #5501: a dotted leaf key has no manifest row to carry a save
  // error, so a refused font write used to vanish into configStatus while the
  // stepped size kept displaying as though it had persisted. The failure must
  // reach the shared toast.
  await page.route("**/v1/SetConfigValue", async route => {
    const key = (route.request().postDataJSON() as { key?: string } | null)?.key;
    if (typeof key === "string" && key.startsWith("web.terminal_font_size_")) {
      return route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ data: null, error: { message: "config write refused in test" } }),
      });
    }
    return route.fallback();
  });

  const stream = frameStream(page);
  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);

  const zoom = page.locator(".af-term-head").first()
    .getByRole("button", { name: "Increase terminal text size", exact: true });
  const before = stream.resizes().length;
  await zoom.click();

  // The size still applies locally — display state is honest about what it is —
  // but the refusal reaches the user on the toast rather than landing on a
  // config row nothing renders.
  await expect.poll(() => stream.resizes().length,
    { message: "the stepped size still applies locally while its save fails" }).toBe(before + 1);
  const toast = page.locator(".af-toast.af-toast-show");
  await expect(toast, "a refused font save must surface a visible error").toBeVisible();
  await expect(toast).toContainText("not saved");
  await expect(toast).toContainText("config write refused in test");
  expect(await fontLeaf(request, "terminal_font_size_desktop"),
    "a refused write must leave the served value as it was").toBe(13);
});

test("#5500 a failed connect-time config read retries until the persisted pitch lands", async ({ page, request }) => {
  // Codex P2 on #5501: a failed GetConfig at connect used to degrade to the
  // compiled defaults for the whole session — nothing scheduled another read,
  // so the terminal ran an un-persisted pitch until an unrelated action
  // refetched. The client must reconcile in the background on a bounded
  // backoff and apply whatever the first landed read serves.
  await setFont(request, "web.terminal_font_size_desktop", "15");
  const stream = frameStream(page);
  let getCalls = 0;
  // The connect read AND the first retry fail; the second retry — one backoff
  // step later — is released to the daemon. A one-shot retry would never land.
  await page.route("**/v1/GetConfig", async route => {
    if (++getCalls <= 2) {
      return route.fulfill({
        status: 500,
        contentType: "application/json",
        body: JSON.stringify({ data: null, error: { message: "config read unavailable in test" } }),
      });
    }
    await route.fallback();
  });

  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);
  const defaulted = stream.resizes().at(-1)!;

  // The loop is silent by contract: a per-retry toast would keep reporting a
  // failure the reconcile is about to recover from — and a persistent one it
  // already recovered from would read as a lie. The connect read and the
  // first retry both fail inside this window; neither may toast.
  const toast = page.locator(".af-toast.af-toast-show");
  await page.waitForTimeout(1_800);
  await expect(toast, "a failed background retry must not surface an error").toBeHidden();

  // The landed read applies the persisted 15 through the normal commit: the
  // font controller refits and a corrective OpResize leaves on the coarser
  // grid — the same convergence an on-time read would have produced before
  // the first attach.
  await expect.poll(() => stream.resizes().length,
    { message: "the retried read must refit the terminal onto the persisted pitch" }).toBeGreaterThan(1);
  const landed = stream.resizes().at(-1)!;
  expect(landed.cols, "the landed read must apply the persisted 15px pitch").not.toEqual(defaulted.cols);
  // And the witness on the wire: two retries behind the failed connect read.
  await expect.poll(() => getCalls).toBeGreaterThanOrEqual(3);
  // Recovery leaves nothing behind — the retry-owned failures were never
  // shown, so there is nothing stale for the success to have to clear.
  await expect(toast, "a recovered reconcile must leave no stale error").toBeHidden();
});

test("#5500 phone A+ steps the phone pitch, persists it, and leaves an armed modifier armed", async ({ page, request }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  // Seed a non-default desktop pitch so an untouched leaf is itself evidence.
  await setFont(request, "web.terminal_font_size_desktop", "14");
  const stream = frameStream(page);
  await page.goto("/");
  await attachSession(page, request);
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(0);
  const initial = stream.resizes().at(-1)!;

  const bar = page.locator(".af-terminal-keybar:visible");
  await expect(bar).toBeVisible();
  // Arm Ctrl on the primary page first: the zoom keys must not consume it —
  // the next real key has to see the modifier still armed.
  const ctrl = bar.getByRole("button", { name: "Ctrl", exact: true });
  await ctrl.click();
  await expect(ctrl).toHaveAttribute("data-state", "once");
  // The zoom pair rides the keybar's arrows page.
  await bar.getByRole("button", { name: "Arrows", exact: true }).click();
  const inputsBefore = stream.inputs().length;
  await bar.getByRole("button", { name: "Increase terminal text size", exact: true }).click();
  await expect.poll(() => stream.resizes().length,
    { message: "the phone A+ must refit the terminal into a coarser grid" }).toBeGreaterThan(1);
  const stepped = stream.resizes().at(-1)!;
  expect(stepped.cols).toBeLessThan(initial.cols);
  expect(stream.inputs().length, "A+ must send no bytes to the PTY").toBe(inputsBefore);

  // The phone step is 0.5: default 6.5 → 7, and the desktop leaf is untouched.
  await expect.poll(() => fontLeaf(request, "terminal_font_size_phone"),
    { message: "the stepped phone size must persist through SetConfigValue" }).toBe(7);
  expect(await fontLeaf(request, "terminal_font_size_desktop")).toBe(14);

  // Back on the primary page the modifier is still armed — and the next real
  // key proves it by arriving as ^A, not "a".
  await bar.getByRole("button", { name: "More keys", exact: true }).click();
  await expect(ctrl, "A+ must not consume an armed modifier").toHaveAttribute("data-state", "once");
  await page.keyboard.type("a");
  await expect.poll(() => stream.inputs().at(-1)).toEqual([0x01]);

  // A reloaded phone attach opens on the saved pitch.
  const before = stream.resizes().length;
  await page.reload();
  await expect(page.locator(".af-term-host .xterm").first()).toBeVisible();
  await expect.poll(() => stream.resizes().length).toBeGreaterThan(before);
  expect(stream.resizes()[before].cols, "phone reload's first OpResize is the persisted pitch").toBe(stepped.cols);
});

test("#5500 a malformed or out-of-range font write is refused", async ({ request }) => {
  for (const bad of ["big", "40", "nan"]) {
    const resp = await request.post("/v1/SetConfigValue", { data: { key: "web.terminal_font_size_phone", value: bad } });
    const body = await resp.json().catch(() => null);
    expect(resp.ok() === false || body?.error != null,
      `SetConfigValue web.terminal_font_size_phone=${bad} must be refused, got ${JSON.stringify(body)}`).toBe(true);
  }
  // A whole-table write validates every member before anything lands.
  const resp = await request.post("/v1/SetConfigValue", {
    data: { key: "web", value: `{"terminal_font_size_phone":8,"terminal_font_size_desktop":99}` },
  });
  const body = await resp.json().catch(() => null);
  expect(resp.ok() === false || body?.error != null).toBe(true);
  expect(await fontLeaf(request, "terminal_font_size_phone"),
    "a refused write must leave the served value as it was").toBe(6.5);
});
