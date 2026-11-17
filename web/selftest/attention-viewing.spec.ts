import { test, expect, type APIRequestContext } from "@playwright/test";
import { quietSession } from "./quiet-session";

// The #7434 property, end to end: an attention request is answered only by the
// user's input or the agent's work — never by a repaint caused by VIEWING.
// The session's fake agent prints a line wider than any viewer grid and then
// sleeps (the probe-quiet-view branch of fake-agent.sh), so every refit is
// GUARANTEED to reflow real pane bytes: the status poll sees churn, and for an
// agent with no working indicator that churn is a Running edge — the edge that
// used to answer and clear the request before the user ever saw it.
//
// The quiet session's pane runs no named agent (ResolvedAgent ""), so it
// stands in for codex/aider/gemini/every custom program: the daemon-side axis
// is pinned per agent in daemon/attention_reflow_test.go; this spec pins the
// whole wire — browser refit → OpResize → resize-window → poll → attention.
const quiet = quietSession("probe-quiet-view", { perTest: true });

async function instanceOf(request: APIRequestContext, id: string) {
  const snapshot = await (await request.post("/v1/Snapshot", { data: {} })).json();
  return snapshot.data.instances.find((s: { id: string }) => s.id === id);
}

const LIVE_READY = 2;

test("a viewer's refit reflow must not answer the agent's attention request", async ({ page, request }) => {
  await page.setViewportSize({ width: 1280, height: 844 });
  const id = await quiet.id(request);
  await quiet.open(page, id);
  const chip = page.locator(".af-term-attention");

  // The bug shape needs the request live while the agent is idle: at Ready the
  // raised request inherits the satisfied idle spell, so the NEXT Running edge
  // is what used to answer it (session/attention.go).
  await expect
    .poll(async () => (await instanceOf(request, id))?.liveness, { timeout: 15_000, message: "the quiet agent must be observed idle before the request is raised" })
    .toBe(LIVE_READY);
  const res = await (await request.post("/v1/SetAttention", { data: { id, reason: "Pick option A or B" } })).json();
  expect(res.error).toBeFalsy();
  await expect(chip).toBeVisible();

  // The refit: the browser sends OpResize, the daemon applies resize-window,
  // tmux rewraps the 200-column line, and the next poll reads the changed
  // capture. On master that churn cleared the request here — before the user
  // ever scrolled to it. The reflow-attributed frame must move the liveness
  // without carrying work evidence.
  //
  // The Running edge that lands on liveness is ~one status-poll cycle: under
  // load a sampler reads before it and after it and never during (#7477). The
  // same observation's DURABLE record is last_pane_churn_at — the poll stamps
  // it on every Updated capture, reflow-attributed included
  // (daemon/manager_status.go); it moves only forward, and nothing in this
  // spec clears it (only a prompt send or a runtime replacement does —
  // session/idle_reason.go). Baseline it BEFORE the refit, then require an
  // advance: if nothing churned, this spec still proves nothing.
  const churnBefore = (await instanceOf(request, id))?.last_pane_churn_at ?? null;
  await page.setViewportSize({ width: 390, height: 844 });
  await expect
    .poll(
      async () => {
        const churn = (await instanceOf(request, id))?.last_pane_churn_at ?? null;
        return churn !== null && churn !== churnBefore;
      },
      {
        timeout: 15_000,
        message: "the refit's reflow must reach the poll as fresh pane churn — if nothing churned, this spec proves nothing",
      },
    )
    .toBe(true);

  // Well past the poll cycle that used to clear it, the request still stands.
  await page.waitForTimeout(4000);
  await expect(chip, "a repaint caused by viewing never answers the request").toBeVisible();
  expect((await instanceOf(request, id))?.attention ?? null, "the daemon still holds the request").not.toBeNull();

  // And refitting back — the second resize the same way — is equally inert.
  // The same durable observation proves THIS refit also churned the pane: a
  // request surviving a refit that never landed proves nothing either.
  const churnBeforeSecond = (await instanceOf(request, id))?.last_pane_churn_at ?? null;
  await page.setViewportSize({ width: 1280, height: 844 });
  await expect
    .poll(
      async () => {
        const churn = (await instanceOf(request, id))?.last_pane_churn_at ?? null;
        return churn !== null && churn !== churnBeforeSecond;
      },
      { timeout: 15_000, message: "the second refit's reflow must also reach the poll as fresh pane churn" },
    )
    .toBe(true);
  await page.waitForTimeout(4000);
  await expect(chip).toBeVisible();
  expect((await instanceOf(request, id))?.attention ?? null).not.toBeNull();
});
