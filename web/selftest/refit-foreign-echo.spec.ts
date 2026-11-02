// #6215: a stale authoritative write can strand the active terminal's grid.
// The shared PTY is last-resize-wins: every subscriber's OpResize is broadcast
// back to all clients as the authoritative size. A write that names no live
// client's geometry — a hidden sibling pane's reconnect force-send, the broker
// re-adopting a pane's tmux-measured size on a repaint capture, an old belief
// replayed to a fresh subscriber — reverts the ACTIVE pane's emulator, and on
// master nothing re-asserts the host's fit: the pane stays on the stale grid
// until an unrelated signal happens to refit it. That is the "taller mobile
// viewport never refits" flake — the growth fit ran, then a stale echo undid it.
//
// This spec drives the strand deterministically: after the viewport growth has
// refit the terminal, a second raw subscriber on the same stream writes the
// PRE-growth grid back — the exact bytes a stale peer sends (OpResize 0x02,
// two big-endian uint16s). The pane must apply the echo (the foreign grid is
// authoritative) and then re-assert its own host fit without any user signal.
import { test, expect, type Page } from "@playwright/test";

const session = process.env.AF_WEB_SESSION_ORDER ?? "probe-order";

// Park the page the way a REAL hidden tab behaves, which is also how the
// production detector sees it: rAF callbacks are deferred, not dropped —
// they queue while the page is unpainted and all fire on the first frame
// after it is visible again. Observers are stubbed the same way (their
// callbacks come from the same rendering steps), or headless Chromium's
// live deliveries would un-park the flag through a signal the scenario
// could never see. Same helper hidden-client-resize.spec.ts carries.
const park = (page: Page): Promise<void> =>
  page.evaluate(() => {
    const w = window as unknown as {
      __afRealRaf?: typeof requestAnimationFrame;
      __afRealCancel?: typeof cancelAnimationFrame;
      __afDeferred?: Map<number, FrameRequestCallback>;
      __afNextRafId?: number;
      __afRealIO?: typeof IntersectionObserver;
      __afRealRO?: typeof ResizeObserver;
    };
    if (w.__afRealRaf) {
      return;
    }
    w.__afRealRaf = window.requestAnimationFrame;
    w.__afRealCancel = window.cancelAnimationFrame;
    w.__afRealIO = window.IntersectionObserver;
    w.__afRealRO = window.ResizeObserver;
    w.__afDeferred = new Map();
    w.__afNextRafId = 1;
    window.requestAnimationFrame = (cb: FrameRequestCallback) => {
      const id = w.__afNextRafId as number;
      w.__afNextRafId = id + 1;
      (w.__afDeferred as Map<number, FrameRequestCallback>).set(id, cb);
      return id;
    };
    window.cancelAnimationFrame = (id: number) => {
      (w.__afDeferred as Map<number, FrameRequestCallback>).delete(id);
    };
    const deadObserver = class {
      observe(): void {}
      unobserve(): void {}
      disconnect(): void {}
      takeRecords(): unknown[] {
        return [];
      }
      get root(): null {
        return null;
      }
      get rootMargin(): string {
        return "0px";
      }
      get thresholds(): readonly number[] {
        return [0];
      }
    };
    window.IntersectionObserver = deadObserver as unknown as typeof IntersectionObserver;
    window.ResizeObserver = deadObserver as unknown as typeof ResizeObserver;
  });

// Un-hide: restore the real scheduling surface, then replay every deferred
// callback in registration order — exactly what a browser does when a
// hidden tab becomes visible.
const unpark = (page: Page): Promise<void> =>
  page.evaluate(() => {
    const w = window as unknown as {
      __afRealRaf?: typeof requestAnimationFrame;
      __afRealCancel?: typeof cancelAnimationFrame;
      __afDeferred?: Map<number, FrameRequestCallback>;
      __afRealIO?: typeof IntersectionObserver;
      __afRealRO?: typeof ResizeObserver;
    };
    if (w.__afRealRaf) {
      window.requestAnimationFrame = w.__afRealRaf;
      window.cancelAnimationFrame = w.__afRealCancel as typeof cancelAnimationFrame;
      window.IntersectionObserver = w.__afRealIO as typeof IntersectionObserver;
      window.ResizeObserver = w.__afRealRO as typeof ResizeObserver;
      delete w.__afRealRaf;
      delete w.__afRealCancel;
      delete w.__afRealIO;
      delete w.__afRealRO;
    }
    const deferred = w.__afDeferred ? [...w.__afDeferred.values()] : [];
    w.__afDeferred?.clear();
    const now = performance.now();
    for (const cb of deferred) {
      cb(now);
    }
  });

