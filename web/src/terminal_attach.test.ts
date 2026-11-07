import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

register("./browser_stub_loader.mjs", import.meta.url);
const { AttachTerminal, terminalFontSize } = await import("./terminal.js");
const { setTerminalFontSizes } = await import("./terminal-font.js");
const { decode, Op } = await import("./frame.js");

// Exercise the real initial-fit/connection boundary without constructing xterm.
// Browser coverage below separately checks the actual surface and input wiring.
function stage() {
  const calls: string[] = [];
  const host = { clientWidth: 900, clientHeight: 600 };
  const term = { rows: 24, cols: 80 };
  const fit = {
    proposeDimensions: (): { rows: number; cols: number } | undefined => ({ rows: 40, cols: 120 }),
    fit: () => { Object.assign(term, { rows: 40, cols: 120 }); calls.push("fit"); },
  };
  const terminal = Object.assign(Object.create(AttachTerminal.prototype), {
    container: host, term, fit, stopped: false, initialConnectStarted: false,
    pendingViewport: null, lastRows: 0, lastCols: 0, parked: false,
    contestedEchoes: new Map<string, number>(), admittedEcho: true,
    owedForceResize: false,
    visibleFitFrame: null, visibleFitTimer: null, visibleFitAttempts: 0, resizeTimer: null,
    // The real sendResize runs so its dedupe and hidden-page write guard are
    // exercised; only the socket write is stubbed.
    send: () => (calls.push("resize"), true),
    connect: () => calls.push("connect"),
  }) as {
    fitVisibleHost(): void;
    scheduleVisibleFit(): void;
    stopped: boolean;
    connect(): void;
    scheduleFit(): void;
    resizeTimer: number | null;
    visibleFitFrame: number | null;
    visibleFitTimer: number | null;
    visibleFitAttempts: number;
    lastRows: number;
    lastCols: number;
    parked: boolean | null;
  };
  return { terminal, host, term, fit, calls };
}

/** A fake `window` for the scheduled-fit path: its rAF queues callbacks for
 *  manual delivery (or drops them — a parked page starves rAF outright) and its
 *  timers the same, so a test can deliver whichever twin it wants. */
function stubWindow() {
  const frames = new Map<number, () => void>();
  const timers = new Map<number, () => void>();
  let nextFrame = 1;
  let nextTimer = 1;
  const win = {
    requestAnimationFrame: (cb: () => void) => { const id = nextFrame++; frames.set(id, cb); return id; },
    cancelAnimationFrame: (id: number) => { frames.delete(id); },
    setTimeout: (cb: () => void) => { const id = nextTimer++; timers.set(id, cb); return id; },
    clearTimeout: (id: number) => { timers.delete(id); },
  };
  const prior = (globalThis as { window?: unknown }).window;
  (globalThis as { window?: unknown }).window = win;
  return {
    frames,
    timers,
    /** Delivers the pending rAF callback, if one is armed — or drops it (starved). */
    runFrame: () => { const cb = frames.values().next().value; frames.clear(); cb?.(); },
    /** Delivers EVERY armed rAF in arming order — one live paint honouring all
     *  queued callbacks, where runFrame models a single frame that starves the
     *  rest. */
    runAllFrames: () => {
      const cbs = [...frames.values()];
      frames.clear();
      for (const cb of cbs) cb();
    },
    runTimer: () => { const cb = timers.values().next().value; cb?.(); },
    restore: () => { (globalThis as { window?: unknown }).window = prior; },
  };
}

test("measured attach fits immediately and connects once after the owner can assign it", async () => {
  const { terminal, calls } = stage();
  terminal.fitVisibleHost();
  assert.deepEqual(calls, ["fit", "resize"]);
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize", "connect"]);
});

test("hidden or unresolved geometry waits for a later successful measurement", async () => {
  const { terminal, host, fit, calls } = stage();
  host.clientWidth = 0;
  terminal.fitVisibleHost();
  host.clientWidth = 900;
  const propose = fit.proposeDimensions;
  fit.proposeDimensions = () => undefined;
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, []);
  fit.proposeDimensions = propose;
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize", "connect"]);
});

test("a starved frame cannot lose the reclaim — the timer twin delivers it", () => {
  const win = stubWindow();
  try {
    const { terminal, calls } = stage();
    terminal.scheduleVisibleFit();
    assert.notEqual(terminal.visibleFitFrame, null);
    // Never deliver the frame — a page the compositor has parked starves rAF.
    // The armed timer carries the fit anyway: the DOM reclaims locally while
    // the wire write is withheld until a frame-coupled signal unparks it.
    win.runTimer();
    assert.deepEqual(calls, ["fit"]);
    assert.equal(terminal.parked, true);
    // The withheld write re-arms ONLY the frame probe — the channel whose
    // delivery can change the outcome. The timer is never armed for a write
    // that would just suppress again: a withheld resize is not a poll.
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
  } finally {
    win.restore();
  }
});

