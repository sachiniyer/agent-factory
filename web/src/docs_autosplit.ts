// Opens a proposal's Docs tab split to the right of the Agent pane (proposal-mode
// ui.md). Pure, so the rule is testable without the DOM SplitView needs.

import { type LayoutNode, type LeafNode, leaves, replaceTab, setRatio, singleLeaf, splitLeaf } from "./layout.js";
import { TabKind } from "./types.js";

const AGENT_TAB = 0;

/**
 * Places each Docs tab not yet in `seen` into `tree`, once: an existing Docs pane is
 * reused (a second proposal replaces the first), otherwise the Agent pane splits
 * right, 50/50. A Docs tab already on screen is only recorded.
 *
 * `seen` holds `key(identity)` for every Docs tab handled so far and is mutated, so a
 * split the user later closes or rearranges is never put back. `narrow` (phone width)
 * records the tab without placing it, which keeps the tab-bar behaviour there.
 *
 * With no Agent pane, a single pane splits right (the user keeps the tab they were
 * on); a multi-pane layout the user built is left alone. A visible Web or VS Code pane is
 * never split around: building the new wrapper moves its container, which reloads the
 * iframe and loses what the user had typed in it, so the tab stays in the tab bar.
 */
export function placeNewDocsTabs(
  tree: LayoutNode,
  tabIds: string[],
  tabKinds: number[],
  seen: Set<string>,
  key: (identity: string) => string,
  narrow: boolean,
): LayoutNode {
  let out = tree;
  tabKinds.forEach((kind, tab) => {
    const k = key(tabIds[tab] ?? "");
    if (kind !== TabKind.Docs || seen.has(k)) {
      return;
    }
    seen.add(k);
    if (narrow || leaves(out).some((l) => tabKinds[l.tab] === TabKind.Web || tabKinds[l.tab] === TabKind.VSCode)) {
      return;
    }
    out = placeDocs(out, tab, tabKinds);
  });
  return out;
}

function placeDocs(tree: LayoutNode, docs: number, tabKinds: number[]): LayoutNode {
  const panes = leaves(tree);
  if (panes.some((l) => l.tab === docs)) {
    return tree;
  }
  const docsPane = panes.find((l) => tabKinds[l.tab] === TabKind.Docs);
  if (docsPane) {
    return replaceTab(tree, docsPane.id, docs);
  }
  const target = panes.find((l) => l.tab === AGENT_TAB) ?? (tree.kind === "leaf" ? tree : null);
  return target ? splitLeaf(tree, target.id, "right", docs) : tree;
}

/** True when `tree` is the proposal view: one row split, Agent left, a Docs tab right. */
export function isProposalView(tree: LayoutNode, tabKinds: number[]): boolean {
  return (
    tree.kind === "split" &&
    tree.dir === "row" &&
    tree.a.kind === "leaf" &&
    tree.a.tab === AGENT_TAB &&
    tree.b.kind === "leaf" &&
    tabKinds[tree.b.tab] === TabKind.Docs
  );
}

/**
 * The layout after a tab click (proposal-tab ui.md). A Docs tab always gives the
 * proposal view, Agent | Docs at `ratio`, whatever was on screen. Any other tab
 * leaves the proposal view for that tab alone, full width. Outside the proposal
 * view, and for a Docs tab at phone width, the focused pane shows the tab, as
 * before. `focus` is the leaf that shows `tab` afterwards.
 */
export function showTab(
  tree: LayoutNode,
  focusedId: string,
  tab: number,
  tabKinds: number[],
  narrow: boolean,
  ratio: number,
): { tree: LayoutNode; focus: string } {
  if (tabKinds[tab] === TabKind.Docs && !narrow && tab !== AGENT_TAB) {
    if (isProposalView(tree, tabKinds) && tree.kind === "split" && tree.b.kind === "leaf" && tree.b.tab === tab) {
      return { tree, focus: tree.b.id };
    }
    const shown = leaves(tree).find((l) => l.tab === AGENT_TAB);
    const agent: LeafNode = shown ? { kind: "leaf", id: shown.id, tab: AGENT_TAB } : singleLeaf(AGENT_TAB);
    const view = splitLeaf(agent, agent.id, "right", tab);
    const sized = view.kind === "split" ? setRatio(view, view.id, ratio) : view;
    return { tree: sized, focus: leaves(sized).find((l) => l.tab === tab)?.id ?? agent.id };
  }
  if (isProposalView(tree, tabKinds) && tree.kind === "split") {
    const keep = tab === AGENT_TAB ? tree.a.id : focusedId;
    return { tree: { kind: "leaf", id: keep, tab }, focus: keep };
  }
  const next = replaceTab(tree, focusedId, tab);
  return { tree: next, focus: leaves(next).find((l) => l.tab === tab)?.id ?? focusedId };
}

/**
 * `showTab` for the rebind that follows an awaited tab mutation (closing or creating a tab), which is
 * bookkeeping, not a click: when the focused pane already shows `tab` the layout is left exactly as
 * it is, so deleting an unrelated tab neither collapses Agent | Proposal nor replaces a layout the
 * user arranged. Any other target is a click's rule.
 */
export function rebindTab(
  tree: LayoutNode,
  focusedId: string,
  tab: number,
  tabKinds: number[],
  narrow: boolean,
  ratio: number,
): { tree: LayoutNode; focus: string } {
  if (leaves(tree).find((l) => l.id === focusedId)?.tab === tab) {
    return { tree, focus: focusedId };
  }
  return showTab(tree, focusedId, tab, tabKinds, narrow, ratio);
}
