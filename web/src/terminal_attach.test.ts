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
    visibleFitFrame: null, visibleFitTimer: null, visibleFitAttempts: 0,
    // The real sendResize runs so its dedupe and hidden-page write guard are
    // exercised; only the socket write is stubbed.
    send: () => (calls.push("resize"), true),
    connect: () => calls.push("connect"),
  }) as {
    fitVisibleHost(): void;
    scheduleVisibleFit(): void;
    stopped: boolean;
    connect(): void;
    visibleFitFrame: number | null;
    visibleFitTimer: number | null;
    visibleFitAttempts: number;
    lastRows: number;
    lastCols: number;
    parked: boolean;
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
    // The withheld write keeps the delivery pair armed: whichever channel can
    // still deliver retries the flush — the frame on wake, the timer meanwhile.
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
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
    // write did not wait for a later trigger — the suppression kept the whole
    // delivery pair armed, so the flush does not depend on any one signal.
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [300, 50]);
    assert.notEqual(terminal.visibleFitFrame, null);
    assert.notEqual(terminal.visibleFitTimer, null);
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

// A send() that returns false dropped the frame on a socket that could not
// carry it. Crediting lastRows/lastCols anyway would erase the divergence
// marker — the daemon never got the grid but every later dedupe would see a
// converged state: the phantom-sent strand. The marker must stay stale and
// the delivery pair must keep retrying until a send actually lands.
test("a dropped resize is never credited — the owed write retries until it lands", async () => {
  const win = stubWindow();
  try {
    const { terminal, calls } = stage();
    terminal.lastRows = 300;
    terminal.lastCols = 50;
    Object.assign(terminal, { send: () => (calls.push("resize"), false) });
    terminal.scheduleVisibleFit();
    win.runFrame();
    await Promise.resolve();
    // The frame was attempted and dropped: no credit, and the pair re-armed.
    assert.deepEqual(calls, ["fit", "resize", "connect"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [300, 50]);
    assert.notEqual(terminal.visibleFitTimer, null);
    // A timer delivery marks the page parked — it suppresses the send rather
    // than attempting it — but the pair stays armed for the next delivery.
    win.runTimer();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize", "connect"]);
    assert.equal(terminal.parked, true);
    assert.notEqual(terminal.visibleFitTimer, null);
    // A frame-delivered attempt retries the write: still dropped, still owed.
    win.runFrame();
    await Promise.resolve();
    assert.deepEqual(calls, ["fit", "resize", "connect", "resize"]);
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [300, 50]);
    Object.assign(terminal, { send: () => (calls.push("resize"), true) });
    win.runFrame();
    await Promise.resolve();
    // The first send that actually lands converges the marker; the loop stops.
    assert.deepEqual([terminal.lastRows, terminal.lastCols], [40, 120]);
    assert.equal(terminal.visibleFitTimer, null);
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