test("a scheduled fit that cannot yet measure retries instead of stranding", () => {
  const win = stubWindow();
  try {
    const { terminal, fit, calls } = stage();
    const propose = fit.proposeDimensions;
    fit.proposeDimensions = () => undefined;
    terminal.scheduleVisibleFit();
    win.runFrame();
    assert.deepEqual(calls, []);
    // The bail rearmed both triggers — one bad instant is not the last word.
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
    fit.proposeDimensions = propose;
    win.runFrame();
    assert.deepEqual(calls, ["fit", "resize"]);
  } finally {
    win.restore();
  }
});

// The timer twin delivers a scheduled fit even on a parked page — but a parked
// page must never write the shared PTY grid: with no user present its resize
// cannot be an activation, and it must never steal a live visible peer's grid.
// "Parked" is detected by the delivery channel — the starved rAF — which covers
// occluded pages that keep visibilityState='visible'; the document gate inside
// sendResize covers genuinely hidden pages for every caller.
test("a parked page's scheduled fit reclaims locally but never writes the grid", async () => {
  const win = stubWindow();
  try {
    const { terminal, term, calls } = stage();
    // A peer's 300x50 echo owns the grid: term follows it, and lastRows/lastCols
    // record that the daemon's size came from the peer — this client is inactive.
    Object.assign(term, { rows: 300, cols: 50 });
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    terminal.scheduleVisibleFit();
    win.runTimer(); // starved rAF → parked delivery: local reclaim only
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect"]);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
    // Nothing was written: the daemon's grid is still the peer's. And the owed
    // write did not wait for a later trigger — the suppression armed the frame
    // probe, whose delivery IS the wake, so the flush needs no external signal.
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [300, 50]);
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
  } finally {
    win.restore();
  }
});

test("the owed write flushes on the wake probe — no external trigger needed", async () => {
  const win = stubWindow();
  try {
    const { terminal, term, calls } = stage();
    Object.assign(term, { rows: 300, cols: 50 });
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    terminal.scheduleVisibleFit();
    win.runTimer();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect"]);
    assert.equal(terminal.parked, true);
    // No scheduleVisibleFit, no input, no visibilitychange — the suppressed
    // write armed a frame whose delivery alone flushes it. This is the
    // deterministic reactivation path a merely-slow page needs.
    win.runFrame();
    await Promise.resolve();
    // xterm needed no refit — the send is purely the owed convergence flush.
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
  }
});

// The wake frame IS a scheduled fit's delivery once it lands — a transient
// measurement bail on it must retry through the delivery pair, not strand the
// owed write. The parked fit that armed the probe reported settled and used to
// spend the fit budget to zero, so a probe carrying no budget made the first
// wake bail final: the pane stayed on the peer's grid until an unrelated
// activation signal.
test("a transient bail on the wake frame retries — the probe carries a fit budget", async () => {
  const win = stubWindow();
  try {
    const { terminal, term, fit, calls } = stage();
    Object.assign(term, { rows: 300, cols: 50 });
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    terminal.scheduleVisibleFit();
    win.runTimer(); // starved rAF → parked delivery: local reclaim, write withheld, probe armed
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect"]);
    assert.equal(terminal.parked, true);
    // The settled parked fit must not spend the probe's budget — zeroing it
    // while the probe stayed armed is what made the first wake bail final.
    assert.equal(terminal.visibleFitAttempts > 0, true);
    // The probe's first delivery meets xterm mid-invalidation — undefined
    // dimensions used to decrement the spent budget and end, leaving the
    // peer's grid in place. With a seeded budget it re-arms the pair.
    const propose = fit.proposeDimensions;
    fit.proposeDimensions = () => undefined;
    win.runFrame();
    await Promise.resolve();
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "connect"]);
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
    // The retry delivers: the owed write finally flushes the local grid.
    fit.proposeDimensions = propose;
    win.runFrame();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
  }
});

