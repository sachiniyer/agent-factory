import { test } from "node:test";
import assert from "node:assert/strict";
import { AppShell } from "./ui.js";

test("a no-op responsive pass discards its event-scoped picker state", () => {
  const captured = { trigger: {}, open: true };
  const shell = {
    phone: { matches: true },
    terminalSelected: false,
    el: { classList: { contains: () => false } },
    responsiveNewTabState: captured,
  };
  const syncPhone = (AppShell.prototype as unknown as {
    syncPhone(this: typeof shell): void;
  }).syncPhone;

  syncPhone.call(shell);

  assert.equal(shell.responsiveNewTabState, null);
});

test("a no-op responsive pass restores a picker closed by the owner media listener", () => {
  const calls: string[] = [];
  const cancel = () => calls.push("cancel");
  let expanded = false;
  const trigger = { getAttribute: () => expanded ? "true" : "false" };
  const returns = new WeakMap<object, () => void>();
  const shell = {
    phone: { matches: true },
    terminalSelected: true,
    el: { classList: { contains: () => true } },
    terminalChrome: { newTabSlot: { querySelector: () => trigger } },
    responsiveNewTabState: { trigger, cancel, open: true },
    newTabCancelReturn: returns,
    openNewTabPicker: () => { expanded = true; calls.push("reopen"); },
  };
  const syncPhone = (AppShell.prototype as unknown as {
    syncPhone(this: typeof shell): void;
  }).syncPhone;

  syncPhone.call(shell);

  assert.equal(shell.responsiveNewTabState, null);
  assert.deepEqual(calls, ["reopen"]);
  assert.equal(returns.get(trigger), cancel);
});

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

for (const action of ["openTab", "switchTab", "closeTab"] as const) {
  test(`${action} dismisses carried actions before its tab transition`, () => {
    const calls: string[] = [];
    const shell = {
      el: { classList: { contains: () => true } },
      appControls: { dismiss: () => calls.push("dismiss") },
      actions: { [action]: (index: number) => calls.push(`${action}:${index}`) },
      dismissCarriedActions: (AppShell.prototype as unknown as {
        dismissCarriedActions(this: AppShell): void;
      }).dismissCarriedActions,
    } as unknown as AppShell;
    AppShell.prototype[action].call(shell, 2);
    assert.deepEqual(calls, ["dismiss", `${action}:2`]);
  });
}

for (const userOpened of [false, true]) {
  for (const cancelBeforeRecomposition of [false, true]) {
    test(`responsive shortcut cancellation preserves userOpened=${userOpened}, cancelBeforeRecomposition=${cancelBeforeRecomposition}`, () => {
      let composed = false;
      let sessionExpanded = userOpened;
      let appExpanded = false;
      let pickerExpanded = false;
      let returned = false;
      const trigger = {
        getAttribute: () => pickerExpanded ? "true" : "false",
        click: () => { pickerExpanded = true; },
      };
      const slot = { querySelector: (selector: string) => selector === ".af-tab-new" ? trigger : { focus() {} } };
      const returns = new WeakMap<object, () => void>();
      const shell = {
        phone: { matches: false }, terminalSelected: true,
        el: { classList: { contains: () => composed, toggle: (_: string, value: boolean) => { composed = value; } } },
        newTabCancelReturn: returns, responsiveNewTabState: null,
        terminalChrome: { newTabSlot: slot, menu: {
          trigger: { getAttribute: () => sessionExpanded ? "true" : "false" },
          open: () => { sessionExpanded = true; }, close: () => { sessionExpanded = false; },
        } },
        appControls: {
          panel: { contains: () => composed, get hidden() { return !appExpanded; } },
          trigger: { getAttribute: () => appExpanded ? "true" : "false" },
          open: () => { appExpanded = true; }, close: () => { appExpanded = false; },
        },
        sessionFirst: { setActive() {} }, closeProjectMenu() {}, actions: { layoutChanged() {} },
        openNewTabPicker: AppShell.prototype.openNewTabPicker,
      };
      const app = shell as unknown as AppShell;
      AppShell.prototype.openNewTabPicker.call(app, () => { returned = true; });
      const cancel = () => {
        const restore = returns.get(trigger);
        returns.delete(trigger);
        pickerExpanded = false;
        restore?.();
      };
      const previousDocument = Object.getOwnPropertyDescriptor(globalThis, "document");
      Object.defineProperty(globalThis, "document", { configurable: true, value: { activeElement: null } });
      try {
        shell.phone.matches = true;
        if (cancelBeforeRecomposition) cancel();
        (AppShell.prototype as unknown as { syncPhone(this: AppShell): void }).syncPhone.call(app);
        if (!cancelBeforeRecomposition) cancel();
        assert.equal(pickerExpanded, false);
        assert.equal(returned, true);
        assert.equal(sessionExpanded, userOpened);
        assert.equal(appExpanded, false, "phone controls opened incidentally must close");
      } finally {
        if (previousDocument) Object.defineProperty(globalThis, "document", previousDocument);
        else Reflect.deleteProperty(globalThis, "document");
      }
    });
  }
}
