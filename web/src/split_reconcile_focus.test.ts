// Pins the focus-desync fix for out-of-band roster changes (#1815): when reconcile
// rebuilds the FOCUSED pane while the keyboard is attached to its terminal, the
// disposed AttachTerminal's blur is suppressed (terminal.ts sets `stopped` before
// xterm tears the textarea down), so no onFocusChange(false) ever corrects
// store.focus — it stays "terminal" while DOM focus falls back to document.body,
// and rail-mode keys (j/k, digits, t, w, Enter, Escape-as-interrupt) are silently
// swallowed by decideKey's `ctx.focus === "terminal"` → kind:"none" branch.
//
// The fix lives in SplitView.reEngageFocusAfterRebuild(), split out of reconcile()'s
// tail so its boolean contract is unit-testable without a DOM/xterm/WS (the real
// AttachTerminal cannot be constructed under `node --test` — see split_focus.test.ts).
// These stage the pane map directly, the same way split_focus.test.ts stages focus()
// and cyclePane(); the end-to-end behavior through a real xterm textarea is pinned by
// the Playwright selftest in web/selftest/web-driver.spec.ts.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load. Stub them out before importing the module (split_focus.test.ts
// precedent), then import dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state reEngageFocusAfterRebuild() / onPaneFocus() read and write. */
type SplitViewInternals = {
  panes: Map<string, unknown>;
  focusedId: string | null;
  termHoldsFocus: boolean;
  reEngageFocusAfterRebuild: (focusedRebuilt: boolean) => void;
  onPaneFocus: (leafId: string, focused: boolean) => void;
};

function recordCallbacks(): { cb: SplitCallbacks; calls: boolean[] } {
  const calls: boolean[] = [];
  return {
    calls,
    cb: {
      onStatus: () => {},
      onFocusChange: (f: boolean): void => {
        calls.push(f);
      },
      onLayout: () => {},
    },
  };
}

/** A pane with just the surface the helper + focus()/refocus() touch: a focus/blur
 *  counter on its terminal (null for a web/VS Code pane, which renders an iframe and
 *  carries no term). Mirrors fakePane() in split_focus.test.ts. */
function fakePane(hasTerm: boolean): { pane: unknown; focused: () => number; blurred: () => number } {
  let focusCount = 0;
  let blurCount = 0;
  const pane = {
    container: { classList: { toggle: (): void => {} } },
    status: "open",
    term: hasTerm
      ? {
          focus: (): void => {
            focusCount++;
          },
          blur: (): void => {
            blurCount++;
          },
        }
      : null,
  };
  return { pane, focused: () => focusCount, blurred: () => blurCount };
}

/** Stages a SplitView whose focused pane is `hasTerm`, with `termHoldsFocus` preset
 *  to model whether the operator was keyboard-attached at the moment of the rebuild. */
function stage(hasTerm: boolean, termHoldsFocus: boolean): {
  internals: SplitViewInternals;
  focused: () => number;
  blurred: () => number;
  calls: boolean[];
} {
  const { cb, calls } = recordCallbacks();
  const fake = fakePane(hasTerm);
  const view = new SplitView(null as unknown as HTMLElement, cb);
  const internals = view as unknown as SplitViewInternals;
  internals.panes.set("leaf-1", fake.pane);
  internals.focusedId = "leaf-1";
  internals.termHoldsFocus = termHoldsFocus;
  return { internals, focused: fake.focused, blurred: fake.blurred, calls };
}

// --- the rebuild happened while the keyboard was attached ---------------------

test("reEngageFocusAfterRebuild: a rebuilt focused TERMINAL is re-focused so the new textarea takes the keyboard", () => {
  // The bug: reconcile disposes the DOM-focused terminal (blur suppressed) and
  // constructs a new one that never auto-focuses, so store.focus stayed "terminal"
  // against a DOM that fell back to body. The fix hands the keyboard back to the
  // new terminal via refocus() → focus() → pane.term.focus().
  const { internals, focused, calls } = stage(true, true);
  internals.reEngageFocusAfterRebuild(true);
  assert.equal(focused(), 1, "the rebuilt terminal must receive focus() — the desync corrector");
  // The onFocusChange(true) echo that re-confirms store.focus="terminal" is emitted
  // by the REAL AttachTerminal's textarea focus listener, not by the helper — it is
  // pinned end-to-end by the Playwright selftest, not reachable from a staged term.
  assert.deepEqual(calls, [], "the helper itself echoes nothing on the terminal path");
});