// Codex P2 on #5501: before the first probe delivery the parked state is
// UNKNOWN, not "not parked" — an occluded page still reports
// visibilityState='visible' while its frames starve, so a socket that opens
// inside the ~250ms window (the onopen force-resize) must defer to the
// classification rather than leak a write on the default reading. Whichever
// twin delivers first then runs the fit that carries — or withholds — the
// size; the un-credited marker is what makes that fit converge on its own.
test("a socket opening inside the probe window writes nothing until classified", async () => {
  const win = stubWindow();
  const prior = (globalThis as { document?: unknown }).document;
  (globalThis as { document?: unknown }).document = { visibilityState: "visible" };
  try {
    const { terminal, calls } = stage();
    // Freshly constructed: the constructor's probe pair is armed but no twin
    // has delivered yet — the occluded-but-visible state under test.
    terminal.parked = null;
    terminal.scheduleVisibleFit();
    // The socket opens inside the probe window — exactly what onopen does.
    (terminal as unknown as { sendResize(r: number, c: number, f: boolean): void })
      .sendResize(24, 80, true);
    assert.deepEqual(calls, [], "an unclassified terminal must not write the shared grid");
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [0, 0],
      "the un-credited marker is what lets the classified fit converge");
    // The starved twin classifies the page parked — the owed write stays
    // withheld (the wake frame stays armed for it), not leaked.
    win.runTimer();
    await Promise.resolve();
    assert.equal(terminal.parked, true);
    assert.deepEqual(calls, ["fit", "connect"]);
    // The first delivered frame — the un-occlusion — flushes the owed grid.
    win.runFrame();
    await Promise.resolve();
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
    (globalThis as { document?: unknown }).document = prior;
  }
});

// An owed resize re-arms only channels that can change the outcome. On a
// parked page a timer delivery can change nothing — it would fit, withhold,
// and re-arm forever: a perpetual 250ms poll on a dormant page. The probe is
// the deferred rAF alone: it costs nothing while the page cannot paint and
// its delivery IS the reactivation.
test("a parked terminal's owed write arms the frame probe only — never a timer poll", async () => {
  const win = stubWindow();
  try {
    const { terminal, term, calls } = stage();
    Object.assign(term, { rows: 300, cols: 50 });
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    terminal.scheduleVisibleFit();
    win.runTimer(); // starved rAF → parked delivery: local reclaim, write withheld
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect"]);
    assert.equal(terminal.parked, true);
    // The withheld write armed a frame probe and NO timer.
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    assert.equal(win.timers.size, 0);
    // Over N timer periods nothing fires — the parked page is quiescent.
    win.runTimer();
    win.runTimer();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "connect"]);
    // The first delivered frame is the wake — it alone flushes the owed write.
    win.runFrame();
    await Promise.resolve();
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
  }
});

