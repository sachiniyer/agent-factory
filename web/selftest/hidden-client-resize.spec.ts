// Parked-client grid-ownership guard — the counterpart proof to #2347's reclaim.
//
// A stranded client's scheduled fit is delivered by a frame OR a 250ms timer, so
// it still lands on a page whose frames have stopped flowing. That timer makes
// one property worth pinning explicitly: a parked page is where a reclaim is
// DELIVERED, but it is not an activation — no user is present — so it must never
// write the shared PTY grid. If it could, a parked tab would steal the grid out
// from under a live visible peer (or bounce it between two parked pages).
// sendResize refuses the wire write while parked; the page still refits locally
// so xterm's DOM is honest, and the first fit after a frame-coupled signal
// unparks it converges the daemon — the divergence between the local grid and
// lastRows/lastCols is itself the owed-write marker.
//
// "Parked" here means the page's frames have STOPPED, not that a DOM flag
// flipped: an occluded headless target (and e.g. a fully covered window on some
// platforms) keeps document.visibilityState='visible' while its rAF is dead.
// The spec proves the state empirically — it measures that the stranded page's
// rAF does not fire — rather than trusting a flag.
//
// Two tabs of one context, both subscribed to one PTY, prove it end to end:
//
//   1. a tall peer strands a 375x667 tab on the peer's 2000px grid;
//   2. the stranded tab's rAF is measurably dead — the parked state;
//   3. a drawer dispatch refits it: its own rows drop to the local grid (the
//      timer twin delivered the reclaim) but the peer's rows do NOT move — no
//      OpResize ever left the parked page, or the daemon would have echoed it
//      into the peer's xterm;
//   4. fronting the first tab flushes the owed write: the daemon learns its
//      grid and the peer is stomped to it — the legitimate last-writer-wins
//      activation #2347's machinery provides.
//
// The same spec run against a master binary shows the other half of the story:
// master delivers nothing while parked (its only vehicle was the starved rAF),
// so the property held vacuously at the price of the strand — the peer stays
// untouched AND the stranded tab's rows never drop, failing leg 3's local
// reclaim. That run is the A/B comparison; this spec passes only on the fix.

import { expect, type Locator, type Page, test } from "@playwright/test";

test("a parked client reclaims locally but never writes the shared grid", async ({ browser }) => {
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
  // The empirical parked probe: whether the page's rAF still fires. An occluded
  // headless target reports visibilityState='visible' while its frames starve,
  // so the state under test is proven by measuring the missing frame — 1.5s of
  // silence, ten times the timer twin's 250ms cadence, is unambiguous deadness —
  // rather than trusting a DOM flag.
  const frameAlive = (page: Page): Promise<boolean> =>
    page.evaluate(
      () =>
        new Promise<boolean>((resolve) => {
          const dead = setTimeout(() => resolve(false), 1_500);
          requestAnimationFrame(() => {
            clearTimeout(dead);
            resolve(true);
          });
        }),
    );

  // One context, two tabs: bringToFront gives airtight frontmost control — the
  // non-front tab stops getting frames, which is the state under test.
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

    // The tall peer's first OpResize strands the occluded tab on a grid that
    // cannot be its own — the strand setup #2347 documents. Opening the tab
    // activates it, parking the first — exactly the stranded-page state.
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

    // Pin the parked state: the peer is the frontmost target, and the stranded
    // tab's frames must measurably have stopped — the state in which a starved
    // rAF used to lose the reclaim entirely.
    await second.bringToFront();
    await expect
      .poll(() => frameAlive(first), {
        message: "the stranded client must be frame-starved for this leg to prove anything",
      })
      .toBe(false);

    // A drawer dispatch is DOM-only: it reaches the layout-change refit hook
    // without activating the page — the same trigger the #2347 drawer leg uses.
    await first.locator(".af-nav-toggle").dispatchEvent("click");
    await expect
      .poll(() => rows(firstHost), {
        message: "the parked client's scheduled fit must still reclaim its own rows",
        timeout: 15_000,
      })
      .toBeLessThan(peerRows);

    // The reclaim is local-only. Give the wire a beat: on loopback a stray
    // OpResize would echo back into the peer's xterm within milliseconds, and
    // applyEchoedSize rebuilds its row divs synchronously — a moved grid is
    // observable, an unmoved one is the proof the parked page never wrote.
    await first.waitForTimeout(2_000);
    expect(
      await rows(secondHost),
      "the parked client's reclaim must not move the shared grid off the live peer",
    ).toBe(peerRows);

    // Activation is the legitimate writer: fronting the stranded tab resumes
    // its frames — a delivered frame or a focus event unparks it — and the next
    // fit's convergence send flushes the owed resize. The daemon echoes the new
    // owner to the peer, whose own rows drop to the fronted client's grid.
    await first.bringToFront();
    await expect
      .poll(() => frameAlive(first), {
        message: "the fronted client's frames must resume for the flush to be an activation",
        timeout: 15_000,
      })
      .toBe(true);
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
