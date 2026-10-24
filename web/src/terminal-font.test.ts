import assert from "node:assert/strict";
import test from "node:test";

import type { ConfigEntry } from "./types.js";
import {
  bindTerminalFontPersist,
  formatTerminalFontSize,
  resetTerminalFontSizesForTest,
  setTerminalFontSizes,
  stepTerminalFontSize,
  subscribeTerminalFontSizes,
  terminalFontSizeFor,
  terminalFontSizesFromConfig,
  TERMINAL_FONT_SIZE_DESKTOP_DEFAULT,
  TERMINAL_FONT_SIZE_MAX,
  TERMINAL_FONT_SIZE_MIN,
  TERMINAL_FONT_SIZE_PHONE_DEFAULT,
} from "./terminal-font.js";

// The module under test is a process-wide singleton — the same instance the
// live app reads — so each test puts it back on the compiled-in defaults AND
// drops any save its presses armed, rather than letting a timer outlive the
// assertions that armed it.
function reset(): void {
  resetTerminalFontSizesForTest();
}

function webEntry(value: string): ConfigEntry {
  return { key: "web", value } as ConfigEntry;
}

test("a config without the web table yields the compiled-in defaults", () => {
  reset();
  assert.deepEqual(terminalFontSizesFromConfig([]), { phone: 6.5, desktop: 13 });
  assert.deepEqual(
    terminalFontSizesFromConfig([{ key: "appearance", value: "dark" } as ConfigEntry]),
    { phone: 6.5, desktop: 13 });
});

test("the web table's compact JSON lands both device-class sizes", () => {
  const sizes = terminalFontSizesFromConfig([
    webEntry(`{"terminal_font_size_phone":9,"terminal_font_size_desktop":15}`),
  ]);
  assert.deepEqual(sizes, { phone: 9, desktop: 15 });
});

test("a malformed table or leaf falls back per class, never to a guess", () => {
  for (const value of ["not-json", `"string"`, `42`, `null`]) {
    assert.deepEqual(
      terminalFontSizesFromConfig([webEntry(value)]),
      { phone: 6.5, desktop: 13 },
      `value ${value}`);
  }
  assert.deepEqual(
    terminalFontSizesFromConfig([webEntry(`{"terminal_font_size_phone":9}`)]),
    { phone: 9, desktop: 13 },
    "a leaf the table omits keeps its own default");
  assert.deepEqual(
    terminalFontSizesFromConfig([webEntry(`{"terminal_font_size_phone":"big","terminal_font_size_desktop":99}`)]),
    { phone: 6.5, desktop: 13 },
    "non-number and out-of-range leaves each drop to that class's default");
});

test("setTerminalFontSizes moves reads and notifies only on change", () => {
  reset();
  let notified = 0;
  const unsubscribe = subscribeTerminalFontSizes(() => notified++);
  setTerminalFontSizes({ phone: 9, desktop: 15 });
  assert.equal(terminalFontSizeFor(true), 9);
  assert.equal(terminalFontSizeFor(false), 15);
  assert.equal(notified, 1);
  // The save-echo refetch is free: an identical value notifies nothing.
  setTerminalFontSizes({ phone: 9, desktop: 15 });
  assert.equal(notified, 1);
  unsubscribe();
  setTerminalFontSizes({ phone: 10, desktop: 16 });
  assert.equal(notified, 1, "an unsubscribed terminal must not refit");
  reset();
});

test("a phone step is half a pixel and a desktop step one", () => {
  reset();
  const steps: number[] = [];
  const unsubscribe = subscribeTerminalFontSizes(() => steps.push(terminalFontSizeFor(true)));
  stepTerminalFontSize(1, true);
  stepTerminalFontSize(-1, true);
  stepTerminalFontSize(-1, true);
  unsubscribe();
  assert.equal(terminalFontSizeFor(true), 6);
  assert.deepEqual(steps, [7, 6.5, 6]);
  stepTerminalFontSize(1, false);
  assert.equal(terminalFontSizeFor(false), 14);
  assert.equal(terminalFontSizeFor(true), 6, "a desktop step must not move the phone size");
  reset();
});

test("steps clamp at the daemon's own bounds and stop notifying at the edge", () => {
  setTerminalFontSizes({ phone: TERMINAL_FONT_SIZE_MIN + 0.25, desktop: TERMINAL_FONT_SIZE_MAX - 0.5 });
  let notified = 0;
  const unsubscribe = subscribeTerminalFontSizes(() => notified++);
  stepTerminalFontSize(-1, true);
  assert.equal(terminalFontSizeFor(true), TERMINAL_FONT_SIZE_MIN);
  stepTerminalFontSize(-1, true);
  assert.equal(terminalFontSizeFor(true), TERMINAL_FONT_SIZE_MIN);
  stepTerminalFontSize(1, false);
  assert.equal(terminalFontSizeFor(false), TERMINAL_FONT_SIZE_MAX);
  assert.equal(notified, 2, "clamped-out presses change nothing and must not refit");
  unsubscribe();
  reset();
});