// A send() that returns false dropped the frame on a socket that could not
// carry it. Crediting lastRows/lastCols anyway would erase the divergence
// marker — the daemon never got the grid but every later dedupe would see a
// converged state: the phantom-sent strand. The marker must stay stale — and
// NOTHING is armed: socket-open is the only channel that can change a dead
// socket's outcome, so the connect path's force-send on open owns the flush
// rather than the page polling send() forever.
test("a dropped resize is never credited — the connect-time reassert owns the flush", async () => {
  const win = stubWindow();
  try {
    const { terminal, term, calls } = stage();
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    Object.assign(terminal, { send: () => (calls.push("resize"), false) });
    terminal.scheduleVisibleFit();
    win.runFrame();
    await Promise.resolve();
    // The frame was attempted and dropped: no credit, no re-arm — the write
    // stays owed on the marker alone until the socket can carry it.
    assert.deepEqual(calls, ["fit", "resize", "connect"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [300, 50]);
    assert.equal(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    assert.equal(win.timers.size, 0);
    // The connect-time reassert is what onopen performs: a force sendResize of
    // the current size. It carries the owed grid and converges the marker.
    Object.assign(terminal, { send: () => (calls.push("resize"), true) });
    (terminal as unknown as { sendResize(r: number, c: number, f: boolean): void })
      .sendResize(term.rows, term.cols, true);
    assert.deepEqual(calls, ["fit", "resize", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
  }
});

test("a document-hidden page never writes the grid, whatever the delivery channel", () => {
  const win = stubWindow();
  const prior = (globalThis as { document?: unknown }).document;
  (globalThis as { document?: unknown }).document = { visibilityState: "hidden" };
  try {
    const { terminal, calls } = stage();
    terminal.scheduleVisibleFit();
    win.runFrame(); // even a frame-delivered fit cannot write while hidden
    assert.deepEqual(calls, ["fit"]);
  } finally {
    win.restore();
    (globalThis as { document?: unknown }).document = prior;
  }
});

test("a hidden pane reports settled — retries are not spent on it", () => {
  const win = stubWindow();
  try {
    const { terminal, host, calls } = stage();
    host.clientHeight = 0;
    terminal.scheduleVisibleFit();
    win.runFrame();
    assert.deepEqual(calls, []);
    // No requeue: a zero-size pane is genuinely hidden, and the intersection
    // observer already owns refitting it when it comes back.
    assert.equal(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    assert.equal(win.timers.size, 0);
  } finally {
    win.restore();
  }
});

test("disposal before the queued attach prevents socket construction and status callbacks", async () => {
  const { terminal, calls } = stage();
  terminal.connect = (AttachTerminal.prototype as unknown as { connect(): void }).connect;
  terminal.fitVisibleHost();
  terminal.stopped = true;
  // Real connect() must return before touching any absent browser dependencies.
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize"]);
});

// Codex review on #4983: xterm's DEFAULT mouse encoding (wheel-capable tracking
// without SGR 1006) emits reports on onBinary — a byte string, one char per
// byte — which used to go nowhere because only onData was forwarded.
test("binary output reaches the socket byte-for-byte, never UTF-8 split", () => {
  const sent: Uint8Array[] = [];
  const t = Object.assign(Object.create(AttachTerminal.prototype), {
    stopped: false,
    exited: false,
    send: (frame: Uint8Array) => (sent.push(frame), true),
  }) as { sendBinary(data: string): void };
  // A wheel-up report at col 101, row 115: every coordinate byte is ≥ 0x80,
  // exactly what a UTF-8 pass would have split into two.
  t.sendBinary("\x1b[M\x60\x85\x93");
  assert.equal(sent.length, 1);
  const frame = decode(sent[0]);
  assert.equal(frame.op, Op.Input);
  assert.deepEqual(Array.from(frame.data), [0x1b, 0x5b, 0x4d, 0x60, 0x85, 0x93]);
});

// #5492: phones open at half the desktop pitch so a ~390px viewport fits a
// usable grid. The two ends pin both acceptance halves — desktop stays 13,
// phone is exactly 50% of it.
test("the phone breakpoint halves the terminal's default font size", () => {
  assert.equal(terminalFontSize(false), 13);
  assert.equal(terminalFontSize(true), terminalFontSize(false) / 2);
});

test("a phone-boundary crossing re-picks the pitch and refits once", () => {
  const term = { options: { fontSize: 13 } };
  const calls: string[] = [];
  const terminal = Object.assign(Object.create(AttachTerminal.prototype), {
    term,
    phone: { matches: true },
    scheduleVisibleFit: () => calls.push("refit"),
  }) as { applyDeviceFontSize(): void };
  terminal.applyDeviceFontSize();
  assert.equal(term.options.fontSize, 6.5);
  assert.deepEqual(calls, ["refit"]);
  // A repeat that changes nothing must not schedule a no-op fit.
  terminal.applyDeviceFontSize();
  assert.deepEqual(calls, ["refit"]);
});

// #5500: the connect-time config read lands the persisted sizes before the
// first attach, so the xterm constructor — and the grid the initial OpResize
// reports — reflects the configured pitch, never a default-then-jump. The
// constructor reads terminalFontSize through the same accessor this asserts.
test("a persisted config size is what a fresh attach constructs with", () => {
  setTerminalFontSizes({ phone: 9, desktop: 15 });
  try {
    assert.equal(terminalFontSize(true), 9);
    assert.equal(terminalFontSize(false), 15);
    const term = { options: { fontSize: 9 } };
    const calls: string[] = [];
    const terminal = Object.assign(Object.create(AttachTerminal.prototype), {
      term,
      phone: { matches: false },
      scheduleVisibleFit: () => calls.push("refit"),
    }) as { applyDeviceFontSize(): void };
    terminal.applyDeviceFontSize();
    assert.equal(term.options.fontSize, 15,
      "a crossing to desktop picks the configured desktop pitch, not the default");
    assert.deepEqual(calls, ["refit"]);
  } finally {
    setTerminalFontSizes({ phone: 6.5, desktop: 13 });
  }
});

// #6215: the observer reports a resize once; if xterm cannot measure at that instant
// the debounced fit must not be the end of it.
test("a resize whose fit cannot measure is retried, not dropped", () => {
  const win = stubWindow();
  try {
    const { terminal, host, fit, term, calls } = stage();
    const propose = fit.proposeDimensions;
    fit.proposeDimensions = () => undefined; // renderer metrics mid-invalidation
    host.clientHeight = 720; // the taller viewport
    terminal.scheduleFit();
    win.runTimer(); // the debounce fires; the measurement fails
    assert.deepEqual(calls, [], "nothing fitted yet");
    assert.notEqual(terminal.visibleFitFrame, null, "a retry is armed");
    win.runFrame(); // still unmeasurable: the retry budget keeps it alive
    assert.notEqual(terminal.visibleFitFrame, null, "the retry persists across a failed attempt");
    fit.proposeDimensions = propose; // metrics settle
    win.runFrame();
    assert.deepEqual(calls, ["fit", "resize"], "the new grid is fitted and sent to the PTY");
    assert.equal(term.rows, 40);
  } finally {
    win.restore();
  }
});

test("a fit that throws between proposal and fit (detached mid-resize) is retried", () => {
  const win = stubWindow();
  try {
    const { terminal, fit, calls } = stage();
    const realFit = fit.fit;
    let first = true;
    fit.fit = () => { if (first) { first = false; throw new Error("detached"); } realFit(); };
    terminal.scheduleFit();
    win.runTimer();
    assert.deepEqual(calls, []);
    win.runFrame();
    assert.deepEqual(calls, ["fit", "resize"]);
  } finally {
    win.restore();
  }
});

test("a resize event during a pending retry is not swallowed: the retry reads the latest host", () => {
  const win = stubWindow();
  try {
    const { terminal, host, fit, term, calls } = stage();
    const propose = fit.proposeDimensions;
    fit.proposeDimensions = () => undefined;
    terminal.scheduleFit();
    win.runTimer(); // failed, retry armed
    host.clientHeight = 900; // a second resize lands before the retry runs
    terminal.scheduleFit(); // its own debounce
    fit.proposeDimensions = () => ({ rows: host.clientHeight / 15, cols: 120 });
    fit.fit = () => { Object.assign(term, fit.proposeDimensions()); calls.push("fit"); };
    win.runFrame(); // the armed retry fits the latest geometry
    assert.equal(term.rows, 60);
    assert.deepEqual(calls.slice(0, 2), ["fit", "resize"]);
    fit.proposeDimensions = propose;
  } finally {
    win.restore();
  }
});

// #6215: an authoritative echo carrying a grid no live client asked for — a
// hidden sibling's reconnect force-send, the broker re-adopting the pane's
// tmux-measured size, a stale belief replayed to a fresh subscriber — reverts
// the emulator and, left alone, strands the active pane on the wrong grid
// until an unrelated signal refits it. A staged terminal with a real buffer
// face exercises the whole echo→reclaim→send path.
function stageEchoed() {
  const staged = stage();
  const { terminal, term } = staged;
  Object.assign(term, {
    resize: (cols: number, rows: number) => Object.assign(term, { rows, cols }),
    buffer: { active: { viewportY: 0, baseY: 0 } },
    registerMarker: () => ({ dispose() {} }),
    scrollToLine: () => {},
    viewportY: 0,
    baseY: 0,
  });
  // Settled on this host's grid and already connected: the last-known size is
  // ours, not a peer's.
  Object.assign(terminal, { initialConnectStarted: true });
  terminal.lastRows = 40;
  terminal.lastCols = 120;
  Object.assign(term, { rows: 40, cols: 120 });
  return {
    ...staged,
    echo: (rows: number, cols: number) =>
      (terminal as unknown as { applyEchoedSize(r: number, c: number): void })
        .applyEchoedSize(rows, cols),
  };
}

// #6215 (the 2026-11 reopening): a taller mobile viewport's grown poll read
// the emulator at a FOREIGN grid — the subscribe admission had replayed the
// broker's stale belief (the previous attach's grown size) and the deferred
// reclaim left the pane displaying it for a frame or more. The grown host
// happened to fit the same grid, so `rows > beforeResizeRows` could never
// pass even though the pane was correctly fit. A foreign grid must therefore
// never REST on a measurable host's emulator: the echo is applied per the
// wire contract and the host's own fit is re-asserted in the same task.
//
// But the wire reclaim is a separate matter (Codex P2): an echo is a network
// event and can land on an OCCLUDED page whose stale `parked === false` still
// reads live. Writing on it would resize the shared PTY out from under the
// active peer — so the reclaim's wire half defers to a fresh classification
// even while the local refit stays synchronous.
test("a foreign echo re-asserts the host's grid in the same task — and writes only after classification", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    // A stale 104x68 belief — the grown-grid replay from the #6215 trace —
    // moves the emulator off this host's 40x120 fit. On master the pane kept
    // displaying 104 rows until the scheduled-fit pair delivered a frame or
    // the 250ms twin fired; any DOM row count in that window sampled the
    // foreign grid as this pane's own. The local reclaim lands synchronously.
    echo(104, 68);
    assert.deepEqual([term.rows, term.cols], [40, 120],
      "the emulator is back on the host's fit before echo() returns");
    // The wire write did NOT leave yet: the stale 'live' was downgraded to
    // unclassified, and the classification pair — not a live-page assumption —
    // is what flushes it.
    assert.deepEqual(calls, ["fit"]);
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
    // The starved twin's delivery classifies this page parked — the write
    // stays withheld, and the wake probe alone owns the flush (no timer poll).
    win.runFrame(); // drains the viewport-restore frame; eats the pair's rAF
    win.runTimer();
    assert.equal(terminal.parked, true);
    assert.deepEqual(calls, ["fit"], "an occluded page never resizes the shared PTY");
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    // The probe's delivery — the un-occlusion — flushes the owed reclaim.
    win.runFrame();
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
  } finally {
    win.restore();
  }
});

// The live-page half of the same contract: when the classification pair's
// frame actually delivers, the reclaim's write flushes one delivery later —
// the local refit never waited for it.
test("a live page's echo reclaim writes as soon as classification delivers", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    echo(104, 68);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
    assert.deepEqual(calls, ["fit"]);
    win.runAllFrames(); // one live paint: restore frame + the pair's rAF
    await Promise.resolve();
    assert.equal(terminal.parked, false);
    assert.deepEqual(calls, ["fit", "resize"],
      "the classified write is the only deferred part");
  } finally {
    win.restore();
  }
});

// A page already classified parked keeps every other guarantee: the local
// refit still lands synchronously, and the withheld write arms the frame
// probe alone — never a timer that would poll a dormant page.
test("a known-parked page's echo refits locally and arms only the wake probe", () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    terminal.parked = true;
    echo(104, 68);
    assert.deepEqual([term.rows, term.cols], [40, 120],
      "the local reclaim is still synchronous");
    assert.deepEqual(calls, ["fit"], "nothing may leave the wire while parked");
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    assert.equal(win.timers.size, 0);
  } finally {
    win.restore();
  }
});

