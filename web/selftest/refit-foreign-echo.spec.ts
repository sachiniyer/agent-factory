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
// the pane's scheduled refit to beat that echo — an ordering the spec cannot
// control directly. What CAN be controlled: whether the pane may write at
// all. A parked page withholds every wire write (sendResize refuses while
// rAF starves), and connectAfterInitialFit is not gated on visibility — a
// parked page still connects. So this spec lets the pane's FIRST connect
// happen while parked:
//
//   1. the probe seeds 34x100 on the agent tab before any pane exists;
//   2. the page is parked (rAF deferred — the real parked state);
//   3. the pane attaches: its initial fit delivers by the 250ms timer,
//      marks the page parked, and connects. On open the force-send
//      withholds; the subscribe replay carries 34x100 — the contested
//      sighting on master, admission on the fix — and the parked refit it
//      schedules withholds its claim too, leaving it owed;
//   4. unpark flushes the owed claim; that echo lands while the emulator
//      already shows the host grid — a no-op that cannot touch the record;
//   5. the probe writes 34x100 once more: the peer's first real answer.
//      On master the replay primed the window so the repeat yields — the
//      emulator stands down onto 34 rows. On the fix the repeat is the
//      pane's first contested sighting and it re-asserts the host fit.
test("#6635 an activation answers a peer's first real write — the subscribe replay is not a contest", async ({
  page,
  request,
}) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === session);
  expect(selected?.id).toBeTruthy();

  // A tall viewport so the pane's own fit claims a grid clearly distinct from
  // the foreign one the probe writes (>60 rows, the #2347 desktop shape).
  await page.setViewportSize({ width: 1280, height: 2000 });
  await page.goto("/");
  await page.locator("#app[data-af-resync-settled]").waitFor();
  // Fonts settle before the pane attaches — its first fit must already be
  // the final grid, or a font-metric change writes a second claim whose
  // echo re-contests the pane and masks the yield this spec pins.
  await page.evaluate(
    () =>
      document.fonts.ready.then(
        () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
      ),
  );

  // A raw subscriber on the agent tab's stream (?tab=0 binds the same broker
  // the pane's ?tab_id= attach lands on). It seeds the foreign grid BEFORE
  // the pane can possibly subscribe, then serves as the ordering probe:
  // every resize the broker applies is broadcast back to it, in broker
  // order — the same probe trick the #6215 spec uses. The grid must differ
  // from xterm's 24x80 default AND from the pane's own fit: a replay that
  // matches the emulator's size is a no-op and primes nothing — that is why
  // the real failure's 34 rows is the signature here.
  const staleRows = 34;
  const staleCols = 100;
  const atSeed = await page.evaluate(
    async ({ sessionId, staleRows, staleCols }) => {
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
      // OpResize 0x02 + rows,cols as big-endian uint16s — the foreign grid,
      // in place before the pane can possibly subscribe.
      ws.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, staleCols]));
      return w.__waitRows((r) => r === staleRows, 0, `${staleRows}-row seed`);
    },
    { sessionId: selected.id, staleRows, staleCols },
  );

  // Park BEFORE the pane exists. Its mount fits through the starved-frame
  // timer twin, which marks the page parked and connects anyway; from then
  // on every wire write withholds. The pane can never stamp an echo of its
  // own between the replay and the peer's answer — the interposition that
  // kept the flake rare is removed by construction, not luck.
  await park(page);
  await page.evaluate((sessionId) => {
    window.location.hash = `#/session/${encodeURIComponent(sessionId)}`;
  }, selected.id);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open", {
    timeout: 15_000,
  });
  // The replay primes {34x100} on master; the parked page's refit withholds
  // the claim and arms the wake probe. Give that parked fit its timer.
  await page.waitForTimeout(500);
  const rows = () => page.locator(".af-term-host .xterm-rows > div").count();
  const waitEchoGT = (min: number, after: number, label: string) =>
    page.evaluate(
      ([n, a, l]) => {
        const w = window as unknown as {
          __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
        };
        return w.__waitRows((r) => r > n, a, l);
      },
      [min, after, label],
    );

  // Un-hide: the deferred frames replay, the owed claim writes, and its
  // echo lands while the emulator already shows the host grid — a no-op
  // that cannot overwrite the replay's contested record.
  await unpark(page);
  await waitEchoGT(60, atSeed + 1, "unpark claim flush");

  // The peer's first real answer, inside the replay-primed window. Its own
  // echo proves the pane has SEEN it — what the emulator does next is the
  // pane's reply, not an echo still in flight. On master the emulator
  // stands down onto 34 rows; on the fix it takes the host grid back.
  await page.evaluate(
    ({ staleRows, staleCols }) => {
      const w = window as unknown as { __probe: WebSocket };
      w.__probe.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, staleCols]));
    },
    { staleRows, staleCols },
  );
  const atRepeat = await page.evaluate(
    ([n, a, l]) => {
      const w = window as unknown as {
        __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
      };
      return w.__waitRows((r) => r === n, a, l);
    },
    [staleRows, atSeed + 2, `${staleRows}-row repeat`],
  );
  // The probe's echo does not prove the PANE processed the repeat — each
  // subscription consumes the broadcast independently, and the emulator was
  // already on the host grid from the unpark flush, so a poll now could pass
  // on pre-repeat state. The reclaim is observable on the wire: a >60 echo
  // AFTER the repeat can only be the pane re-asserting its host fit. On
  // master none ever comes — the yield writes nothing.
  await waitEchoGT(60, atRepeat + 1, "post-repeat reclaim");
  await expect
    .poll(rows, {
      message: "the activation must re-assert its host fit against the peer's first real answer",
      timeout: 10_000,
    })
    .toBeGreaterThan(60);
});