test("#6215 a stale authoritative resize cannot strand the active terminal's grid", async ({ page, request }) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === session);
  expect(selected?.id).toBeTruthy();

  await page.setViewportSize({ width: 390, height: 812 });
  await page.goto(`/#/session/${encodeURIComponent(selected.id)}`);
  await page.locator("#app[data-af-resync-settled]").waitFor();
  await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);

  await page.locator(".af-appbar-more").click();
  const processTab = page.locator('.af-tabbar .af-tab:has([data-icon="terminal"])').first();
  await expect(processTab).toBeVisible();
  await processTab.click();
  await expect(processTab).toHaveClass(/af-tab-active/);

  const rows = () => page.locator(".af-term-host .xterm-rows > div").count();
  await expect.poll(rows).toBeGreaterThan(0);
  const small = await rows();

  // The reported failure: a taller viewport refits the active terminal. Then
  // let every writer fall quiet — the growth fit's own sendResize, the resize
  // debounce, and any snapshot-size adoption racing the tmux apply — so the
  // stale write below is the LAST word and a grown echo can only be the pane's
  // own reclaim. Without the settle, an in-flight capture can adopt the
  // pre-stale geometry and echo it back, masking the strand (a "repair" that
  // arrives by luck, not by the pane re-asserting its host).
  await page.setViewportSize({ width: 390, height: 932 });
  await expect.poll(rows, { timeout: 8000 }).toBeGreaterThan(small);
  const grown = await rows();
  await page.waitForTimeout(2500);

  // A stale writer now pushes the PRE-growth grid back onto the shared PTY —
  // for example a sibling pane's reconnect force-sending its old emulator size.
  // The daemon's broadcast makes it authoritative for every subscriber — so the
  // writer's own socket sees every resize echo, and can act as the ordering
  // probe: the stale echo proves the foreign write landed for every subscriber,
  // and a GROWN echo afterwards can only come from the active pane re-asserting
  // its host fit. The pane's own data-tab-id is the exact ?tab_id= identity the
  // stream binds.
  const tabId = await page.evaluate(
    () =>
      document
        .querySelector(".af-term-host .af-pane:not(.af-pane-hidden)")
        ?.getAttribute("data-tab-id") ?? null,
  );
  expect(tabId).toBeTruthy();
  await page.evaluate(
    async ({ sessionId, tabId, staleRows, grownRows }) => {
      const ws = new WebSocket(
        `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}` +
          `/v1/sessions/${encodeURIComponent(sessionId)}/stream?access_token=&tab_id=${encodeURIComponent(tabId ?? "")}`,
      );
      ws.binaryType = "arraybuffer";
      // Resize echoes received on this socket, in order (rows values only).
      // The authoritative echo is a TEXT control frame — {"type":"resize"} —
      // not the client→server binary OpResize this socket writes.
      const echoes: number[] = [];
      ws.onmessage = (ev) => {
        if (typeof ev.data !== "string") {
          return;
        }
        try {
          const msg = JSON.parse(ev.data) as { type?: string; rows?: number };
          if (msg.type === "resize" && typeof msg.rows === "number") {
            echoes.push(msg.rows);
          }
        } catch {
          // not a control frame
        }
      };
      const waitEcho = (want: number, after: number) =>
        new Promise<number>((resolve, reject) => {
          const timer = setTimeout(
            () => reject(new Error(`no ${want}-row resize echo within 10s (saw ${echoes.join(",")})`)),
            10_000,
          );
          const poll = setInterval(() => {
            const at = echoes.indexOf(want, after);
            if (at >= 0) {
              clearTimeout(timer);
              clearInterval(poll);
              resolve(at);
            }
          }, 20);
        });
      await new Promise<void>((resolve, reject) => {
        ws.onopen = () => resolve();
        ws.onerror = () => reject(new Error("stale-writer stream failed to open"));
      });
      // OpResize 0x02 + rows,cols as big-endian uint16s — the pre-growth grid.
      // A subscribe-time echo of the current authority may arrive first, so the
      // stale echo is matched by value, not position.
      ws.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, 80]));
      const staleAt = await waitEcho(staleRows, 0);
      // The daemon broadcast the stale grid to every subscriber; the active
      // pane has applied it. Nothing but that pane's own fit can now write the
      // grown grid back — so a grown echo here IS the reclaim. On master it
      // never comes: the pane stays on the foreign grid until an unrelated
      // signal happens to refit it (the #6215 strand).
      await waitEcho(grownRows, staleAt + 1);
      ws.close();
    },
    { sessionId: selected.id, tabId, staleRows: small, grownRows: grown },
  );

  // And the emulator shows it: the re-asserted fit has taken the grid back.
  await expect.poll(rows, { timeout: 8000 }).toBe(grown);
});

