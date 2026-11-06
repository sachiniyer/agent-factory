// Pins the #6635 reopen contract: closing a DIFFERENT tab must not remount an
// unaffected pane. Run 42244518786's trace showed a peer pane flipping to
// `connecting` — a fresh AttachTerminal — right after a sibling tab's roster
// close landed on the same session. A remount force-sends the pane's grid on
// open, which under last-resize-wins re-strands whichever client just activated
// (#2347 spec reads the emulator ~50ms after the refit).
//
// The rebuild gate lives in reconcile(): a pane is recreated only when
// `staleAddress` holds — its tab's identity string changed at the slot — or the
// tab MOVED and the pane's address embeds the ordinal (`?tab=`, i.e. the tab
// has no daemon id — paneAddressUsesOrdinal). A tab addressed by `?tab_id=`
// rides the roster shift untouched: remapByIdentity moves the leaf to the tab's
// new ordinal and reconcile follows it without tearing down the stream.
//
// The driver is setSession's same-session branch — the path an out-of-band
// roster event takes — staged against a hand-rolled DOM (precedent:
// split_divider_stale_capture.test.ts). `new AttachTerminal` cannot be
// constructed under `node --test` (xterm stubs are inert), so the rebuild is
// observed through the fake terminal's dispose counter: the rebuild branch
// disposes the live term before constructing its replacement.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { leaves, resetIds, singleLeaf, splitLeaf, type LayoutNode } from "./layout.js";
import type { SplitCallbacks, SplitView as SplitViewType } from "./split.js";

register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

// --- hand-rolled DOM stub (split_divider_stale_capture.test.ts pattern) ---------

function makeEl(tag = "div"): any {
  const listeners = new Map<string, Set<(e: any) => void>>();
  const classSet = new Set<string>();
  const children: any[] = [];
  const style: Record<string, string> = {};
  const el: any = {
    tagName: tag,
    nodeName: tag.toUpperCase(),
    className: "",
    style,
    hidden: false,
    textContent: "",
    dataset: {},
    parentElement: null as any,
    attrs: {} as Record<string, string>,
    children,
    classList: {
      add: (...cs: string[]) => void cs.forEach((c) => classSet.add(c)),
      remove: (...cs: string[]) => void cs.forEach((c) => classSet.delete(c)),
      toggle: (c: string, on?: boolean) => {
        if (on === undefined) on = !classSet.has(c);
        classSet[on ? "add" : "delete"](c);
      },
      contains: (c: string) => classSet.has(c),
    },
    setAttribute: (k: string, v: string) => { el.attrs[k] = String(v); },
    getAttribute: (k: string) => el.attrs[k] ?? null,
    removeAttribute: (k: string) => { delete el.attrs[k]; },
    addEventListener: (type: string, fn: (e: any) => void) => {
      let set = listeners.get(type);
      if (!set) { set = new Set(); listeners.set(type, set); }
      set.add(fn);
    },
    removeEventListener: (type: string, fn: (e: any) => void) => {
      listeners.get(type)?.delete(fn);
    },
    dispatch: (type: string, e: any) => {
      const set = listeners.get(type);
      if (set) for (const fn of [...set]) fn(e);
    },
    append: (...kids: any[]) => { children.push(...kids); },
    appendChild: (kid: any) => { children.push(kid); return kid; },
    replaceChildren: (...kids: any[]) => { children.length = 0; children.push(...kids); },
    prepend: (kid: any) => { children.unshift(kid); },
    remove: () => {},
    getBoundingClientRect: () => ({ width: 0, height: 0, left: 0, top: 0, right: 0, bottom: 0 }),
    contains: (_n: any) => false,
    focus: () => {},
    blur: () => {},
    querySelector: () => null,
    querySelectorAll: () => [],
  };
  return el;
}