// #6635 (the 2026-11-06 #2347 recurrence, run 42219554428): the admission flag
// was spent only by a TERM-DIVERGENT echo. A client that already owned the
// authority saw its subscribe replay as a no-op — `rows === term.rows` — so the
// flag survived to the FIRST FOREIGN echo, a live peer write. That write took
// the admission's free pass without seeding the contest record; the peer's
// counter to the pane's claim then read as a fresh sighting inside the yield
// window instead of a repeat. The pane re-claimed where it must yield — and the
// reclaim it landed on the peer WAS the repeat inside the peer's window: the
// peer stood down to the grid the pane had just re-written, stranding the
// active client at 34 rows for the whole 15s poll.
//
// The strand needs only one state-machine fact: the pane's first delivered
// echo must be a NO-OP — the replayed authority equals its own emulator grid.
// That is forced, not raced: a first context (A) claims the grid a second
// context (B) will fit — both are 1280x2000, so both hosts produce the same
// rows/cols — and B subscribes while A's grid is already the broker's
// authority. B's replay is its own size: on master the flag survives to the
// probe's first write below; on the fix it is spent there regardless.
// The probe then plays the insistent peer verbatim — claim, then the counter
// inside the yield window — and B must stand down: no reclaim may leave the
// wire after the repeat, and B's emulator must rest on the peer's grid.
test("#6635 (2026-11-06 recurrence): a pane already owning the authority yields to the peer's counter — the replay consumes the admission even as a no-op", async ({
  browser,
  request,
}) => {
  const snap = await (await request.post("/v1/Snapshot", { data: { repo_id: "" } })).json();
  const selected = snap.data.instances.find((s: { title: string }) => s.title === session);
  expect(selected?.id).toBeTruthy();

  const ctxA = await browser.newContext({ viewport: { width: 1280, height: 2000 } });
  const ctxB = await browser.newContext({ viewport: { width: 1280, height: 2000 } });
  try {
    const settleFonts = (p: Page) =>
      p.evaluate(
        () =>
          document.fonts.ready.then(
            () => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))),
          ),
      );
    const attach = async (p: Page) => {
      await p.goto("/");
      await p.locator("#app[data-af-resync-settled]").waitFor();
      await settleFonts(p);
      await p.evaluate((sessionId) => {
        window.location.hash = `#/session/${encodeURIComponent(sessionId)}`;
      }, selected.id);
      await expect(p.locator(".af-main")).toHaveAttribute("data-term-status", "open", {
        timeout: 15_000,
      });
      const rows = () => p.locator(".af-term-host .xterm-rows > div").count();
      await expect.poll(rows, { timeout: 10_000 }).toBeGreaterThan(60);
      return rows;
    };

    const pageA = await ctxA.newPage();
    const rowsA = await attach(pageA);
    const gridRows = await rowsA();

    // The ordering probe on the same broker, seeded while A's grid is already
    // the authority — its own subscribe replay is therefore gridRows, which
    // anchors the echo indices below.
    await pageA.evaluate(
      async ({ sessionId }) => {
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
      },
      { sessionId: selected.id },
    );
    const waitEchoEq = (want: number, after: number, label: string) =>
      pageA.evaluate(
        ([n, a, l]) => {
          const w = window as unknown as {
            __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
          };
          return w.__waitRows((r) => r === n, a, l);
        },
        [want, after, label] as const,
      );

    // B attaches under A's grid: its subscribe replay echoes gridRows, which is
    // exactly what B's emulator already fits — the no-op replay that on master
    // left the admission standing for the probe's first write.
    const pageB = await ctxB.newPage();
    const rowsB = await attach(pageB);
    // B's connect claim lands as a same-grid write; its echo (the first
    // gridRows after the probe's own subscribe replay at index 0) is a no-op
    // for every subscriber and only proves the epoch is live.
    const atBClaim = await waitEchoEq(gridRows, 1, "B's subscribe claim");

    // The peer's claim: 34x100 differs from every grid on the wire. Both panes
    // re-assert their host fit, but the wire need not show two reclaim echoes:
    // whichever OpResize lands first supplies the gridRows echo the second
    // pane's sendResize then dedupes, and two writes arriving between a
    // subscriber's reads coalesce into one. Wait for the first reclaim echo —
    // some pane must write, so one is guaranteed — then settle past the 250ms
    // classification twin so a second, if any exists, lands before the counter
    // (which must stay inside the yield window measured from B's first
    // sighting; the settle leaves a wide margin under it).
    const staleRows = 34;
    const staleCols = 100;
    const writeForeign = () =>
      pageA.evaluate(
        ({ staleRows, staleCols }) => {
          const w = window as unknown as { __probe: WebSocket };
          w.__probe.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, staleCols]));
        },
        { staleRows, staleCols },
      );
    await writeForeign();
    const atClaim = await waitEchoEq(staleRows, atBClaim + 1, "peer's foreign claim");
    await waitEchoEq(gridRows, atClaim + 1, "a pane's reclaim");
    await pageA.waitForTimeout(700);

    // The peer's counter — a repeat of the SAME foreign grid inside the yield
    // window. On master it reads fresh to B (the record was never seeded) and
    // B re-claims: a gridRows echo leaves the wire, which A then contests
    // afresh, and the pair keeps last-resize-wins on the grid the pane had no
    // business re-writing. On the fix B yields: nothing follows the repeat,
    // and B's emulator rests at the peer's 34 rows.
    await writeForeign();
    const atRepeat = await waitEchoEq(staleRows, atClaim + 1, "peer's repeat");
    // Give a forfeited reclaim its full delivery before reading the wire: on
    // master a gridRows echo lands here within a few frames of the repeat.
    await pageA.waitForTimeout(2500);
    const trailing = await pageA.evaluate(
      (after) =>
        (window as unknown as { __echoes: number[] }).__echoes.filter(
          (r, i) => i > after && r > 60,
        ),
      atRepeat,
    );
    expect(trailing, "no reclaim may leave the wire after the pane yields").toEqual([]);
    // And the emulator shows the stand-down: B rests on the yielded authority.
    // On master it re-claims its own grid instead — it may flicker through 34
    // on the way there, so the settled DOM is what discriminates.
    await expect
      .poll(rowsB, {
        message: "the yielded pane's emulator rests on the insistent peer's grid",
        timeout: 10_000,
      })
      .toBe(staleRows);
  } finally {
    await ctxA.close();
    await ctxB.close();
  }
});
