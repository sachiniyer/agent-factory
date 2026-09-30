// Pins the reachability-cache contract of `previewOriginReachable` (#1856 step 3b):
// the browser-side probe that decides whether a web tab may move from the
// same-origin daemon mirror onto its own per-tab preview origin. The answer is
// memoized in the module-scope `previewReachable` map keyed by preview PORT.
//
// The bug these guard against: a SUCCESSFUL probe was cached for the whole SPA
// lifetime (only a FAILURE evicted), so a per-tab origin that became
// browser-unreachable LATER (same port, daemon's bind still up, but the
// browser-to-port network path broken — e.g. the preview-port ssh forward
// dropped while the main forward stayed up) kept returning the stale `true`,
// navigating the frame to a dead origin with no fallback. The fix threads the
// user-initiated ↻ / Retry `fresh` flag into `previewOriginReachable` so an
// explicit re-probe bypasses the cache the same way it bypasses `previewSrcOnce`.
//
// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which
// plain node can load (esbuild resolves them at bundle time). Stub them out
// before importing the module, then drive the framed probe through a hand-rolled
// DOM stub (no jsdom — matches the rest of web/src/*.test.ts). The rendered
// end-to-end behavior is additionally proven in the Playwright selftest.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

register("./browser_stub_loader.mjs", import.meta.url);
const { previewOriginReachable, PREVIEW_PROBE_MESSAGE } = (await import("./split.js")) as {
  previewOriginReachable: (origin: string, fresh?: boolean) => Promise<boolean>;
  PREVIEW_PROBE_MESSAGE: string;
};

/** A fake iframe the probe creates. Only the surface `previewOriginReachable`
 *  touches is implemented; `contentWindow` is a unique object so the message
 *  handler's `e.source === frame.contentWindow` identity check works. */
interface FakeFrame {
  attrs: Record<string, string>;
  style: Record<string, string>;
  contentWindow: { id: number };
  src: string;
  setAttribute(name: string, value: string): void;
  remove(): void;
}

interface ProbeControls {
  /** Number of `iframe`s created so far — a re-probe creates a NEW one, a cache
   *  hit creates none. The core observable for cache hit vs bypass. */
  iframeCount: () => number;
  /** The most recently created frame (the one the in-flight probe is waiting
   *  on). Throws if a probe has not started. */
  lastFrame: () => FakeFrame;
  /** Deliver the daemon's success postMessage for `frame`. Resolves the probe
   *  with `true` and removes its listener. */
  deliverSuccess: (frame: FakeFrame) => void;
  /** Fire the probe's timeout. Resolves the probe with `false` and evicts the
   *  cache entry (the "port is now browser-unreachable" path). */
  fireTimeout: () => void;
  /** Number of message listeners currently registered. */
  listenerCount: () => number;
}

/** Install hand-rolled `document`/`window` stubs that let the framed probe run
 *  deterministically under node. Restored on test completion. */
function stubDom(t: import("node:test").TestContext): ProbeControls {
  const frames: FakeFrame[] = [];
  const timers: Array<() => void> = [];
  const listeners: Array<(e: { source: unknown; data: unknown }) => void> = [];
  let seq = 0;

  const doc = {
    body: { appendChild: () => {} },
    createElement: (tag: string): FakeFrame => {
      if (tag !== "iframe") {
        throw new Error(`unexpected createElement(${tag})`);
      }
      const id = ++seq;
      const frame: FakeFrame = {
        attrs: {},
        style: {},
        contentWindow: { id },
        src: "",
        setAttribute(name: string, value: string): void {
          this.attrs[name] = value;
        },
        remove(): void {
          /* no-op: the probe removes the hidden frame from the DOM */
        },
      };
      frames.push(frame);
      return frame;
    },
  };

  const win = {
    setTimeout: (fn: () => void): number => {
      timers.push(fn);
      return timers.length;
    },
    clearTimeout: (_handle?: number): void => {
      /* no-op: the stub never auto-fires, so clearing is a no-op */
    },
    addEventListener: (
      type: string,
      fn: (e: { source: unknown; data: unknown }) => void,
    ): void => {
      if (type === "message") {
        listeners.push(fn);
      }
    },
    removeEventListener: (
      type: string,
      fn: (e: { source: unknown; data: unknown }) => void,
    ): void => {
      if (type === "message") {
        const i = listeners.indexOf(fn);
        if (i >= 0) {
          listeners.splice(i, 1);
        }
      }
    },
  };

  const prevDoc = Object.getOwnPropertyDescriptor(globalThis, "document");
  const prevWin = Object.getOwnPropertyDescriptor(globalThis, "window");
  Object.defineProperty(globalThis, "document", { configurable: true, value: doc });
  Object.defineProperty(globalThis, "window", { configurable: true, value: win });
  t.after(() => {
    if (prevDoc) Object.defineProperty(globalThis, "document", prevDoc);
    else Reflect.deleteProperty(globalThis, "document");
    if (prevWin) Object.defineProperty(globalThis, "window", prevWin);
    else Reflect.deleteProperty(globalThis, "window");
  });

  return {
    iframeCount: () => frames.length,
    lastFrame: () => {
      const f = frames[frames.length - 1];
      if (!f) {
        throw new Error("no probe has started yet");
      }
      return f;
    },
    deliverSuccess: (frame: FakeFrame) => {
      for (const fn of [...listeners]) {
        fn({ source: frame.contentWindow, data: PREVIEW_PROBE_MESSAGE });
      }
    },
    fireTimeout: () => {
      const fn = timers[timers.length - 1];
      if (!fn) {
        throw new Error("no probe timer is pending");
      }
      fn();
    },
    listenerCount: () => listeners.length,
  };
}

