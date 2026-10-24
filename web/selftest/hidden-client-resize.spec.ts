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
// "Parked" means the page's frames have STOPPED — a hidden tab, an occluded
// window, a renderer under pressure. Headless Chromium cannot produce it
// organically: verified on this harness, a non-front target keeps
// visibilityState='visible' AND keeps firing rAF; CDP window minimization is
// virtual; the only lifecycle state the protocol offers ('frozen') kills
// timers too — the wrong shape. (That is also why master's lone-rAF reclaim
// could pass for so long: the strand only bites where frames really starve.)
// So the spec parks the page exactly the way the production detector sees it:
// requestAnimationFrame is stubbed to never deliver. Everything downstream is
// the shipped code: the armed-twin scheduler, the parked latch, the local-only
// fit, the suppressed OpResize, the unpark-and-flush — driven by the real
// 250ms timer and a real frame + focus signal.
//
// Two tabs of one context, both subscribed to one PTY, prove it end to end:
//
//   1. a tall peer strands a 375x667 tab on the peer's 2000px grid;
//   2. the stranded tab's frame delivery is switched off — measurably dead;
//   3. a drawer dispatch refits it: its own rows drop to the local grid (the
//      timer twin delivered the reclaim) but the peer's rows do NOT move — no
//      OpResize ever left the parked page, or the daemon would have echoed it
//      into the peer's xterm;
//   4. frame delivery restored + a real activation: the owed write flushes, the
//      daemon learns the fronted client's grid, and the peer is stomped to it.
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
  // The empirical parked probe: whether the page's rAF still fires. 1.5s of
  // silence — six times the timer twin's 250ms cadence — is unambiguous deadness.
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
  // Park the page exactly the way the production detector sees it: the armed
  // rAF can never run, so only the 250ms timer twin can deliver a scheduled
  // fit — the same input a hidden tab or occluded window gives it.
  const park = (page: Page): Promise<void> =>
    page.evaluate(() => {
      (window as { __afRealRaf?: typeof requestAnimationFrame }).__afRealRaf =
        window.requestAnimationFrame;
      window.requestAnimationFrame = () => -1;
    });
  const unpark = (page: Page): Promise<void> =>
    page.evaluate(() => {
      const w = window as { __afRealRaf?: typeof requestAnimationFrame };
      if (w.__afRealRaf) {
        window.requestAnimationFrame = w.__afRealRaf;
      }
      window.dispatchEvent(new Event("focus"));
    });

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

    // Establish the environment honestly: the non-front tab's frames ARE alive
    // in headless — there is no organic parked state to lean on — so park it by
    // cutting frame delivery, then prove the cut took.
    await second.bringToFront();
    expect(await frameAlive(first)).toBe(true);
    await park(first);
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

    // Activation is the legitimate writer: restore frame delivery, front the
    // tab (a trusted focus signal), and dispatch focus in case bringToFront's
    // focus event is not delivered in headless. Either way the page sees the
    // shipped unpark path — focus/observer/frame — then the next fit's
    // convergence send flushes the owed resize and the daemon stomps the peer.
    await unpark(first);
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