// #6808/#6812 — the observation that reverted the classification gate: the
// re-parked tab's DOM resting on the peer's grid (the old spec's "expected
// 263" assertion). It was the leak's artifact, not a property of parking.
// With the wire write gated on a fresh classification, a stale-'live' parked
// page answers a foreign echo with its local refit alone, so it never
// triggers the peer's re-claim and no second echo of that grid exists to
// re-apply it. Resting on the foreign grid is reachable only through
// applyEchoedSize itself: a repeat echo inside the yield window is an
// insistent writer the contested guard lets stand. On the un-gated path the
// first echo's leaked OpResize was what manufactured that repeat.
test("a re-parked page rests on the peer's grid only via a second echo — the strand the leak manufactured", () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    echo(104, 68);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
    assert.deepEqual(calls, ["fit"], "the echo-path reclaim wrote nothing");
    // The withheld write owes the classification pair; the starved twin's
    // delivery parks the page, and the emulator is STILL on the host's grid —
    // no strand exists.
    win.runTimer();
    assert.equal(terminal.parked, true);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
    // The counterfactual the old assertion encoded: the leaked OpResize made
    // the live peer re-claim its grid, and that re-claim's echo landed inside
    // the yield window — an insistent writer that stands unreclaimed. The DOM
    // resting on the foreign grid is the second echo's signature; nothing
    // parked-side produces it.
    echo(104, 68);
    assert.deepEqual([term.rows, term.cols], [104, 68],
      "a repeat of the same foreign grid inside the yield window stands — the 263 the old spec asserted");
    assert.deepEqual(calls, ["fit"], "the yielded strand writes nothing either");
  } finally {
    win.restore();
  }
});

