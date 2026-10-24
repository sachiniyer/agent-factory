// Hidden-client grid-ownership guard — the counterpart proof to #2347's reclaim.
//
// A stranded client's scheduled fit is delivered by a frame OR a 250ms timer, so
// it still lands on a document-hidden page (whose rAF is dead). That timer makes
// one property worth pinning explicitly: a hidden page is where a reclaim is
// DELIVERED, but it is not an activation — no user is present — so it must never
// write the shared PTY grid. If it could, a parked tab would steal the grid out
// from under a live visible peer (or bounce it between two parked pages).
// sendResize refuses the wire write while hidden; the page still refits locally
// so xterm's DOM is honest, and the first fit after the page shows again
// converges the daemon — the divergence between the local grid and
// lastRows/lastCols is itself the owed-write marker.
//
// Two tabs of one context, both subscribed to one PTY, prove it end to end:
//
//   1. a tall peer strands a 375x667 tab on the peer's 2000px grid;
//   2. while the first tab is document-hidden, a drawer dispatch refits it — its
//      own rows drop to the local grid (the timer twin delivered the reclaim)
//      but the peer's rows do NOT move: no OpResize ever left the hidden page,
//      or the daemon would have echoed it into the peer's xterm;
//   3. fronting the first tab flushes the owed write: the daemon learns its
//      grid and the peer is stomped to it — the legitimate last-writer-wins
//      activation #2347's machinery provides.
//
// The same spec run against a master binary shows the other half of the story:
// master delivers nothing while hidden (its only vehicle was a starved rAF), so
// the property held vacuously at the price of the strand — the peer stays
// untouched AND the hidden client's rows never drop, failing leg 2's local
// reclaim. That run is the A/B comparison; this spec passes only on the fix.

import { expect, type Locator, type Page, test } from "@playwright/test";

test("a hidden client reclaims locally but never writes the shared grid", async ({ browser }) => {
  const afBin = process.env.AF_BIN;
  const mockRepo = process.env.AF_MOCK_REPO;
  test.skip(!afBin || !mockRepo, "AF_BIN/AF_MOCK_REPO are set only by web-selftest-entry.sh");
  const { execFileSync } = await import("node:child_process");
  const af = (...args: string[]): void => {
    execFileSync(afBin as string, ["--repo", mockRepo as string, ...args], { stdio: "pipe" });
  };
  const sessionName = "hidden-client-guard";
  af("sessions", "create", "--name", sessionName, "--program", "claude");

  const pinRealFixtureProject = async (page: Page): Promise<void> => {
    await page.addInitScript((fixtureRepo) => {
      if (window.top !== window) return;
      try {
        localStorage.setItem("af-project", fixtureRepo);
      } catch {
        // Sandboxed documents can deny storage; the real app document does not.
      }
    }, mockRepo as string);
  };
  const open = async (page: Page): Promise<void> => {
    await pinRealFixtureProject(page);
    await page.goto("/");
    await expect(page.locator(".af-app")).toBeVisible();
  };
  const attach = async (page: Page): Promise<void> => {
    await page.locator(".af-nav-toggle").click();
    await expect(
      page.locator(".af-rail-list .af-row", { hasText: sessionName }),
    ).toBeVisible({ timeout: 15_000 });
    await page.locator(".af-rail-list .af-row", { hasText: sessionName }).click();
    await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  };
  const rows = (host: Locator): Promise<number> => host.locator(".xterm-rows > div").count();
  const visible = (page: Page): Promise<string> => page.evaluate(() => document.visibilityState);

  // One context, two tabs: bringToFront gives airtight visibility control —
  // the non-front tab is document-hidden, which is the state under test.
  const ctx = await browser.newContext({ viewport: { width: 375, height: 667 } });
  try {
    const first = await ctx.newPage();
    await open(first);
    await attach(first);
    const firstHost = first.locator(".af-term-host");
    // Attach fit can land in two passes (metrics arrive after open), so measure
    // the settled grid — the convergence assert below compares against it.
    let localRows = 0;
    await expect
      .poll(async () => {
        const next = await rows(firstHost);
        const settled = next > 0 && next === localRows;
        localRows = next;
        return settled;
      }, { message: "the phone grid must finish its attach fit before it is measured" })
      .toBe(true);

    // The tall peer's first OpResize strands the hidden tab on a grid that
    // cannot be its own — the strand setup #2347 documents. Opening the tab
    // activates it, hiding the first — exactly the parked-page state.
    const second = await ctx.newPage();
    await second.setViewportSize({ width: 375, height: 2000 });
    await open(second);
    await attach(second);
    const secondHost = second.locator(".af-term-host");
    let peerRows = 0;
    await expect
      .poll(async () => (peerRows = await rows(secondHost)), {
        message: "the tall peer must own a meaningfully larger grid",
      })
      .toBeGreaterThan(localRows * 2);
    await expect
      .poll(() => rows(firstHost), { message: "the peer's echo must strand the first tab" })
      .toBe(peerRows);

    // Pin the visibility roles: the peer is the frontmost target, the stranded
    // tab is document-hidden — where a starved frame used to lose the reclaim.
    await second.bringToFront();
    await expect
      .poll(() => visible(first), {
        message: "the stranded client must be document-hidden for this leg to prove anything",
      })
      .toBe("hidden");

    // A drawer dispatch is DOM-only: it reaches the layout-change refit hook
    // without activating the page — the same trigger the #2347 drawer leg uses.
    await first.locator(".af-nav-toggle").dispatchEvent("click");
    await expect
      .poll(() => rows(firstHost), {
        message: "the hidden client's scheduled fit must still reclaim its own rows",
        timeout: 15_000,
      })
      .toBeLessThan(peerRows);

    // The reclaim is local-only. Give the wire a beat: on loopback a stray
    // OpResize would echo back into the peer's xterm within milliseconds, and
    // applyEchoedSize rebuilds its row divs synchronously — a moved grid is
    // observable, an unmoved one is the proof the hidden page never wrote.
    await first.waitForTimeout(2_000);
    expect(
      await rows(secondHost),
      "the hidden client's reclaim must not move the shared grid off the live peer",
    ).toBe(peerRows);

    // Activation is the legitimate writer: fronting the stranded tab schedules
    // the fit whose convergence send flushes the owed resize, and the daemon
    // echoes the new owner to the peer — whose own rows now drop to it.
    await first.bringToFront();
    await expect
      .poll(() => rows(secondHost), {
        message: "activation must flush the owed resize — the peer now follows the fronted client",
        timeout: 15_000,
      })
      .toBe(localRows);
  } finally {
    await ctx.close();
    try {
      af("sessions", "kill", sessionName);
    } catch {
      // The throwaway daemon may already have removed a failed startup.
    }
  }
});
