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
import { test, expect } from "@playwright/test";

const session = process.env.AF_WEB_SESSION_ORDER ?? "probe-order";

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
// This spec drives the strand deterministically. A raw subscriber seeds a
// small foreign grid on the agent tab BEFORE the page's pane subscribes, so
// the pane's subscribe replay carries exactly it — the priming. The pane's own
// claim (>60 rows at this viewport) then lands, and the probe writes the
// foreign grid once more — precisely the peer's first post-claim answer. A
// >60-row echo afterwards can only be the pane re-asserting its host fit; on
// master the pane has yielded and that echo never comes.
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
  // Land on the session list: the shell is live but no pane has attached, so
  // the probe below is the tab's only subscriber when it seeds the grid.
  await page.goto("/");
  await page.locator("#app[data-af-resync-settled]").waitFor();

  // A raw subscriber on the agent tab's stream (?tab=0 binds the same broker
  // the pane's ?tab_id= attach lands on). It writes the foreign grid and then
  // serves as the ordering probe: every resize the broker applies is broadcast
  // back to it, in broker order — the same probe trick the #6215 spec uses.
  const staleRows = 24;
  const staleCols = 80;
  await page.evaluate(
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
      // OpResize 0x02 + rows,cols as big-endian uint16s — the foreign grid, in
      // place before the pane can possibly subscribe.
      ws.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, staleCols]));
      await w.__waitRows((r) => r === staleRows, 0, `${staleRows}-row`);
    },
    { sessionId: selected.id, staleRows, staleCols },
  );

  // Now the pane attaches (in-page hash navigation keeps the probe's socket).
  // Its subscribe replay delivers the foreign grid — the contested sighting
  // that primes the yield on master — before the pane's claim is on the wire.
  await page.evaluate((sessionId) => {
    window.location.hash = `#/session/${encodeURIComponent(sessionId)}`;
  }, selected.id);
  await expect(page.locator(".af-app")).toHaveClass(/af-session-first/);
  await expect(page.locator(".af-main")).toHaveAttribute("data-term-status", "open");
  const rows = () => page.locator(".af-term-host .xterm-rows > div").count();
  // The activation claims the host fit: the emulator renders the pane's own
  // >60-row grid (on every branch — the claim is unconditional).
  await expect.poll(rows, { timeout: 8000 }).toBeGreaterThan(60);
  const grown = await rows();

  // The claim must be on the wire before the peer answers — a repeat that
  // lands while the pane's armed fit is still pending would be followed by
  // that fit's own write either way and would prove nothing. The probe sees
  // every broadcast in order, so a >60-row echo here is the pane's claim.
  const after = await page.evaluate(async () => {
    const w = window as unknown as {
      __echoes: number[];
      __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
    };
    await w.__waitRows((r) => r > 60, 0, ">60-row");
    return w.__echoes.length;
  });
  await page.evaluate(
    ({ staleRows, staleCols }) => {
      const w = window as unknown as { __probe: WebSocket };
      w.__probe.send(new Uint8Array([0x02, staleRows >> 8, staleRows & 0xff, 0, staleCols]));
    },
    { staleRows, staleCols },
  );

  // The peer's FIRST answer to the claim. A >60-row echo after it can only be
  // the pane's reclaim write: on master the yield window reads this repeat of
  // the replayed grid as an insistent writer and stands down — the activation
  // loses to the grid it was meant to take, and no such echo ever comes.
  await page.evaluate(async ({ after }) => {
    const w = window as unknown as {
      __waitRows: (pred: (r: number) => boolean, after: number, label: string) => Promise<number>;
    };
    await w.__waitRows((r) => r > 60, after, ">60-row reclaim");
  }, { after });

  // And the emulator agrees: the re-asserted fit took the grid back.
  await expect.poll(rows, { timeout: 8000 }).toBe(grown);
});