function stubDom(t: import("node:test").TestContext): void {
  const doc: any = {
    body: makeEl("body"),
    createElement: (tag: string) => makeEl(tag),
    createElementNS: (_ns: string, tag: string) => makeEl(tag),
    activeElement: null,
    addEventListener: () => {},
    removeEventListener: () => {},
  };
  const win: any = {
    matchMedia: () => ({ matches: false }),
    setTimeout: (fn: () => void) => setTimeout(fn, 0),
    clearTimeout: (h: ReturnType<typeof setTimeout>) => clearTimeout(h),
    addEventListener: () => {},
    removeEventListener: () => {},
    innerHeight: 0,
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
}

// --- staging ------------------------------------------------------------------

type SplitViewInternals = {
  tree: LayoutNode | null;
  builtTree: LayoutNode | null;
  displayTree: LayoutNode | null;
  builtRootEl: HTMLElement | null;
  sessionId: string | null;
  token: string | null;
  tabIds: string[];
  tabKinds: number[];
  tabRealIds: string[];
  tabTargets: (string | undefined)[];
  tabNames: string[];
  tabWebProxied: (boolean | undefined)[];
  focusedId: string | null;
  termHoldsFocus: boolean;
  panes: Map<string, any>;
  narrowEl: HTMLElement | null;
  tabCount: number;
};

function noopCallbacks(): SplitCallbacks {
  return { onStatus: () => {}, onFocusChange: () => {}, onLayout: () => {} };
}

/** A terminal-pane stub whose term.dispose() is counted — the rebuild branch's
 *  first observable act. `host.replaceChildren` is counted too, since the branch
 *  clears it before constructing the replacement AttachTerminal. */
function makePane(tab: number, identity: string): any {
  let disposed = 0;
  let hostCleared = 0;
  const host = makeEl("div");
  const orig = host.replaceChildren;
  host.replaceChildren = (...kids: any[]) => {
    hostCleared++;
    return orig(...kids);
  };
  const pane: any = {
    container: makeEl("div"),
    host,
    label: makeEl("span"),
    glyph: makeEl("span"),
    overlay: makeEl("div"),
    term: { dispose: () => { disposed++; } },
    tab,
    identity,
    status: "open",
    webUrl: null,
    webDispose: null,
    webArchived: false,
    iframeProxied: undefined,
    disposed: () => disposed,
    hostCleared: () => hostCleared,
  };
  return pane;
}

type Staged = {
  view: SplitViewType;
  in_: SplitViewInternals;
  paneFor: (tabIdentity: string) => any;
};

/** Stages a SplitView as it would look after its first setSession bound one leaf
 *  per shown tab: panes keyed by leaf id, each with a live fake terminal and the
 *  tab's current identity recorded. Returns helpers to read a pane by identity.
 *
 *  `shown` lists the shown ordinals — one leaf each (the fixture is a strip of
 *  sibling panes). `tabIds`/`tabRealIds`/`tabKinds`/`tabNames` describe the
 *  FULL roster; ids may be real (`id-*`) or synthesized (`kind:name`). */
function stage(
  t: import("node:test").TestContext,
  shown: number[],
  tabIds: string[],
  tabRealIds: string[],
  tabKinds: number[],
  tabNames: string[],
): Staged {
  stubDom(t);
  resetIds();
  const host = makeEl("div");
  host.clientWidth = 0;
  host.clientHeight = 0;
  const view = new SplitView(host as unknown as HTMLElement, noopCallbacks());
  const in_ = view as unknown as SplitViewInternals;

  // Compose the retained tree from the shown ordinals: a row of sibling leaves
  // when several tabs are shown, else the single leaf the fixture favours.
  let tree: LayoutNode = singleLeaf(shown[0]);
  for (const ord of shown.slice(1)) {
    tree = splitLeaf(tree, leaves(tree)[leaves(tree).length - 1].id, "right", ord);
  }

  in_.sessionId = "s1";
  in_.token = "tok";
  in_.tree = tree;
  in_.builtTree = tree;
  in_.displayTree = null;
  in_.builtRootEl = null;
  in_.tabIds = tabIds;
  in_.tabRealIds = tabRealIds;
  in_.tabKinds = tabKinds;
  in_.tabNames = tabNames;
  in_.tabTargets = [];
  in_.tabWebProxied = [];
  in_.tabCount = tabIds.length;
  in_.focusedId = leaves(tree)[0].id;
  in_.termHoldsFocus = false;
  in_.narrowEl = null;
  for (const leaf of leaves(tree)) {
    in_.panes.set(leaf.id, makePane(leaf.tab, tabIds[leaf.tab] ?? ""));
  }
  const paneFor = (identity: string) => {
    for (const leaf of leaves(in_.tree!)) {
      const ord = leaf.tab;
      const p = in_.panes.get(leaf.id);
      if (p && (in_.tabIds[ord] === identity || p.identity === identity)) return p;
    }
    return undefined;
  };
  return { view, in_, paneFor };
}

/** Drives the same-session roster update an out-of-band close produces. */
function rosterUpdate(
  view: SplitViewType,
  tabIds: string[],
  tabRealIds: string[],
  tabKinds: number[],
  tabNames: string[],
): void {
  view.setSession("s1", "tok", tabIds, 0, [], tabKinds, tabRealIds, false, tabNames, []);
}

// --- the contract: a sibling close must not remount an id-addressed pane -------

test("closing a sibling tab does NOT remount a pane bound to an id-addressed tab", (t) => {
  // The #2347 fixture's shape: pane shows peer-fit at ordinal 2 while
  // scrollkeep at ordinal 1 closes. The pane must follow its tab to ordinal 1
  // with its live terminal untouched — a remount would force-send its grid on
  // open and re-strand whichever client just activated.
  const { view, in_, paneFor } = stage(
    t,
    [2],
    ["id-agent", "id-scrollkeep", "id-peerfit"],
    ["id-agent", "id-scrollkeep", "id-peerfit"],
    [0, 1, 1],
    ["agent", "scrollkeep", "peer-fit"],
  );
  const pane = paneFor("id-peerfit");
  assert.ok(pane, "staged pane for peer-fit");

  rosterUpdate(view, ["id-agent", "id-peerfit"], ["id-agent", "id-peerfit"], [0, 1], ["agent", "peer-fit"]);

  assert.equal(pane.disposed(), 0, "the unaffected pane's terminal must NOT be disposed on a sibling close");
  assert.equal(pane.status, "open", "no remount → status stays open (a rebuild flips it to connecting)");
  assert.equal(pane.tab, 1, "the pane follows its tab to the new ordinal");
  assert.equal(in_.tabIds[pane.tab], "id-peerfit", "and the ordinal now names the same tab");
});

test("a two-leaf pane survives a sibling close the same way", (t) => {
  const { view, paneFor } = stage(
    t,
    [1, 2],
    ["id-agent", "id-scrollkeep", "id-peerfit"],
    ["id-agent", "id-scrollkeep", "id-peerfit"],
    [0, 1, 1],
    ["agent", "scrollkeep", "peer-fit"],
  );
  const peer = paneFor("id-peerfit");
  const scroll = paneFor("id-scrollkeep");
  assert.ok(peer && scroll, "both panes staged");

  rosterUpdate(view, ["id-agent", "id-peerfit"], ["id-agent", "id-peerfit"], [0, 1], ["agent", "peer-fit"]);

  // The pane whose own tab closed is disposed by the wanted-set walk — that is
  // the contract's correct half.
  assert.equal(scroll.disposed(), 1, "the pane whose tab closed is torn down");
  // The survivor follows, untouched.
  assert.equal(peer.disposed(), 0, "the surviving pane is not remounted");
  assert.equal(peer.tab, 1, "and follows its tab to the new ordinal");
});

// --- the cases where a remount IS correct -------------------------------------

test("an id-less tab that moved MUST remount — its ?tab= address went stale", (t) => {
  // A legacy (no daemon id) pane streams ?tab=<ordinal>: after the shift the
  // ordinal names a different tab, so rebuild is the only correct outcome.
  const { view, in_, paneFor } = stage(
    t,
    [2],
    ["0:agent", "1:scrollkeep", "1:peer-fit"],
    ["", "", ""],
    [0, 1, 1],
    ["agent", "scrollkeep", "peer-fit"],
  );
  const pane = paneFor("1:peer-fit");
  assert.ok(pane, "staged id-less pane");

  // The rebuild branch reaches `new AttachTerminal`, which the inert xterm stub
  // cannot construct — the throw propagates after dispose() ran. Catch it and
  // read the observable half.
  try {
    rosterUpdate(view, ["0:agent", "1:peer-fit"], ["", ""], [0, 1], ["agent", "peer-fit"]);
  } catch {
    // AttachTerminal construction under the xterm stub — dispose already ran.
  }
  assert.equal(pane.disposed(), 1, "an id-less moved tab's terminal IS disposed — its ordinal address is stale");
  assert.equal(in_.tabIds[pane.tab], "1:peer-fit", "the pane rebinds at the moved ordinal");
});

test("a pane whose tab's identity flipped at the slot remounts (backfill)", (t) => {
  // The #1779 adoption ordering: the close's roster event also delivered the
  // survivor's first real id. identity `1:peer-fit` → `id-peerfit` is a real
  // change at the slot, so the stale `?tab=` pane rebuilds onto `?tab_id=`.
  const { view, paneFor } = stage(
    t,
    [2],
    ["0:agent", "1:scrollkeep", "1:peer-fit"],
    ["", "", ""],
    [0, 1, 1],
    ["agent", "scrollkeep", "peer-fit"],
  );
  const pane = paneFor("1:peer-fit");
  assert.ok(pane, "staged pane");

  try {
    rosterUpdate(
      view,
      ["0:agent", "id-peerfit"],
      ["", "id-peerfit"],
      [0, 1],
      ["agent", "peer-fit"],
    );
  } catch {
    // AttachTerminal construction under the xterm stub.
  }
  assert.equal(pane.disposed(), 1, "an identity flip at the slot is a genuine address change → rebuild");
});