test("a burst of presses debounces into one save carrying the settled value", async () => {
  reset();
  const writes: Array<[string, string]> = [];
  bindTerminalFontPersist(async (key, value) => {
    writes.push([key, value]);
  });
  stepTerminalFontSize(1, true);
  stepTerminalFontSize(1, true);
  stepTerminalFontSize(1, true);
  // The debounce is 400ms — give the last armed timer room to fire.
  await new Promise((resolve) => setTimeout(resolve, 480));
  assert.deepEqual(writes, [["web.terminal_font_size_phone", "8"]],
    "three presses write once, with the settled 8 — not each intermediate size");
  // The two classes save under their own keys and cannot clobber each other.
  stepTerminalFontSize(1, false);
  await new Promise((resolve) => setTimeout(resolve, 480));
  assert.deepEqual(writes, [
    ["web.terminal_font_size_phone", "8"],
    ["web.terminal_font_size_desktop", "14"],
  ]);
  reset();
});

test("a config read landing mid-burst must not stomp the armed class", () => {
  reset();
  stepTerminalFontSize(1, true); // arms a phone save at 7
  // A refetch that still carries the PRE-STEP value must not snap the phone
  // size back — its save is armed and will land the stepped value itself.
  setTerminalFontSizes({ phone: 6.5, desktop: 15 });
  assert.equal(terminalFontSizeFor(true), 7, "the stepped size stays applied while its write is pending");
  assert.equal(terminalFontSizeFor(false), 15, "an unarmed class still takes the inbound value");
  reset();
});

test("the stale-read guard covers a save queued or in flight, not only its debounce", async () => {
  reset();
  // Deferred saves: the test decides when each write's round trip completes,
  // modelling a link slow enough for the debounce to lapse first.
  const completions: Array<() => void> = [];
  const writes: string[] = [];
  bindTerminalFontPersist((key, value) => {
    writes.push(value);
    return new Promise<void>((resolve) => completions.push(resolve));
  });

  stepTerminalFontSize(1, false); // desktop 13 → 14
  await new Promise((resolve) => setTimeout(resolve, 480)); // debounce fires; save A in flight
  assert.deepEqual(writes, ["14"]);

  // The timer has already run, yet the save is still pending — a read carrying
  // the pre-step value arriving NOW must still be skipped. (The pre-fix guard
  // ended with the timer and would have dropped the display to 13.)
  setTerminalFontSizes({ phone: 9, desktop: 13 });
  assert.equal(terminalFontSizeFor(false), 14, "an in-flight save still guards its class");
  assert.equal(terminalFontSizeFor(true), 9, "an unguarded class takes the read normally");

  // A second burst settles at 15 while the first save is outstanding: its write
  // queues behind it, so the guard must hold through BOTH completions.
  stepTerminalFontSize(1, false);
  await new Promise((resolve) => setTimeout(resolve, 480));
  assert.deepEqual(writes, ["14", "15"]);
  completions[0](); // save A's round trip ends; save B is still pending
  await new Promise((resolve) => setImmediate(resolve));
  setTerminalFontSizes({ phone: 9, desktop: 13 });
  assert.equal(terminalFontSizeFor(false), 15, "a queued save keeps the guard up after an earlier save settles");

  completions[1](); // save B ends; the guard drains
  await new Promise((resolve) => setImmediate(resolve));
  setTerminalFontSizes({ phone: 9, desktop: 20 });
  assert.equal(terminalFontSizeFor(false), 20, "once every pending save completes, a read lands again");
  reset();
});

test("a failed or rejected save still drains its pending window", async () => {
  reset();
  let fail = false;
  bindTerminalFontPersist(() => (fail ? Promise.reject(new Error("boom")) : Promise.resolve()));
  stepTerminalFontSize(1, true);
  await new Promise((resolve) => setTimeout(resolve, 480));
  // The save resolved fine. Now arm another that rejects: the module must not
  // leak the pending count and wedge the class's guard forever.
  fail = true;
  stepTerminalFontSize(1, true);
  await new Promise((resolve) => setTimeout(resolve, 480));
  await new Promise((resolve) => setImmediate(resolve)); // let the rejection settle
  setTerminalFontSizes({ phone: 6.5, desktop: 13 });
  assert.equal(terminalFontSizeFor(true), 6.5, "a rejected save must not hold the guard past its settle");
  reset();
});

test("the wire spelling stays the short form a config set accepts", () => {
  assert.equal(formatTerminalFontSize(7.5), "7.5");
  assert.equal(formatTerminalFontSize(8), "8");
});
