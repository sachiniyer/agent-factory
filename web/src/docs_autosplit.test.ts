import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

import { isProposalView, placeNewDocsTabs, rebindTab, showTab } from "./docs_autosplit.js";
import { closeLeaf, type LayoutNode, leaves, resetIds, singleLeaf, splitLeaf } from "./layout.js";
import type { SplitView as SplitViewType } from "./split.js";
import { TabKind } from "./types.js";

register("./browser_stub_loader.mjs", import.meta.url);
const { SplitView } = (await import("./split.js")) as { SplitView: typeof SplitViewType };

const A = TabKind.Agent;
const S = TabKind.Shell;
const D = TabKind.Docs;
const key = (id: string) => id;

function tabs(node: LayoutNode): number[] {
  return leaves(node).map((l) => l.tab);
}

function rowRatio(node: LayoutNode): [string, number] | null {
  return node.kind === "split" ? [node.dir, node.ratio] : null;
}

test("a Docs tab that appears while viewing splits right of Agent, 50/50", () => {
  resetIds();
  const seen = new Set<string>();
  let tree: LayoutNode = singleLeaf(0);
  tree = placeNewDocsTabs(tree, ["agent", "shell"], [A, S], seen, key, false);
  assert.deepEqual(tabs(tree), [0], "no Docs tab: layout untouched");
  tree = placeNewDocsTabs(tree, ["agent", "shell", "docs"], [A, S, D], seen, key, false);
  assert.deepEqual(tabs(tree), [0, 2], "Agent left, Docs right");
  assert.deepEqual(rowRatio(tree), ["row", 0.5]);
});

test("a session opened with an existing Docs tab shows Agent and Docs", () => {
  resetIds();
  const tree = placeNewDocsTabs(singleLeaf(0), ["agent", "docs"], [A, D], new Set(), key, false);
  assert.deepEqual(tabs(tree), [0, 1]);
  assert.deepEqual(rowRatio(tree), ["row", 0.5]);
});

test("a split the user closes stays closed", () => {
  resetIds();
  const seen = new Set<string>();
  const ids = ["agent", "docs"];
  const split = placeNewDocsTabs(singleLeaf(0), ids, [A, D], seen, key, false);
  const docsLeaf = leaves(split)[1]!;
  const closed = closeLeaf(split, docsLeaf.id)!;
  const again = placeNewDocsTabs(closed, ids, [A, D], seen, key, false);
  assert.equal(again, closed, "the same tree comes back");
  assert.deepEqual(tabs(again), [0]);
});

test("a rearranged split is not put back", () => {
  resetIds();
  const seen = new Set<string>();
  const ids = ["agent", "shell", "docs"];
  const kinds = [A, S, D];
  const split = placeNewDocsTabs(singleLeaf(0), ids, kinds, seen, key, false);
  const moved = splitLeaf(split, leaves(split)[0]!.id, "bottom", 2);
  assert.equal(placeNewDocsTabs(moved, ids, kinds, seen, key, false), moved);
});

test("at phone width the Docs tab stays a tab, and is not split later", () => {
  resetIds();
  const seen = new Set<string>();
  const leaf = singleLeaf(0);
  const narrow = placeNewDocsTabs(leaf, ["agent", "docs"], [A, D], seen, key, true);
  assert.equal(narrow, leaf);
  assert.equal(placeNewDocsTabs(leaf, ["agent", "docs"], [A, D], seen, key, false), leaf);
});

test("a second Docs tab replaces the Docs side instead of adding a third pane", () => {
  resetIds();
  const seen = new Set<string>();
  let tree = placeNewDocsTabs(singleLeaf(0), ["agent", "docs1"], [A, D], seen, key, false);
  const docsLeafId = leaves(tree)[1]!.id;
  tree = placeNewDocsTabs(tree, ["agent", "docs1", "docs2"], [A, D, D], seen, key, false);
  assert.deepEqual(tabs(tree), [0, 2]);
  assert.equal(leaves(tree)[1]!.id, docsLeafId, "the same pane now shows the new proposal");
});

