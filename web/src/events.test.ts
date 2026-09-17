// Tests for the /v1/events subscriber's reconnect/resync contract (#1592 Phase 5
// PR5). The load-bearing assertion is the FIRST-open resync: connect() takes its
// seed Snapshot BEFORE this socket opens, so a mutation in that window would be
// lost unless the stream re-Snapshots once the socket is subscribed. This pins
// that onResync fires on the first open (not only on reconnects), closing the
// login-window race — with no browser, using a tiny controllable WebSocket mock.

import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";

import { EventStream } from "./events.js";

// A minimal WebSocket stand-in the test drives: it records the URL, exposes the
// handlers events.ts assigns, and lets the test fire open/close synchronously.
class MockWebSocket {
  static instances: MockWebSocket[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((e: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;
  onerror: (() => void) | null = null;
  closed = false;

  constructor(public url: string) {
    MockWebSocket.instances.push(this);
  }
  close(): void {
    this.closed = true;
  }
  fireOpen(): void {
    this.onopen?.();
  }
  fireClose(): void {
    this.onclose?.();
  }
}

// Deterministic timer control so the reconnect backoff fires exactly when the test
// wants, without wall-clock waits.
type Timer = { id: number; fn: () => void };
let timers: Timer[] = [];
let nextTimerId = 1;

function flushTimers(): void {
  const due = timers;
  timers = [];
  for (const t of due) {
    t.fn();
  }
}

beforeEach(() => {
  MockWebSocket.instances = [];
  timers = [];
  nextTimerId = 1;
  const win = {
    location: { protocol: "https:", host: "af.example.com" },
    setTimeout: (fn: () => void): number => {
      const id = nextTimerId++;
      timers.push({ id, fn });
      return id;
    },
    clearTimeout: (id: number): void => {
      timers = timers.filter((t) => t.id !== id);
    },
  };
  (globalThis as unknown as { window: unknown }).window = win;
  (globalThis as unknown as { WebSocket: unknown }).WebSocket = MockWebSocket;
});

afterEach(() => {
  delete (globalThis as unknown as { window?: unknown }).window;
  delete (globalThis as unknown as { WebSocket?: unknown }).WebSocket;
});



test("onResync fires on the FIRST open, closing the login-window race", () => {
  let resyncs = 0;
  const statuses: string[] = [];
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {
      resyncs++;
    },
    onStatus: (s) => statuses.push(s),
    onAuthFailure: () => {},
  });
  stream.start();

  assert.equal(MockWebSocket.instances.length, 1, "one socket opened on start");
  assert.equal(resyncs, 0, "no resync before the socket opens");

  MockWebSocket.instances[0]!.fireOpen();
  assert.equal(resyncs, 1, "first open triggers a resync (the race fix)");
  assert.deepEqual(statuses, ["connecting", "open"]);

  stream.stop();
});

test("onResync fires again on every reconnect open", () => {
  let resyncs = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {
      resyncs++;
    },
    onStatus: () => {},
    onAuthFailure: () => {},
  });
  stream.start();
  MockWebSocket.instances[0]!.fireOpen();
  assert.equal(resyncs, 1, "first open");

  // Drop the socket → a reconnect is scheduled; fire the backoff timer to reopen.
  MockWebSocket.instances[0]!.fireClose();
  flushTimers();
  assert.equal(MockWebSocket.instances.length, 2, "reconnect opened a new socket");
  MockWebSocket.instances[1]!.fireOpen();
  assert.equal(resyncs, 2, "reconnect open triggers another resync");

  stream.stop();
});

test("the subscribe URL carries the token as ?access_token and rides wss under https", () => {
  const stream = new EventStream("secret tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {},
  });
  stream.start();
  const url = MockWebSocket.instances[0]!.url;
  assert.match(url, /^wss:\/\/af\.example\.com\/v1\/events\?access_token=secret%20tok$/);
  stream.stop();
});

// --- WS upgrade auth rejection (#1674 escalation) ----------------------------
//
// The bug: a WS upgrade whose access_token the daemon rejects (HTTP 401) fires
// onclose(1006) with NO onopen, so the old EventStream scheduled a reconnect
// with the same revoked credential forever and never told index.ts. The fix
// surfaces that signature: EventStream counts consecutive close-before-open
// reconnects, and once past a small threshold (AUTH_FAILURE_THRESHOLD) fires
// onAuthFailure ONCE so the caller can issue an authenticated requestResync
// that, on 401, trips shouldForgetToken -> disconnect() and returns the SPA
// to login. These tests drive the MockWebSocket with fireClose() WITHOUT a
// preceding fireOpen() to model the rejected-upgrade signature: the browser
// surfaces a 401 as onclose(1006) and no onopen ever fires for that attempt.
//
// A tiny harness for "fail the next reconnect attempt without opening": runs
// the armed backoff timer (creating a fresh socket) then immediately closes
// it before onopen. Equivalent to `flushTimers(); inst.fireClose()` but pulled
// out because every escalation test repeats it. Tracks nothing itself; the
// test reads MockWebSocket.instances for the count.
function failReconnectBeforeOpen(): void {
  flushTimers();
  MockWebSocket.instances[MockWebSocket.instances.length - 1]!.fireClose();
}