test("reEngageFocusAfterRebuild: a rebuilt focused WEB pane corrects store.focus to rail", () => {
  // The symmetric case: the focused pane rebuilt from a terminal into a web/VS Code
  // pane (an out-of-band close that slid a web tab into the focused ordinal). There
  // is no terminal to hand the keyboard to, and the disposed terminal's blur was
  // suppressed, so onPaneFocus's debounced corrector is never armed — the helper
  // reports the focus loss itself so the nav model lands on "rail" (the right mode
  // when the focused pane ceases to be a terminal) instead of staying pinned on a
  // dead "terminal".
  const { internals, focused, calls } = stage(false, true);
  internals.reEngageFocusAfterRebuild(true);
  assert.equal(focused(), 0, "a web pane has no term to focus");
  assert.deepEqual(calls, [false], "the focus loss is reported so store.focus becomes \"rail\"");
  assert.equal(internals.termHoldsFocus, false, "the keyboard-attached mirror is cleared");
});

// --- the gate: never yank a detached operator back into terminal mode ---------

test("reEngageFocusAfterRebuild: a rebuild while in RAIL mode (detached) does not re-attach the operator", () => {
  // The user pressed Ctrl+] (focusRail → store.focus="rail", splitView.blur()) so
  // no terminal holds the keyboard; an out-of-band close then rebuilds the focused
  // pane. Re-focusing the new terminal would yank them back into "terminal" without
  // intent. termHoldsFocus=false (the gate the blur corrector clears) skips it.
  const { internals, focused, calls } = stage(true, false);
  internals.reEngageFocusAfterRebuild(true);
  assert.equal(focused(), 0, "a detached operator is not re-attached to the rebuilt terminal");
  assert.deepEqual(calls, [], "no focus change is reported — rail mode is left untouched");
  assert.equal(internals.termHoldsFocus, false, "the mirror stays clear");
});

test("reEngageFocusAfterRebuild: a rebuild while in RAIL mode onto a WEB pane reports nothing (already rail)", () => {
  const { internals, calls } = stage(false, false);
  internals.reEngageFocusAfterRebuild(true);
  assert.deepEqual(calls, [], "store.focus was already rail; nothing to correct");
});

// --- the gate: a passive repaint that did not rebuild the focused pane --------

test("reEngageFocusAfterRebuild: a reconcile that left the focused pane untouched re-focuses nothing", () => {
  // A passive repaint (a rename, an archive flip on a SIBLING web pane, a proxy
  // change) does not rebuild the focused pane, so focusedRebuilt is false. The
  // still-live terminal keeps DOM focus and store.focus stays "terminal" on its
  // own — calling refocus() here would fire the textarea's focus listener again
  // and, on a user who had deliberately detached, yank them back into terminal.
  const { internals, focused, calls } = stage(true, true);
  internals.reEngageFocusAfterRebuild(false);
  assert.equal(focused(), 0, "an untouched focused terminal is not re-focused");
  assert.deepEqual(calls, []);
});

test("reEngageFocusAfterRebuild: no focused pane is a no-op, not a crash", () => {
  const { cb } = recordCallbacks();
  const view = new SplitView(null as unknown as HTMLElement, cb);
  const internals = view as unknown as SplitViewInternals;
  internals.focusedId = null;
  internals.termHoldsFocus = true;
  assert.doesNotThrow(() => internals.reEngageFocusAfterRebuild(true));
});

// --- the mirror the gate reads: onPaneFocus maintains termHoldsFocus ----------

test("onPaneFocus(true) records that a terminal holds the keyboard and echoes store.focus=terminal", () => {
  // termHoldsFocus mirrors store.focus (both are driven by the same onFocusChange
  // echoes); onPaneFocus(true) — fired by a pane's textarea gaining focus — sets it
  // so a SUBSEQUENT focused-pane rebuild knows the operator was keyboard-attached.
  const { cb, calls } = recordCallbacks();
  const view = new SplitView(null as unknown as HTMLElement, cb);
  const internals = view as unknown as SplitViewInternals;
  internals.focusedId = "leaf-1";
  internals.onPaneFocus("leaf-1", true);
  assert.equal(internals.termHoldsFocus, true, "a textarea gaining focus marks the keyboard attached");
  assert.deepEqual(calls, [true], "the focus echo reaches index.ts → store.focus=\"terminal\"");
});
