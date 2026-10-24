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
    pendingViewport: null,
    visibleFitFrame: null, visibleFitTimer: null, visibleFitAttempts: 0,
    sendResize: () => calls.push("resize"),
    connect: () => calls.push("connect"),
  }) as {
    fitVisibleHost(): void;
    scheduleVisibleFit(): void;
    stopped: boolean;
    connect(): void;
    visibleFitFrame: number | null;
    visibleFitTimer: number | null;
    visibleFitAttempts: number;
  };
  return { terminal, host, fit, calls };
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
    // The armed timer carries the fit anyway.
    win.runTimer();
    assert.deepEqual(calls, ["fit", "resize"]);
    assert.equal(terminal.visibleFitFrame, null);
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
