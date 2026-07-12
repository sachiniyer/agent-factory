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
  });
  stream.start();
  const url = MockWebSocket.instances[0]!.url;
  assert.match(url, /^wss:\/\/af\.example\.com\/v1\/events\?access_token=secret%20tok$/);
  stream.stop();
});
