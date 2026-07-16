// Pins SplitView.focus()'s boolean contract: whether a terminal actually took the
// keyboard. index.ts's focusTerminal() branches on it to decide between "terminal"
// mode and the rail fallback, so a silent regression here (back to a void/no-op
// focus) strands the user in terminal mode on any pane that has no xterm — a web
// tab (#1751) or a VS Code tab (#1817), both of which render an iframe with
// pane.term === null. nav.ts then drops every non-Escape key.
//
// These construct SplitView directly and stage its pane map rather than driving
// setSession(): the real path needs a DOM, an xterm and a live WS, none of which
// npm test has, and none of which this contract depends on. The Playwright selftest
// (web/selftest/web-driver.spec.ts) covers the end-to-end effect through the DOM.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

// split.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of which plain
// node can load (esbuild resolves them at bundle time). Stub them out, then import
// the module dynamically so the hook is registered first. node --test gives each
// test file its own process, so this affects nothing else in the suite.
register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

/** The private state focus() reads. Mirrors the shape of SplitView's fields; the
 *  cast is confined here so the tests below read as ordinary calls. */
type SplitViewInternals = {
  panes: Map<string, { term: { focus: () => void } | null }>;
  focusedId: string | null;
};

function noopCallbacks(): SplitCallbacks {
  return {
    onStatus: () => {},
    onFocusChange: () => {},
    onLayout: () => {},
  };
}

/** A SplitView whose focused pane is `pane` ("focused" pointing at a pane that is
 *  absent from the map when `pane` is undefined). Returns the view plus a counter of
 *  real term.focus() calls. */
function stage(pane: { term: { focus: () => void } | null } | undefined): {
  view: SplitViewType;
  focused: () => number;
} {
  let focusCount = 0;
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const internals = view as unknown as SplitViewInternals;
  if (pane) {
    const staged = pane.term
      ? {
          term: {
            focus: () => {
              focusCount++;
            },
          },
        }
      : { term: null };
    internals.panes.set("leaf-1", staged);
  }
  internals.focusedId = "leaf-1";
  return { view, focused: () => focusCount };
}

test("focus() returns true and focuses the term when the focused pane has one", () => {
  const { view, focused } = stage({ term: { focus: () => {} } });
  assert.equal(view.focus(), true);
  assert.equal(focused(), 1, "the pane's terminal must actually receive focus()");
});

test("focus() returns false for a pane with no terminal (a web/VS Code iframe pane)", () => {
  // The regression: mountWebPane leaves pane.term null, so the old
  // `pane?.term?.focus()` silently no-opped while the caller had already
  // committed the app to terminal mode.
  const { view, focused } = stage({ term: null });
  assert.equal(view.focus(), false);
  assert.equal(focused(), 0);
});

test("focus() returns false when no pane is focused", () => {
  const { view } = stage(undefined);
  assert.equal(view.focus(), false);
});

test("focus() returns false when the tree is empty (focusedId null)", () => {
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  assert.equal(view.focus(), false);
});

// --- cyclePane (Alt+j/k) must honor the same boolean -------------------------
//
// nav.ts fires Alt+j/k in EITHER mode, resolved BEFORE the terminal branch, so a user
// attached to a terminal pane can cycle onto a VS Code/web pane while focus is still
// "terminal". If cyclePane calls focus() bare, the no-op leaves the PREVIOUS pane's
// xterm holding DOM focus: the header highlights the new pane while keystrokes still
// reach the agent in the old one. Routing every internal focus site through refocus()
// is what keeps "the decision lives in one place" true.

/** A pane fake with just the surface cyclePane touches: applyFocusClass toggles a
 *  class on `container`, focusPane reads `status`, and focus()/blur() use `term`. */
function fakePane(hasTerm: boolean): {
  pane: unknown;
  focused: () => number;
  blurred: () => number;
} {
  let focusCount = 0;
  let blurCount = 0;
  const pane = {
    container: { classList: { toggle: () => {} } },
    status: "open",
    term: hasTerm
      ? {
          focus: () => {
            focusCount++;
          },
          blur: () => {
            blurCount++;
          },
        }
      : null,
  };
  return { pane, focused: () => focusCount, blurred: () => blurCount };
}

/** A two-pane split: leaf 0 is a terminal, leaf 1 is `secondHasTerm`. Focus starts on
 *  leaf 0, as if the user were attached to it. */
async function stageTwoPanes(secondHasTerm: boolean) {
  const { leaves, resetIds, singleLeaf, splitLeaf } = await import("./layout.js");
  resetIds();
  const root = singleLeaf(0);
  const tree = splitLeaf(root, root.id, "right", 1);
  const ids = leaves(tree).map((l) => l.id);

  const first = fakePane(true);
  const second = fakePane(secondHasTerm);
  const view = new SplitView(null as unknown as HTMLElement, noopCallbacks());
  const internals = view as unknown as {
    panes: Map<string, unknown>;
    focusedId: string | null;
    tree: unknown;
  };
  internals.tree = tree;
  internals.panes.set(ids[0] as string, first.pane);
  internals.panes.set(ids[1] as string, second.pane);
  internals.focusedId = ids[0] as string;
  return { view, internals, ids, first, second };
}

test("cyclePane onto a pane with NO terminal blurs the one that had the keyboard", async () => {
  const { view, internals, ids, first, second } = await stageTwoPanes(false);

  view.cyclePane(1);

  assert.equal(internals.focusedId, ids[1], "focus must move to the second pane");
  assert.equal(second.focused(), 0, "an iframe pane has no term to focus");
  assert.equal(
    first.blurred(),
    1,
    "the previously focused terminal MUST be blurred — otherwise it keeps eating keys " +
      "while the header highlights the pane the user cycled to",
  );
});

test("cyclePane onto a pane WITH a terminal focuses it and does not blur everything", async () => {
  const { view, internals, ids, first, second } = await stageTwoPanes(true);

  view.cyclePane(1);

  assert.equal(internals.focusedId, ids[1]);
  assert.equal(second.focused(), 1, "the target terminal takes the keyboard");
  assert.equal(first.blurred(), 0, "the DOM handles the A→B handoff; no blanket blur");
});

test("cyclePane reports rail mode through onFocusChange when the target has no terminal", async () => {
  // blur() is how SplitView asks index.ts for rail mode: it owns no store, so the
  // onFocusChange(false) echo (via each term's blur event) IS the focusRail() path.
  // Pin that the blur actually reaches a real terminal, which is what raises the echo.
  const { view, first } = await stageTwoPanes(false);
  view.cyclePane(1);
  assert.equal(first.blurred(), 1);
});
