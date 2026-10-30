// The task switch's pending state: it shows the requested value and stays busy
// while UpdateTask is in flight, and settles back if the request failed.
import assert from "node:assert/strict";
import test from "node:test";

import { flipSwitch, paintSwitch, reconcileOnUpdate, releaseSettled, type PendingFlip, type SwitchEl } from "./tasks.js";

function fakeSwitch(): SwitchEl & { attrs: Map<string, string>; on: boolean } {
  const attrs = new Map<string, string>();
  const el = {
    title: "",
    attrs,
    on: false,
    classList: { toggle(_name: string, on: boolean) { el.on = on; } },
    setAttribute(name: string, value: string) { attrs.set(name, value); },
    removeAttribute(name: string) { attrs.delete(name); },
  };
  return el;
}

const flush = () => new Promise<void>((r) => setImmediate(r));

function harness(enabled: boolean) {
  const el = fakeSwitch();
  const pending = new Map<string, PendingFlip>();
  let settle: (r: boolean | Error) => void = () => {};
  let calls = 0;
  const toggle = () => {
    calls++;
    return new Promise<boolean>((res, rej) => { settle = (r) => (r instanceof Error ? rej(r) : res(r)); });
  };
  // The rendered task: the click handler toggles from THIS value, like the pane's.
  let task = { id: "t1", enabled };
  const click = () => flipSwitch(pending, task, toggle, (on, busy) => paintSwitch(el, on, busy), () => task.enabled);
  // A list arrives: releases applied flips and rebuilds the handler's task from it.
  const refetch = (nowEnabled: boolean) => { releaseSettled(pending, [{ id: "t1", enabled: nowEnabled }]); task = { id: "t1", enabled: nowEnabled }; paintSwitch(el, nowEnabled, pending.has("t1")); };
  return { el, pending, click, refetch, settle: (r: boolean | Error) => settle(r), calls: () => calls };
}

test("a click shows the requested value and stays busy until the request settles", async () => {
  const h = harness(true);
  h.click();
  assert.equal(h.el.attrs.get("aria-checked"), "false");
  assert.equal(h.el.attrs.get("aria-disabled"), "true");
  assert.equal(h.el.attrs.get("aria-busy"), "true");
  assert.equal(h.el.title, "Disabled · click to enable");
  h.click();
  assert.equal(h.calls(), 1, "a second click while in flight is ignored");
  h.settle(true);
  await flush();
  // Applied, but the list has not reconciled it yet: still the new value, still busy.
  assert.equal(h.el.attrs.get("aria-checked"), "false");
  assert.equal(h.el.attrs.get("aria-busy"), "true");
  h.refetch(false);
  assert.equal(h.el.attrs.get("aria-checked"), "false");
  assert.equal(h.el.attrs.has("aria-busy"), false);
  assert.equal(h.pending.size, 0);
});

test("UpdateTask finishing before the list refetch cannot let a quick second click toggle from the stale value", async () => {
  const h = harness(true);
  h.click();
  h.settle(true);
  await flush();
  // The old handler still holds enabled=true; a click now must not send "disable" again.
  h.click();
  assert.equal(h.calls(), 1, "no second request until the list has reconciled");
  h.refetch(false);
  // Reconciled: displayed value and the handler's base value agree, so the click toggles from false.
  h.click();
  assert.equal(h.calls(), 2);
  assert.equal(h.el.attrs.get("aria-checked"), "true", "the second click moves to enabled");
});

test("a refused or failed request restores the old value", async () => {
  for (const fail of [false, new Error("boom")]) {
    const h = harness(true);
    h.click();
    h.settle(fail);
    await flush();
    assert.equal(h.el.attrs.get("aria-checked"), "true");
    assert.equal(h.el.on, true);
    assert.equal(h.el.attrs.has("aria-disabled"), false);
    assert.equal(h.pending.size, 0);
  }
});

test("the busy switch keeps focusability: aria-disabled, never the disabled attribute", () => {
  const h = harness(false);
  h.click();
  assert.equal(h.el.attrs.has("disabled"), false);
  assert.equal(h.el.attrs.get("aria-disabled"), "true");
});

test("a rebuild mid-request repaints the pending value from the map", () => {
  const h = harness(false);
  h.click();
  const rebuilt = fakeSwitch();
  paintSwitch(rebuilt, h.pending.get("t1")!.value, true);
  assert.equal(rebuilt.attrs.get("aria-checked"), "true");
  assert.equal(rebuilt.attrs.get("aria-busy"), "true");
});

// A manual clock for the backstop: set/clear/advance, no real timers.
function fakeClock() {
  let now = 0;
  let next = 1;
  const timers = new Map<number, { at: number; fn: () => void }>();
  return {
    set: (fn: () => void, ms: number) => { const id = next++; timers.set(id, { at: now + ms, fn }); return id; },
    clear: (h: unknown) => { timers.delete(h as number); },
    advance(ms: number) {
      const to = now + ms;
      for (;;) {
        const due = [...timers.entries()].filter(([, t]) => t.at <= to).sort((a, b) => a[1].at - b[1].at)[0];
        if (!due) break;
        timers.delete(due[0]);
        now = due[1].at;
        due[1].fn();
      }
      now = to;
    },
    pendingTimers: () => timers.size,
  };
}