test("a second Docs tab after the first split was closed splits right of Agent again", () => {
  resetIds();
  const seen = new Set<string>();
  const first = placeNewDocsTabs(singleLeaf(0), ["agent", "docs1"], [A, D], seen, key, false);
  const closed = closeLeaf(first, leaves(first)[1]!.id)!;
  const tree = placeNewDocsTabs(closed, ["agent", "docs1", "docs2"], [A, D, D], seen, key, false);
  assert.deepEqual(tabs(tree), [0, 2]);
});

test("with no Agent pane, a single pane keeps its tab and gains Docs on the right", () => {
  resetIds();
  const tree = placeNewDocsTabs(singleLeaf(1), ["agent", "shell", "docs"], [A, S, D], new Set(), key, false);
  assert.deepEqual(tabs(tree), [1, 2]);
});

test("a multi-pane layout with no Agent pane is left alone", () => {
  resetIds();
  const base = splitLeaf(singleLeaf(1), "leaf1", "right", 2);
  const ids = ["agent", "s1", "s2", "docs"];
  assert.equal(placeNewDocsTabs(base, ids, [A, S, S, D], new Set(), key, false), base);
});

type Internals = {
  placeDocs: (sessionId: string, tree: LayoutNode, tabIds: string[], tabKinds: number[]) => LayoutNode;
};

function view(narrow: boolean): Internals {
  const noop = { onStatus: () => {}, onFocusChange: () => {}, onLayout: () => {} };
  return new SplitView(null as unknown as HTMLElement, noop, () => narrow) as unknown as Internals;
}

test("SplitView records Docs tabs per session", () => {
  resetIds();
  const v = view(false);
  const ids = ["agent", "docs"];
  assert.deepEqual(tabs(v.placeDocs("s1", singleLeaf(0), ids, [A, D])), [0, 1]);
  assert.deepEqual(tabs(v.placeDocs("s1", singleLeaf(0), ids, [A, D])), [0], "once per tab");
  assert.deepEqual(tabs(v.placeDocs("s2", singleLeaf(0), ids, [A, D])), [0, 1], "other session");
});

test("SplitView reads phone width from its narrow probe", () => {
  resetIds();
  assert.deepEqual(tabs(view(true).placeDocs("s1", singleLeaf(0), ["agent", "docs"], [A, D])), [0]);
});

test("a Proposal click from any layout gives exactly Agent | Proposal", () => {
  resetIds();
  const kinds = [A, S, S, D];
  const handMade = splitLeaf(singleLeaf(1), "leaf1", "bottom", 2);
  const { tree, focus } = showTab(handMade, "leaf1", 3, kinds, false, 0.5);
  assert.deepEqual(tabs(tree), [0, 3]);
  assert.deepEqual(rowRatio(tree), ["row", 0.5]);
  assert.ok(isProposalView(tree, kinds));
  assert.equal(focus, leaves(tree)[1]!.id, "the Proposal pane gets focus");
});

test("the proposal view reuses the Agent pane and keeps the ratio it is given", () => {
  resetIds();
  const agent = singleLeaf(0);
  const { tree } = showTab(agent, agent.id, 1, [A, D], false, 0.3);
  assert.equal(leaves(tree)[0]!.id, agent.id, "the Agent terminal is not rebuilt");
  assert.deepEqual(rowRatio(tree), ["row", 0.3]);
});

test("a Proposal click in the proposal view changes nothing", () => {
  resetIds();
  const kinds = [A, D];
  const view = showTab(singleLeaf(0), "leaf1", 1, kinds, false, 0.5).tree;
  const again = showTab(view, leaves(view)[0]!.id, 1, kinds, false, 0.5);
  assert.equal(again.tree, view);
  assert.equal(again.focus, leaves(view)[1]!.id);
});