test("escalates to onAuthFailure after the WS upgrade closes before open past the threshold (#1674)", () => {
  let authFailures = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {
      authFailures += 1;
    },
  });
  stream.start();
  // Happy-path open: the first socket opens (so `everOpened` becomes true, as
  // it is in the bug's representative sequence — a logged-in client whose WS
  // had been healthy before the operator rotated the token).
  MockWebSocket.instances[0]!.fireOpen();
  // A transport drop closes the live socket. `opened=true` for this attempt, so
  // it is NOT a close-before-open and must NOT count toward the streak.
  MockWebSocket.instances[0]!.fireClose();
  assert.equal(authFailures, 0, "a normal close after an open must not escalate");

  // The daemon now rejects every reconnect upgrade (HTTP 401 -> close 1006,
  // no onopen). AUTH_FAILURE_THRESHOLD is 3: two close-before-opens below the
  // threshold stay quiet, the third trips the seam exactly once.
  failReconnectBeforeOpen();
  assert.equal(authFailures, 0, "one close-before-open is below the threshold");
  failReconnectBeforeOpen();
  assert.equal(authFailures, 0, "two close-before-opens are below the threshold");
  failReconnectBeforeOpen();
  assert.equal(authFailures, 1, "three close-before-opens past the threshold escalate ONCE");
  assert.equal(
    MockWebSocket.instances.length,
    4,
    "exactly 4 sockets attempted (the seed + 3 reconnects)",
  );

  stream.stop();
});

test("onAuthFailure fires at most once per close-before-open streak, not on every failed reconnect", () => {
  let authFailures = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {
      authFailures += 1;
    },
  });
  stream.start();
  MockWebSocket.instances[0]!.fireOpen();
  MockWebSocket.instances[0]!.fireClose();
  // Reach the threshold so escalation fires once.
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 1, "escalated exactly once at the threshold");

  // Keep failing: further close-before-opens must NOT re-fire onAuthFailure —
  // the streak has already escalated, and one probe is the caller's contract
  // (a 401 on it calls disconnect -> stop(); a transport failure leaves the
  // reconnect loop alone). Continuous re-escalation would spam requestResync.
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 1, "subsequent close-before-opens must not re-escalate");

  stream.stop();
});

test("a successful open resets the close-before-open streak so a blip-then-recover does not escalate", () => {
  let authFailures = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {
      authFailures += 1;
    },
  });
  stream.start();
  MockWebSocket.instances[0]!.fireOpen();
  MockWebSocket.instances[0]!.fireClose();
  // Two close-before-opens (below the threshold).
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 0, "not yet at the threshold");

  // The next reconnect opens successfully — the streak resets to 0 and the
  // escalation guard clears. Another transport drop is a normal close.
  flushTimers();
  MockWebSocket.instances[3]!.fireOpen();
  MockWebSocket.instances[3]!.fireClose();

  // Two MORE close-before-opens: still below threshold, no escalation. WITHOUT
  // the reset on open, the count would already be 4 (past the threshold) and
  // would have escalated — this test pins that the reset is load-bearing.
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 0, "an open resets the streak, so a blip-then-recover must not escalate");

  stream.stop();
});

test("a successful open clears the escalation guard so a later genuine revocation re-escalates", () => {
  // Companion to the previous test: recovery must not suppress a future
  // genuine revocation. Escalate once, recover, then a fresh streak of
  // close-before-opens must fire onAuthFailure again (the guard was reset).
  let authFailures = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {
      authFailures += 1;
    },
  });
  stream.start();
  MockWebSocket.instances[0]!.fireOpen();
  MockWebSocket.instances[0]!.fireClose();
  // First genuine revocation: reaches the threshold and escalates once.
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 1, "first genuine revocation escalated");

  // Recover: the next reconnect opens, reset, and another transport drop is normal.
  flushTimers();
  MockWebSocket.instances[4]!.fireOpen();
  MockWebSocket.instances[4]!.fireClose();

  // A second genuine revocation: three close-before-opens would NOT escalate
  // if the previous streak's `escalatedAuthFailure` had stayed true — this is
  // the regression that would re-introduce the forever-loop bug, just delayed
  // to the second rotation.
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 2, "a later genuine revocation re-escalates after recovery");

  stream.stop();
});

test("stop() interrupts a close-before-open streak and suppresses any further escalation", () => {
  let authFailures = 0;
  const stream = new EventStream("tok", {
    onEvent: () => {},
    onResync: () => {},
    onStatus: () => {},
    onAuthFailure: () => {
      authFailures += 1;
    },
  });
  stream.start();
  MockWebSocket.instances[0]!.fireOpen();
  MockWebSocket.instances[0]!.fireClose();
  failReconnectBeforeOpen();
  failReconnectBeforeOpen();
  assert.equal(authFailures, 0, "two close-before-opens are below the threshold");

  // The caller's probe returned 401 -> disconnect() -> stopStream() ->
  // stream.stop(). stop() must clear the armed reconnect timer and drop the
  // handlers so a streak that was below the threshold cannot continue and tip
  // past it. flushTimers() should find NO armed timer to run.
  stream.stop();
  flushTimers();
  assert.equal(MockWebSocket.instances.length, 3, "no reconnect fires after stop()");
  assert.equal(authFailures, 0, "the abandoned streak should not escalate once stopped");
});
