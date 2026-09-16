import { test } from "node:test";
import assert from "node:assert/strict";
import { restoreShortcutFocus } from "./shortcut-focus.js";

// A fake element wired to a shared `active` ref standing in for
// document.activeElement. `focusable: false` models an element focus() cannot
// reach — a visibility:hidden phone drawer — where the call is a no-op and any
// already-focused node keeps DOM focus (#4360).
function harness() {
  let active: unknown = null;
  const body = { tagName: "BODY" };
  const el = (opts: { connected?: boolean; focusable?: boolean } = {}) => {
    const self = {
      isConnected: opts.connected ?? true,
      tabIndex: 0,
      focused: false,
      blurred: false,
      focus() {
        if (opts.focusable === false) return;
        active = self;
        self.focused = true;
      },
      blur() {
        self.blurred = true;
        if (active === self) active = body;
      },
    };
    return self;
  };
  const previous = Object.getOwnPropertyDescriptor(globalThis, "document");
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: { get activeElement() { return active; }, body },
  });
  const restore = () => {
    if (previous) Object.defineProperty(globalThis, "document", previous);
    else Reflect.deleteProperty(globalThis, "document");
  };
  return {
    body, el, restore,
    get active() { return active; },
    set active(v: unknown) { active = v; },
  };
}

test("a rail that cannot take focus drops the stale element instead", () => {
  const h = harness();
  try {
    const stale = h.el();          // the just-hidden picker item
    const rail = h.el({ focusable: false }); // visibility:hidden phone drawer
    h.active = stale;
    restoreShortcutFocus(h.body as HTMLElement, rail as unknown as HTMLElement);
    assert.equal(h.active, h.body, "hidden rail leaves stale focus: next shortcut is swallowed");
    assert.equal(stale.blurred, true);
    assert.equal(rail.tabIndex, -1);
  } finally {
    h.restore();
  }
});

test("a focusable rail still becomes the navigation target", () => {
  const h = harness();
  try {
    const stale = h.el();
    const rail = h.el();
    h.active = stale;
    restoreShortcutFocus(h.body as HTMLElement, rail as unknown as HTMLElement);
    assert.equal(h.active, rail);
    assert.equal(stale.blurred, false);
    assert.equal(rail.tabIndex, -1);
  } finally {
    h.restore();
  }
});

test("the element focused at shortcut time wins when it can take focus", () => {
  const h = harness();
  try {
    const prior = h.el();
    const rail = h.el();
    const stale = h.el();
    h.active = stale;
    restoreShortcutFocus(prior as unknown as HTMLElement, rail as unknown as HTMLElement);
    assert.equal(h.active, prior);
    assert.equal(rail.focused, false);
    assert.equal(stale.blurred, false);
  } finally {
    h.restore();
  }
});

test("a disconnected prior target falls through to the rail", () => {
  const h = harness();
  try {
    const prior = h.el({ connected: false });
    const rail = h.el();
    const stale = h.el();
    h.active = stale;
    restoreShortcutFocus(prior as unknown as HTMLElement, rail as unknown as HTMLElement);
    assert.equal(prior.focused, false);
    assert.equal(h.active, rail);
  } finally {
    h.restore();
  }
});

test("no rail at all still blurs the stale element to body", () => {
  const h = harness();
  try {
    const stale = h.el();
    h.active = stale;
    restoreShortcutFocus(h.body as HTMLElement, null);
    assert.equal(h.active, h.body);
    assert.equal(stale.blurred, true);
  } finally {
    h.restore();
  }
});
