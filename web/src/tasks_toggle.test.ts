// The task switch's pending state: it shows the requested value and stays busy
// while UpdateTask is in flight, and settles back if the request failed.
import assert from "node:assert/strict";
import test from "node:test";

import { flipSwitch, paintSwitch, releaseSettled, type PendingFlip, type SwitchEl } from "./tasks.js";

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
  const click = () => flipSwitch(pending, task, toggle, (on, busy) => paintSwitch(el, on, busy));
  // A list arrives: releases applied flips and rebuilds the handler's task from it.
  const refetch = (nowEnabled: boolean) => { releaseSettled(pending); task = { id: "t1", enabled: nowEnabled }; paintSwitch(el, nowEnabled, pending.has("t1")); };
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