/** Unique preview ports per test so the module-scope `previewReachable` map
 *  (which persists across tests within this file's process) cannot let one
 *  test's cached answer leak into another. */
let portSeq = 9000;
function freshOrigin(): string {
  portSeq += 1;
  return `http://afprobe.localhost:${portSeq}/`;
}

// ---------------------------------------------------------------------------
// Pre-existing behavior that MUST stay intact (regression guards).
// ---------------------------------------------------------------------------

test("a successful probe is cached: a second non-fresh call reuses it without re-probing", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  const first = previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 1, "the first probe creates a hidden frame");
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await first, true);

  // A cache hit must NOT create a second iframe — one probe answers the page.
  const cached = previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 1, "a cached success is reused, never re-probed");
  assert.equal(await cached, true);
});

test("a failed probe is evicted: the next call re-probes rather than pinning a sticky false", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  const first = previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 1);
  dom.fireTimeout();
  assert.equal(await first, false);
  assert.equal(dom.listenerCount(), 0, "the failed probe tears its listener down");

  // The failure must have evicted itself so ↻ (and an ordinary reload) re-probes.
  const second = previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 2, "an evicted failure re-probes on the next call");
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await second, true);
});

// ---------------------------------------------------------------------------
// The fix: a user-initiated ↻ / Retry (`fresh`) bypasses the stale success.
// ---------------------------------------------------------------------------

test("THE FIX: fresh bypasses a cached success and re-probes the port", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  // Prime the cache with a successful probe.
  const first = previewOriginReachable(origin, false);
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await first, true);
  assert.equal(dom.iframeCount(), 1);

  // A non-fresh call hits the cache (no new frame) — the prior behavior.
  void previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 1, "non-fresh still reuses the cached success");

  // The user clicks ↻: `fresh` must bypass the cache and re-probe.
  const retry = previewOriginReachable(origin, true);
  assert.equal(dom.iframeCount(), 2, "fresh re-probes instead of returning the stale true");
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await retry, true);
});

test("THE FIX (bug scenario): after a cached success, a fresh re-probe that now times out falls back to false", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  // Step 1: the preview port was reachable, the per-tab probe succeeded, and the
  // success was cached for the SPA's lifetime.
  const first = previewOriginReachable(origin, false);
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await first, true);

  // Step 2: the browser's path to the port breaks (ssh forward dropped) while the
  // daemon stays bound and keeps vending the same origin. The user clicks ↻.
  // BEFORE the fix this returned the stale cached `true` and pointed the frame at
  // the dead origin; AFTER it re-probes, finds the port unreachable, and falls back.
  const retry = previewOriginReachable(origin, true);
  assert.equal(dom.iframeCount(), 2, "fresh re-probes the now-dead port");
  dom.fireTimeout();
  assert.equal(await retry, false, "fresh re-probe returns false → pane stays on the mirror");
  assert.equal(dom.listenerCount(), 0, "the failed re-probe tears its listener down");
});

test("after a fresh re-probe fails, a subsequent non-fresh call re-probes (the failure evicted itself)", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  const first = previewOriginReachable(origin, false);
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await first, true);

  const retry = previewOriginReachable(origin, true);
  dom.fireTimeout();
  assert.equal(await retry, false);

  // The fresh failure evicted the entry, so the cache is empty: the next ordinary
  // load re-probes rather than inheriting either the old true or the new false.
  const again = previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 3, "an evicted failure is re-probed on the next call");
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await again, true);
});

test("fresh on a never-cached port just probes (deleting a missing key is a no-op)", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  const retry = previewOriginReachable(origin, true);
  assert.equal(dom.iframeCount(), 1, "fresh on a cold cache probes exactly once");
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await retry, true);
});

test("a successful fresh re-probe re-caches, so a later non-fresh call still hits", async (t) => {
  const dom = stubDom(t);
  const origin = freshOrigin();

  const first = previewOriginReachable(origin, false);
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await first, true);
  assert.equal(dom.iframeCount(), 1);

  const retry = previewOriginReachable(origin, true);
  dom.deliverSuccess(dom.lastFrame());
  assert.equal(await retry, true);
  assert.equal(dom.iframeCount(), 2);

  // The fresh success re-cached, so the cache is NOT permanently broken by a
  // `fresh` bypass: a later ordinary load still reuses the fresh answer.
  void previewOriginReachable(origin, false);
  assert.equal(dom.iframeCount(), 2, "the fresh success is re-cached for later reuse");
});

test("fresh drops ONLY the named port: a cached answer for another port survives", async (t) => {
  const dom = stubDom(t);
  const a = freshOrigin();
  const b = freshOrigin();

  // Cache a success on port A.
  const pa = previewOriginReachable(a, false);
  dom.deliverSuccess(dom.lastFrame());
  await pa;

  // A fresh re-probe for a DIFFERENT port B must not evict A's cached answer.
  const pb = previewOriginReachable(b, true);
  assert.equal(dom.iframeCount(), 2, "fresh probes port B");
  dom.deliverSuccess(dom.lastFrame());
  await pb;

  // A's cache entry is still live: a non-fresh call on A does NOT re-probe.
  void previewOriginReachable(a, false);
  assert.equal(dom.iframeCount(), 2, "port A's cached success survived the fresh call on B");
});