test("any other tab click leaves the proposal view for that tab alone", () => {
  resetIds();
  const kinds = [A, S, D];
  const view = showTab(singleLeaf(0), "leaf1", 2, kinds, false, 0.5).tree;
  const [agentPane, docsPane] = leaves(view);
  const toAgent = showTab(view, docsPane!.id, 0, kinds, false, 0.5);
  assert.deepEqual(tabs(toAgent.tree), [0]);
  assert.equal(toAgent.tree.id, agentPane!.id, "Agent keeps its pane");
  const toShell = showTab(view, docsPane!.id, 1, kinds, false, 0.5);
  assert.deepEqual(tabs(toShell.tree), [1]);
  assert.equal(toShell.focus, toShell.tree.id);
});

test("outside the proposal view a tab click rebinds the focused pane", () => {
  resetIds();
  const kinds = [A, S, S, D];
  const handMade = splitLeaf(singleLeaf(0), "leaf1", "right", 1);
  const { tree, focus } = showTab(handMade, "leaf1", 2, kinds, false, 0.5);
  assert.deepEqual(tabs(tree), [2, 1]);
  assert.equal(focus, "leaf1");
});

test("at phone width a Proposal click shows the proposal in the focused pane", () => {
  resetIds();
  const agent = singleLeaf(0);
  const { tree } = showTab(agent, agent.id, 1, [A, D], true, 0.5);
  assert.deepEqual(tabs(tree), [1]);
});

const W = TabKind.Web;
const V = TabKind.VSCode;

test("an arriving Proposal tab does not split around a visible Web or VS Code pane", () => {
  // Moving a pane's container to a new wrapper reloads its iframe, losing what the user typed in it:
  // the tab stays in the tab bar instead, and is not split in later.
  for (const kind of [W, V]) {
    resetIds();
    const seen = new Set<string>();
    const shown = singleLeaf(1);
    assert.equal(placeNewDocsTabs(shown, ["agent", "page", "docs"], [A, kind, D], seen, key, false), shown, "the only pane is an iframe");
    const agent = singleLeaf(0);
    const withAgent = splitLeaf(agent, agent.id, "right", 1);
    assert.equal(placeNewDocsTabs(withAgent, ["agent", "page", "docs"], [A, kind, D], new Set(), key, false), withAgent, "an iframe beside the Agent");
    assert.ok(seen.has("docs"), "recorded, so leaving the iframe later does not pop the split in");
    const back = singleLeaf(0);
    assert.equal(placeNewDocsTabs(back, ["agent", "page", "docs"], [A, kind, D], seen, key, false), back, "not split in once the iframe is gone");
  }
});

test("a Proposal tab still splits when only terminals are visible", () => {
  resetIds();
  const agent = singleLeaf(0);
  const terminals = splitLeaf(agent, agent.id, "right", 1);
  assert.ok(tabs(placeNewDocsTabs(terminals, ["agent", "shell", "docs"], [A, S, D], new Set(), key, false)).includes(2), "the Proposal pane is added beside the Agent");
  assert.deepEqual(tabs(placeNewDocsTabs(singleLeaf(0), ["agent", "docs"], [A, D], new Set(), key, false)), [0, 1]);
});

test("rebinding to the tab the focused pane already shows changes nothing", () => {
  // The bookkeeping rebind after a hidden tab is deleted: the target is the tab on screen already.
  resetIds();
  const kinds = [A, S, D];
  const view = showTab(singleLeaf(0), "leaf1", 2, kinds, false, 0.3).tree;
  const agentPane = leaves(view).find((l) => l.tab === 0)!;
  const keepAgent = rebindTab(view, agentPane.id, 0, kinds, false, 0.5);
  assert.equal(keepAgent.tree, view, "the Agent | Proposal split is not collapsed");
  assert.equal(keepAgent.focus, agentPane.id);
  const first = singleLeaf(0);
  const handMade = splitLeaf(splitLeaf(first, first.id, "right", 2), first.id, "bottom", 1);
  const proposalPane = leaves(handMade).find((l) => l.tab === 2)!;
  assert.equal(rebindTab(handMade, proposalPane.id, 2, kinds, false, 0.5).tree, handMade, "a custom layout is not replaced by the proposal view");
  // A different tab is a click's rule, as before.
  assert.deepEqual(tabs(rebindTab(view, agentPane.id, 1, kinds, false, 0.5).tree), [1]);
});