// The synchronous reclaim can only run when the host already measures: a
// foreign echo that arrives while xterm's metrics are mid-invalidation falls
// back to the scheduled-fit pair, whose retry budget carries the reclaim to
// the first measurable frame.
test("a foreign echo the host cannot yet measure is reclaimed through the scheduled pair", async () => {
  const win = stubWindow();
  try {
    const { echo, term, fit, calls, terminal } = stageEchoed();
    const propose = fit.proposeDimensions;
    fit.proposeDimensions = () => undefined;
    echo(24, 80);
    // The echo still applies verbatim; the pair owns the deferred retry.
    assert.deepEqual([term.rows, term.cols], [24, 80]);
    assert.deepEqual(calls, []);
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
    fit.proposeDimensions = propose;
    win.runFrame();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize"]);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
  } finally {
    win.restore();
  }
});

test("the same foreign grid repeated inside the yield window is an insistent writer — it wins", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    echo(24, 80);
    // The reclaim's write waits on a fresh classification — one live paint
    // delivers the pair's rAF and flushes it.
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize"]);
    // The foreign writer immediately re-asserts the same grid: the reclaim
    // loses to last-resize-wins rather than ping-ponging between clients.
    echo(24, 80);
    assert.deepEqual([term.rows, term.cols], [24, 80], "the emulator still follows the authoritative echo");
    assert.deepEqual(calls, ["fit", "resize"], "no second reclaim leaves the wire");
    assert.equal(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
  } finally {
    win.restore();
  }
});