// #6635 (the #2347 flake's post-#6628 signature): activating a terminal while a
// peer owns the shared PTY must leave the grid at the NEW host's fit. The yield
// window exists to bound a genuinely insistent peer — one that re-writes its
// grid AFTER the pane's claim is on the wire. The broker itself marks the one
// echo that is NOT such a write: a fresh subscriber's first resize echo is a
// replay of the current authority (ptybroker_sub.go — `resizeSeen = resizeGen
// - 1`), the grid the world already had, not a contested answer to a claim the
// pane had not yet made. Recording it as a contest lets the peer's FIRST real
// answer masquerade as a repeat inside the yield window — the activation then
// stands down to the grid it was meant to take. That is the real failure: the
// second client stayed at the first's 34 rows while the spec polled for >60.
//
// The strand is hard to pin because master's last-only contested record is
// overwritten by EVERY differing echo — including the pane's OWN claim echo
// when it lands while the emulator sits on the foreign grid. The flake needed
// the pane's scheduled refit to beat that echo. This spec makes the ordering
// airtight instead: the pane is PARKED when the replay primes it, so its own
// writes withhold (sendResize refuses the wire on a parked page) and no echo
// of theirs can ever interpose between the replay and the peer's answer.
//
//   1. the pane attaches and claims its tall grid (>60 rows at 1280x2000);
//   2. it is parked (rAF deferred) and a foreign echo classifies it parked —
//      its scheduled fit delivers by timer, marks it parked, withholds;
//   3. its socket is dropped; the probe writes 34x100 while it is blind;
//   4. it reconnects: the subscribe replay carries 34x100 — the contested
//      sighting on master, admission on the fix — and the parked refit
//      withholds its claim again;
//   5. unpark flushes the owed claim; that echo lands while the emulator
//      already shows the host grid — a no-op that cannot touch the record;
//   6. the probe writes 34x100 once more: the peer's first real answer.
//      On master the replay primed the window so the repeat yields — the
//      emulator stands down onto 34 rows. On the fix the repeat is the
//      pane's first contested sighting and it re-asserts the host fit.
test("#6635 an activation answers a peer's first real write — the subscribe replay is not a contest", async ({
  browser,
  request,
}) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === session);
  expect(selected?.id).toBeTruthy();

  // Two contexts so the pane's socket can drop and rejoin without losing the
  // probe's ear on the broker (setOffline is per-context).
  const paneCtx = await browser.newContext({ viewport: { width: 1280, height: 2000 } });
  const probeCtx = await browser.newContext({ viewport: { width: 800, height: 600 } });
  const pane = await paneCtx.newPage();
  const probe = await probeCtx.newPage();
  const staleRows = 34;
  const staleCols = 100;
  try {
    // The pane attaches first and claims its host grid — the baseline the
    // emulator must return to. Fonts settle before the nav so the first fit
    // is already the final one.
    await pane.goto("/");
    await pane.locator("#app[data-af-resync-settled]").waitFor();
    await pane.evaluate(
      () =>
        document.fonts.ready.then(
          () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
        ),
    );
    await pane.evaluate((sessionId) => {
      window.location.hash = `#/session/${encodeURIComponent(sessionId)}`;
    }, selected.id);
    await expect(pane.locator(".af-main")).toHaveAttribute("data-term-status", "open");
    const rows = () => pane.locator(".af-term-host .xterm-rows > div").count();
    await expect.poll(rows, { timeout: 8000 }).toBeGreaterThan(60);
    const claimed = await rows();

    // The ordering probe on the agent tab's stream (?tab=0 binds the same
    // broker the pane's ?tab_id= attach lands on). Every resize the broker
    // applies is broadcast back to it, in broker order — the same probe
    // trick the #6215 spec uses.
    await probe.goto("/");
    await probe.evaluate(async (sessionId) => {
      const w = window as unknown as {
        __echoes: number[];
        __probe: WebSocket;
        __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
      };
      w.__echoes = [];
      const ws = new WebSocket(
        `${location.protocol === "https:" ? "wss" : "ws"}://${location.host}` +
          `/v1/sessions/${encodeURIComponent(sessionId)}/stream?access_token=&tab=0`,
      );
      ws.binaryType = "arraybuffer";
      ws.onmessage = (ev) => {
        if (typeof ev.data !== "string") {
          return;
        }
        try {
          const msg = JSON.parse(ev.data) as { type?: string; rows?: number };
          if (msg.type === "resize" && typeof msg.rows === "number") {
            w.__echoes.push(msg.rows);
          }
        } catch {
          // not a control frame
        }
      };
      w.__waitRows = (pred, after, label) =>
        new Promise<number>((resolve, reject) => {
          const timer = setTimeout(
            () =>
              reject(new Error(`no ${label} resize echo within 10s (saw ${w.__echoes.join(",")})`)),
            10_000,
          );
          const poll = setInterval(() => {
            const at = w.__echoes.findIndex((r, i) => i >= after && pred(r));
            if (at >= 0) {
              clearTimeout(timer);
              clearInterval(poll);
              resolve(at);
            }
          }, 20);
        });
      w.__probe = ws;
      await new Promise<void>((resolve, reject) => {
        ws.onopen = () => resolve();
        ws.onerror = () => reject(new Error("probe stream failed to open"));
      });
    }, selected.id);

    // Park the pane, then have the probe write a foreign grid. The echo's
    // contested sighting schedules a fit on a starved page; the 250ms timer
    // delivers it, marks the page parked, and its write withholds — the
    // classification the reconnect below depends on. (The 34x100 seed must
    // come later, while the pane is blind, or its echo would be a REAL
    // sighting that makes the repeat yield on the fix too.)
    await park(pane);
    const writeGrid = (r: number, c: number) =>
      probe.evaluate(
        ([rows, cols]) => {
          const w = window as unknown as { __probe: WebSocket };
          w.__probe.send(
            new Uint8Array([0x02, (rows >> 8) & 0xff, rows & 0xff, (cols >> 8) & 0xff, cols & 0xff]),
          );
        },
        [r, c] as [number, number],
      );
    // Broker-order waits: the index an echo lands at is returned so each
    // later wait starts past every echo already accounted for.
    const waitEcho = (rows: number, after: number, label: string) =>
      probe.evaluate(
        ([n, a, l]) => {
          const w = window as unknown as {
            __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
          };
          return w.__waitRows((r) => r === n, a, l);
        },
        [rows, after, label],
      );
    const waitEchoGT = (min: number, after: number, label: string) =>
      probe.evaluate(
        ([n, a, l]) => {
          const w = window as unknown as {
            __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
          };
          return w.__waitRows((r) => r > n, a, l);
        },
        [min, after, label],
      );
    await writeGrid(50, 110);
    const atClassify = await waitEcho(50, 0, "50-row classifier");
    // Well past the 250ms timer twin: the parked fit has delivered and
    // classified the page (writes now withhold until un-hide).
    await pane.waitForTimeout(600);

    // Blind the pane, seed the foreign grid, reconnect: the subscribe replay
    // is the pane's only 34-row sighting. Wait for "reconnecting" first so
    // the "open" below is the REOPEN — a stale "open" would let the unpark
    // run before the replay and let the connect's force-send echo overwrite
    // the record it must not touch.
    await paneCtx.setOffline(true);
    await writeGrid(staleRows, staleCols);
    const atSeed = await waitEcho(staleRows, atClassify + 1, `${staleRows}-row seed`);
    await expect(pane.locator(".af-main")).toHaveAttribute("data-term-status", "reconnecting", {
      timeout: 10_000,
    });
    await paneCtx.setOffline(false);
    await expect(pane.locator(".af-main")).toHaveAttribute("data-term-status", "open", {
      timeout: 15_000,
    });
    // The replay primes {34x100} on master; the parked page's refit withholds
    // the claim and arms the wake probe. Give that parked fit its timer.
    await pane.waitForTimeout(400);

    // Un-hide: the deferred frames replay, the owed claim writes, and its
    // echo lands while the emulator already shows the host grid — a no-op
    // that cannot overwrite the replay's contested record.
    await unpark(pane);
    await waitEchoGT(60, atSeed + 1, "unpark claim flush");

    // The peer's first real answer, inside the replay-primed window. Its own
    // echo proves the pane has SEEN it — what the emulator does next is the
    // pane's reply, not an echo still in flight. On master the emulator
    // stands down onto 34 rows; on the fix it takes the host grid back.
    await writeGrid(staleRows, staleCols);
    await waitEcho(staleRows, atSeed + 2, `${staleRows}-row repeat`);
    await expect
      .poll(rows, {
        message: "the activation must re-assert its host fit against the peer's first real answer",
        timeout: 10_000,
      })
      .toBe(claimed);
  } finally {
    await paneCtx.close();
    await probeCtx.close();
  }
});