function backstopHarness() {
  const clock = fakeClock();
  const expired: string[] = [];
  const backstop = { ms: 5000, set: clock.set, clear: clock.clear, expire: (id: string) => { expired.push(id); } };
  const pending = new Map<string, PendingFlip>();
  const el = fakeSwitch();
  const resolvers: ((r: boolean) => void)[] = [];
  const toggle = () => new Promise<boolean>((res) => { resolvers.push(res); });
  let enabled = true;
  const click = () => flipSwitch(pending, { id: "t1", enabled }, toggle, (on, busy) => paintSwitch(el, on, busy), () => enabled, backstop);
  return { clock, expired, pending, el, resolvers, click, setEnabled: (v: boolean) => { enabled = v; } };
}

test("a backstop timer clears only the flip it was armed for", async () => {
  const h = backstopHarness();
  h.click();                         // flip 1 at t=0
  h.resolvers[0](true);
  await flush();                     // applied: backstop 1 armed for t=5000
  h.clock.advance(1000);
  releaseSettled(h.pending, [{ id: "t1", enabled: false }], { clear: h.clock.clear }); // the list reconciles flip 1
  h.setEnabled(false);
  h.clock.advance(1000);
  h.click();                         // flip 2 at t=2000
  h.clock.advance(2000);
  h.resolvers[1](true);
  await flush();                     // flip 2 applied at t=4000: backstop 2 armed for t=9000
  h.clock.advance(1000);             // t=5000: flip 1's old deadline passes
  assert.equal(h.pending.has("t1"), true, "flip 1's timer must not clear flip 2");
  assert.equal(h.el.attrs.get("aria-busy"), "true");
  assert.deepEqual(h.expired, []);
  h.clock.advance(4000);             // t=9000: flip 2's own backstop
  assert.equal(h.pending.has("t1"), false);
  assert.deepEqual(h.expired, ["t1"]);
});

test("a stale backstop that was never cancelled still cannot clear a newer flip", async () => {
  const h = backstopHarness();
  h.click();
  h.resolvers[0](true);
  await flush();
  h.clock.advance(1000);
  releaseSettled(h.pending, [{ id: "t1", enabled: false }]); // no clear: the old timer stays armed
  h.setEnabled(false);
  h.click();
  h.clock.advance(2000);
  h.resolvers[1](true);
  await flush();
  h.clock.advance(2000);             // t=5000: the stale timer fires
  assert.equal(h.pending.has("t1"), true, "the token check keeps the newer flip");
  assert.deepEqual(h.expired, []);
});

test("reconciling cancels the backstop", async () => {
  const h = backstopHarness();
  h.click();
  h.resolvers[0](true);
  await flush();
  assert.equal(h.clock.pendingTimers(), 1);
  releaseSettled(h.pending, [{ id: "t1", enabled: false }], { clear: h.clock.clear });
  assert.equal(h.clock.pendingTimers(), 0);
  h.clock.advance(10000);
  assert.deepEqual(h.expired, []);
});

test("a failed flip repaints the latest list value, not the one captured at click time", async () => {
  const h = harness(true);
  h.click();                // disable requested
  h.refetch(false);         // another client disabled it: the list now says false
  // (an unsettled flip is not released, so the switch still shows the pending value)
  h.settle(false);          // our request is refused
  await flush();
  assert.equal(h.el.attrs.get("aria-checked"), "false", "the latest list value, not the captured enabled=true");
  assert.equal(h.el.attrs.has("aria-busy"), false);
  h.click();
  assert.equal(h.calls(), 2, "the next click is accepted, toggling from the displayed value");
  assert.equal(h.el.attrs.get("aria-checked"), "true");
});

test("a list that does not carry the flipped value leaves an applied flip pending", async () => {
  const h = harness(true);
  h.click();
  h.settle(true);
  await flush();
  h.refetch(true);          // a list fetched before the update landed
  assert.equal(h.pending.has("t1"), true, "no authoritative list yet");
  assert.equal(h.el.attrs.get("aria-busy"), "true");
  h.click();
  assert.equal(h.calls(), 1);
  h.refetch(false);         // the list that reflects it
  assert.equal(h.pending.size, 0);
});

test("a task the new list no longer holds is released", async () => {
  const h = backstopHarness();
  h.click();
  h.resolvers[0](true);
  await flush();
  releaseSettled(h.pending, [], { clear: h.clock.clear });
  assert.equal(h.pending.size, 0);
  assert.equal(h.clock.pendingTimers(), 0);
});

test("a project switch re-renders the same list and keeps an applied flip pending", async () => {
  const h = backstopHarness();
  const list = [{ id: "t1", enabled: true }];
  h.click();
  h.resolvers[0](true);
  await flush();
  reconcileOnUpdate(h.pending, list, list, { clear: h.clock.clear });   // away and back: same array
  assert.equal(h.pending.has("t1"), true, "no authoritative list arrived");
  assert.equal(h.clock.pendingTimers(), 1);
  reconcileOnUpdate(h.pending, list, [{ id: "t1", enabled: false }], { clear: h.clock.clear });
  assert.equal(h.pending.size, 0, "a new list that carries the flipped value releases it");
});