test("a foreign echo on a hidden pane follows the grid but schedules no reclaim", () => {
  const win = stubWindow();
  try {
    const { echo, term, host, terminal } = stageEchoed();
    host.clientWidth = 0; // a session-first sibling hidden under CSS
    echo(24, 80);
    assert.deepEqual([term.rows, term.cols], [24, 80]);
    assert.equal(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
    assert.equal(win.frames.size, 0);
    assert.equal(win.timers.size, 0);
  } finally {
    win.restore();
  }
});

test("the same foreign grid after the yield window is a fresh contest — one stale write cannot strand the pane for good", async () => {
  const win = stubWindow();
  const priorNow = Date.now;
  let now = 10_000;
  Date.now = () => now;
  try {
    const { echo, term, calls } = stageEchoed();
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    echo(24, 80); // insistent: yielded, the pane stays on the foreign grid
    now += 1600;
    Object.assign(term, { rows: 40, cols: 120 }); // our own fit converged meanwhile
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize", "fit", "resize"]);
    assert.deepEqual([term.rows, term.cols], [40, 120]);
  } finally {
    Date.now = priorNow;
    win.restore();
  }
});

// #6635 (the #2347 recurrence): a freshly attached pane's FIRST foreign echo is
// the broker's subscribe replay — the grid the world already had, not a live
// peer answering a claim the pane has not yet made. Recording it as a contest
// primed the yield window, so the peer's first real answer to the pane's claim
// read as a repeat and won unchallenged: the activation stood down to the grid
// it was meant to take. The admission echo must earn the refit without seeding
// the contest record.
test("the subscribe admission echo is not a contested sighting — the activation answers the peer's first real write", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    // A pane whose socket just opened: nothing has been admitted yet.
    Object.assign(terminal, { admittedEcho: false });
    // The broker's replayed authority lands: the emulator takes it and the
    // activation refit is owed — but no contest is recorded.
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize"], "the admission echo still earns the host's claim");
    assert.deepEqual([term.rows, term.cols], [40, 120]);
    // The peer's FIRST real answer to that claim is a brand-new contest, not a
    // repeat: the activation re-asserts instead of yielding to the replayed
    // grid (on master this echo was consumed by the primed window and nothing
    // left the wire — the stranded-34 failure).
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize", "fit", "resize"]);
    // Only NOW is a same-grid repeat insistence: it wins unchallenged.
    echo(24, 80);
    assert.deepEqual([term.rows, term.cols], [24, 80], "the emulator still follows the authoritative echo");
    assert.deepEqual(calls, ["fit", "resize", "fit", "resize"], "an insistent peer still wins");
  } finally {
    win.restore();
  }
});

// #6635 (the 2026-11-06 #2347 recurrence, run 42219554428): the admission flag
// was spent only on a TERM-DIVERGENT echo — so a client that already owned the
// authority (the whole reason the replay matched its emulator) left the flag
// standing for its first FOREIGN echo: a live peer write, not the replay. That
// write took the admission's free pass without seeding the contest record, so
// the peer's counter to our claim read as a fresh sighting — the pane re-claimed
// where it must yield — and the reclaim it landed on the peer was the repeat
// the peer then yielded to: the stranded-34 signature. The admission is the
// first DELIVERED echo, divergent or not.
test("a replay that already matches the emulator still spends the admission — the peer's counter is a yield, not a fresh contest", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    // A pane whose socket just opened while it already owned the authority:
    // the broker replays its own 40x120 grid — a no-op for the emulator.
    Object.assign(terminal, { admittedEcho: false });
    echo(40, 120);
    assert.deepEqual(calls, [], "the replay changes nothing on the emulator");
    // The peer's first real write is a contested sighting, not the replay: it
    // seeds the record and earns the reclaim. On master this echo instead
    // consumed the leftover admission — nothing was seeded, and its counter
    // below read fresh rather than insistent.
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize"]);
    // The peer's counter inside the yield window is the insistent writer: the
    // pane yields, emulator AND wire. On master the un-seeded record made it a
    // fresh contest — a second reclaim left the wire, which the peer read as
    // the repeat and yielded to.
    echo(24, 80);
    assert.deepEqual([term.rows, term.cols], [24, 80],
      "the emulator follows the authoritative repeat");
    assert.deepEqual(calls, ["fit", "resize"],
      "no second reclaim leaves the wire");
  } finally {
    win.restore();
  }
});

// The same yield must also disarm whatever the forfeited claim left armed: the
// deferred wire half of a contested reclaim, or a parked page's wake probe.
// Delivered after the yield, that work would refit the emulator off the
// authoritative grid and write the conceded size back out — the pane re-claiming
// by accident the grid it just stood down from.
test("a yield disarms the forfeited reclaim — no queued delivery may write the conceded grid", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    echo(24, 80);
    // The contested reclaim already refit locally; its wire half is armed and
    // has not delivered yet when the peer's repeat lands.
    assert.deepEqual(calls, ["fit"]);
    assert.notEqual(terminal.visibleFitFrame, null);
    echo(24, 80);
    assert.deepEqual([term.rows, term.cols], [24, 80], "the pane yields to the repeat");
    // The armed delivery died with the claim it carried: a frame now would
    // refit the emulator off the yielded grid AND flush the conceded write —
    // a stale claim landed by the forfeiting side.
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit"], "nothing delivers after the yield");
    assert.deepEqual([term.rows, term.cols], [24, 80],
      "the emulator rests on the yielded authority");
    assert.equal(terminal.visibleFitFrame, null);
    assert.equal(terminal.visibleFitTimer, null);
  } finally {
    win.restore();
  }
});

