import { test } from "node:test";
import assert from "node:assert/strict";
import { AppShell } from "./ui.js";

// Exercise the real picker entry point without constructing terminals or browser
// chrome. Ownership is the DOM contains() question, independent of viewport width.
for (const [owned, hidden] of [[true, true], [true, false], [false, true]]) {
  test(`picker opens its enclosing disclosure only when owned=${owned}, hidden=${hidden}`, () => {
    const calls: string[] = [];
    const trigger = { getAttribute: () => "false", click: () => calls.push("picker") };
    const slot = { querySelector: (selector: string) => selector === ".af-tab-new"
      ? trigger : { focus: () => calls.push("focus") } };
    const returns = new WeakMap<object, () => void>();
    const shell = {
      newTabCancelReturn: returns,
      terminalChrome: { newTabSlot: slot, menu: { open: () => calls.push("session") } },
      appControls: {
        panel: { hidden, contains: (node: unknown) => { assert.equal(node, slot); return owned; } },
        trigger: { focus: () => calls.push("return-focus") },
        open: () => calls.push("app"),
        close: (restoreFocus: boolean) => { assert.equal(restoreFocus, true); calls.push("return"); },
      },
    } as unknown as AppShell;
    AppShell.prototype.openNewTabPicker.call(shell);
    assert.equal(returns.has(trigger), owned);
    assert.deepEqual(calls, owned ? ["app", "session", "picker", "focus"] : ["session", "picker", "focus"]);
    returns.get(trigger)?.();
    if (owned) assert.equal(calls.at(-1), hidden ? "return" : "return-focus");
  });
}

test("phone picker return follows its current desktop owner", () => {
  const calls: string[] = [];
  let owned = true;
  const trigger = { getAttribute: () => "false", click: () => {}, focus: () => calls.push("new-tab") };
  const slot = { querySelector: (selector: string) => selector === ".af-tab-new" ? trigger : { focus: () => {} } };
  const returns = new WeakMap<object, () => void>();
  const shell = {
    newTabCancelReturn: returns,
    terminalChrome: { newTabSlot: slot, menu: { open: () => calls.push("session") } },
    appControls: {
      panel: { hidden: true, contains: () => owned },
      trigger: { focus: () => calls.push("app-trigger") },
      open: () => {}, close: () => calls.push("app-close"),
    },
  } as unknown as AppShell;
  AppShell.prototype.openNewTabPicker.call(shell);
  calls.length = 0;
  owned = false;
  returns.get(trigger)!();
  assert.deepEqual(calls, ["session", "new-tab"]);
});
