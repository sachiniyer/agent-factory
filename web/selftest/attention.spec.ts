import { test, expect, type APIRequestContext } from "@playwright/test";
import { echoSession, quietSession } from "./quiet-session";
import { openAfterInitialResync } from "./initial-resync";

// The agent-set attention request, end to end against the REAL daemon the harness
// boots on a throwaway home (#6284 theme A): the agent raises it over the same
// SetAttention route `af sessions attention --self` calls, the rail shows the dot,
// the session header shows the chip, and only the chip's × (or a submitted line)
// clears it. This runs against the embedded app only inside the sanctioned web
// testbox.

// Every spec that raises a request and then interacts with it runs on a quiet
// session where nothing but that interaction can clear it (#4217, #6465): on
// `cat`, the echo of the browser's terminal replies cleared the request under
// load before the Dismiss click. The two specs that WANT pane activity to answer
// the request (a submitted line, a send-prompt) cannot use the shared `cat`
// session either (#7432): the same stray-reply echo can clear the request before
// it renders, or churn before the idle record arms so the send's own answer
// cannot count. They run on `probe-echo`, which echoes submitted lines only —
// the only pane churn in those tests is the answer they themselves submit.
const SESSION = process.env.AF_WEB_SESSION_A ?? "probe-a";
const quiet = quietSession("probe-quiet-attention", { perTest: true });

async function target(request: APIRequestContext) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  const session = snapshot.data.instances.find((s: { title: string }) => s.title === SESSION);
  expect(session?.id, `seeded session ${SESSION}`).toBeTruthy();
  return session.id as string;
}

async function attentionOf(
  request: APIRequestContext,
  id: string,
): Promise<{ set_at: string; reason?: string; seen_idle?: boolean; idle_since?: string } | null> {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((s: { id: string }) => s.id === id)?.attention ?? null;
}

async function raise(request: APIRequestContext, id: string, reason: string) {
  const res = await (await request.post("/v1/SetAttention", { data: { id, reason } })).json();
  expect(res.error, "the agent's SetAttention is accepted").toBeFalsy();
}

// The daemon's own idle verdict, observed through Snapshot — never a sleep. A
// request raised while the pane is Ready inherits the idle spell (IdleSince zero
// reads as long enough, session/attention.go:123-128), so the spec's OWN next
// pane activity — the echo of the line or prompt it submits — is the Running
// edge that answers it. Raised while the pane is still churning, the same edge
// lands before the idle record arms and cannot answer (the 11-11 sighting).
//
// "ready" alone cannot be the gate: a session is born LiveReady before its agent
// emits anything (session/transition_table.go), and the daemon's 1s capture
// sweep can still be one tick away from noticing the startup burst — its
// Running edge would land AFTER the raise and answer a request the spec never
// meant to answer. The gate is "ready AND the burst is already inside a
// captured frame": last_pane_churn_at is proof the sweep saw the output and
// idled after it, while a session whose first capture already contained the
// burst never stamps it — so the fallback is created_at ~two sweeps old at
// worst, long enough that even a serial sweep stretched by the session count
// has captured the pane at least twice and no post-baseline `updated` can
// still be in flight.
async function paneSettled(request: APIRequestContext, id: string) {
  await expect
    .poll(
      async () => {
        const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
        const inst = snapshot.data.instances.find((s: { id: string; liveness_name?: string; last_pane_churn_at?: string; created_at?: string }) => s.id === id);
        if (inst?.liveness_name !== "ready") return false;
        return inst.last_pane_churn_at != null || Date.now() - Date.parse(inst.created_at!) >= 6_000;
      },
      { timeout: 15_000, message: "the pane's startup output is inside a captured frame and it settles to ready" },
    )
    .toBe(true);
}

// The raised request is armed to be answered by the spec's own next Running
// edge: seen_idle with no idle_since (raised at Ready — the inherited spell
// reads as long enough) or an idle_since at least attentionAnswerIdle old
// (session/attention.go:21). If the raise ever lands while the pane is still
// Running, seen_idle waits for the next Ready edge and the 10s arm plays out
// inside the poll — the send's answer still counts.
async function attentionArmed(request: APIRequestContext, id: string) {
  await expect
    .poll(
      async () => {
        const a = await attentionOf(request, id);
        if (a?.seen_idle !== true) return false;
        return a.idle_since == null || Date.now() - Date.parse(a.idle_since) >= 10_000;
      },
      { timeout: 15_000, message: "the raised request is armed before the answer is sent" },
    )
    .toBe(true);
}

test.describe.configure({ mode: "serial" });

test.afterEach(async ({ request }) => {
  // Leave no request behind for the specs that run after this one.
  // Each dedicated session is killed after its test, which takes its request
  // with it; this clears probe-a in case anything ever lands there.
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

test.describe("answered through the pane", () => {
  // The two specs that WANT pane activity to answer the request run on a
  // dedicated `probe-echo` session (#7432): `stty -echo` plus a read loop that
  // echoes newline-terminated lines only, so the browser terminal's attach
  // replies can never churn the pane the way `cat`'s byte echo did — the ONLY
  // Running edge in these tests is the answer they themselves submit.
  const echo = echoSession("probe-echo-attention", { perTest: true });

  test("a submitted line in the agent pane clears the request", async ({ page, request }) => {
    await page.setViewportSize({ width: 1280, height: 844 });
    const id = await echo.id(request);
    await openAfterInitialResync(page, () => echo.open(page, id));
    await paneSettled(request, id);
    await raise(request, id, "Answer in the pane");
    await attentionArmed(request, id);
    await expect(page.locator(".af-term-attention")).toBeVisible();
    await page.locator(".af-term-host").click();
    // A submitted line answers the request (the Running edge its echo causes
    // would too; the input rule itself is pinned by the daemon's
    // isSubmittedInput test).
    await page.keyboard.type("ok");
    await page.keyboard.press("Enter");
    await expect(page.locator(".af-term-attention")).toBeHidden({ timeout: 15_000 });
    expect(await attentionOf(request, id)).toBeNull();
  });

  test("a user's send-prompt answers the request, with the chip and dot following", async ({ page, request }) => {
    await page.setViewportSize({ width: 1280, height: 844 });
    const id = await echo.id(request);
    await openAfterInitialResync(page, () => echo.open(page, id));
    await paneSettled(request, id);
    await raise(request, id, "Which port?");
    await attentionArmed(request, id);
    await expect(page.locator(".af-term-attention")).toBeVisible();
    // The route the web client and API callers use. A task delivery cannot be
    // sent this way (its provenance is daemon-internal), so that half is pinned
    // by the daemon's TestAttentionIsAnsweredByAPromptTheUserSubmits. The clear
    // lands as the keyed send-prompt clear on a proven-delivered verdict
    // (daemon/manager_sessions.go), or as the echo's Running edge answering the
    // armed request when the delivery verdict is sent-unverified — either way
    // the answering act is this test's own send, and nothing else can clear it.
    const sent = await (await request.post("/v1/SendPrompt", { data: { id, title: echo.name, prompt: "8080" } })).json();
    expect(sent.error, "the prompt is accepted").toBeFalsy();
    await expect(page.locator(".af-term-attention")).toBeHidden({ timeout: 15_000 });
    expect(await attentionOf(request, id)).toBeNull();
  });
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