test("alternating foreign grids cannot bypass the yield window — contest history is per-grid", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls } = stageEchoed();
    // Three visible clients fitting the same PTY to different hosts: a
    // last-only contest record is overwritten by each intervening grid, so
    // every echoed grid looks new and every echo earns a reclaim — an
    // indefinite resize ping-pong (Codex P2 on #6628).
    echo(24, 80);
    win.runAllFrames();
    await Promise.resolve();
    echo(30, 90);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize", "fit", "resize"], "each new foreign grid gets one reclaim");
    // A repeat of EITHER grid inside its own window is the same insistent
    // writer — it wins unchallenged, whichever order the echoes arrive in.
    echo(24, 80);
    echo(30, 90);
    assert.deepEqual([term.rows, term.cols], [30, 90], "the emulator still follows the authoritative echo");
    assert.deepEqual(calls, ["fit", "resize", "fit", "resize"], "no further reclaims leave the wire");
  } finally {
    win.restore();
  }
});

// #6635 (run 42370084465 — "pointer activation must refit … rows 109"): the
// contested record is wall-clock only, so a foreign grid sighted again inside
// FOREIGN_ECHO_YIELD_MS counts as insistent no matter when it was first seen —
// including sightings from BEFORE the user's activation, which are not answers
// to the claim the activation is about to produce but the previous contest's
// echoes arriving late. In the run the pane had yielded to the peer's repeat,
// the user's pointer activation re-claimed, and the peer's counter ~16ms later
// read as the same contest's next repeat (~820ms from the pre-claim sighting),
// stranding the now-active pane on the peer's rows. A user-coupled activation
// starts a new contest epoch: the next foreign write is that epoch's FIRST
// sighting, contested like any fresh claim — while the concession itself still
// holds: a SECOND answer inside the new window is insistence and still wins.
test("a trusted activation starts a new contest epoch — the pre-activation yield record cannot stand the new claim down", async () => {
  const win = stubWindow();
  try {
    const { echo, term, calls, terminal } = stageEchoed();
    const noteActivation = (terminal as unknown as {
      noteActivation(): void;
    }).noteActivation.bind(terminal);
    const resizes = () => calls.filter((c) => c === "resize").length;

    // The peer's claim: the pane's first contested sighting of 34x100, answered
    // by one reclaim write once classification delivers.
    echo(34, 100);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize"]);
    // The peer's repeat inside the window: the pane yields and its emulator
    // rests on the peer's grid — the record stays stamped at the FIRST
    // sighting.
    echo(34, 100);
    assert.deepEqual([term.rows, term.cols], [34, 100]);
    assert.equal(resizes(), 1);

    // The user activates: a trusted gesture — noteActivation + fitVisibleHost
    // is exactly what onPointerEnter does for a trusted event. On any version
    // the yielded emulator no longer fits, so the refit's claim is on the
    // wire; only the fix also clears the stale sighting history.
    noteActivation();
    terminal.fitVisibleHost();
    assert.equal(terminal.parked, false);
    assert.equal(resizes(), 2, "the activation's own claim went out");
    assert.deepEqual([term.rows, term.cols], [40, 120]);

    // The peer's answer inside the OLD window: on master the pre-activation
    // sighting still counts, so this is a repeat and the pane yields to the
    // very write it was activated to take — the strand (34 rows, 2 writes).
    // With the epoch restarted at the gesture it is the first sighting of a
    // fresh contest: the pane re-asserts and the deferred write flushes on
    // the next classified frame.
    echo(34, 100);
    win.runAllFrames();
    await Promise.resolve();
    assert.deepEqual([term.rows, term.cols], [40, 120],
      "the activated pane reclaims instead of yielding to a stale repeat");
    assert.equal(resizes(), 3,
      "fresh contest → the reclaim write actually leaves (master: strand, 2 writes)");

    // The concession still holds inside the NEW epoch: a second answer to the
    // same grid within the window is insistence and wins unchallenged.
    echo(34, 100);
    assert.deepEqual([term.rows, term.cols], [34, 100],
      "an insistent peer still wins the new epoch");
    assert.equal(resizes(), 3, "the yield writes nothing");
  } finally {
    win.restore();
  }
});
