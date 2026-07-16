// The pure split-layout tree model for the web client's per-instance split panes
// (feat(web): drag-and-drop split tabs). A layout is a binary tree: each LEAF binds
// one of the instance's tabs to a pane; each SPLIT divides its area into two
// children — side by side ("row", a vertical divider) or stacked ("column", a
// horizontal divider) — at a resize ratio. The DEFAULT layout is a single leaf, so
// a user who never splits sees exactly today's one-tab-at-a-time behavior and
// nothing regresses.
//
// One tab lives in at most ONE pane: dragging a tab that is already shown elsewhere
// MOVES it (VS-Code-style), which also sidesteps two panes fighting over the same
// tab's PTY resize (the multi-writer stream is last-resize-wins, but showing one tab
// at two sizes would still look janky).
//
// Kept pure — no DOM, no I/O — so the tree transforms are unit-tested
// (layout.test.ts) independently of the imperative view (split.ts), exactly as
// nav.ts and sessions.ts separate their pure logic from index.ts's wiring.

/** The dataTransfer MIME the tab bar (ui.ts) stamps a dragged tab index into, and the
 *  panes (split.ts) read on drop. A private type keeps a page's other drags out. It
 *  lives here — the css-free pure module — so ui.ts can import it without dragging in
 *  the xterm/terminal (and its CSS) that split.ts pulls in. */
export const TAB_DND_MIME = "application/x-af-tab";

/** A split's orientation: "row" lays its two children left→right (a vertical
 *  divider between them); "column" lays them top→bottom (a horizontal divider). */
export type SplitDir = "row" | "column";

/** Where a dragged tab lands on a pane: an edge splits in that direction with the
 *  new pane on that side; "center" replaces the pane's bound tab. */
export type Edge = "left" | "right" | "top" | "bottom" | "center";

/** A pane: one of the instance's tabs rendered as a live terminal. */
export interface LeafNode {
  kind: "leaf";
  id: string;
  tab: number;
}

/** An internal split of one area into two children at `ratio` (the first child's
 *  fraction of the axis, in [0.1, 0.9]). */
export interface SplitNode {
  kind: "split";
  id: string;
  dir: SplitDir;
  ratio: number;
  a: LayoutNode;
  b: LayoutNode;
}

export type LayoutNode = LeafNode | SplitNode;

// A monotonic id source for freshly created nodes. A module counter (not
// Math.random) keeps split/close deterministic for the unit tests; resetIds() lets a
// test pin the sequence.
let idSeq = 0;

/** Test hook: reset the node-id counter so a test sees a deterministic sequence. */
export function resetIds(): void {
  idSeq = 0;
}

function nextId(prefix: string): string {
  idSeq += 1;
  return `${prefix}${idSeq}`;
}

/** The default single-pane layout bound to `tab` (today's behavior). */
export function singleLeaf(tab: number): LeafNode {
  return { kind: "leaf", id: nextId("leaf"), tab };
}

/** Every leaf in visual order (a-child before b-child, i.e. left/top first) — the
 *  order pane-focus cycling walks. */
export function leaves(node: LayoutNode): LeafNode[] {
  if (node.kind === "leaf") {
    return [node];
  }
  return [...leaves(node.a), ...leaves(node.b)];
}

/** The number of panes in the layout. */
export function leafCount(node: LayoutNode): number {
  return leaves(node).length;
}

/** The leaf with the given id, or null. */
export function findLeaf(node: LayoutNode, id: string): LeafNode | null {
  if (node.kind === "leaf") {
    return node.id === id ? node : null;
  }
  return findLeaf(node.a, id) ?? findLeaf(node.b, id);
}

/** Returns a new tree with the leaf `id` replaced by `fn(leaf)` (which may be a
 *  leaf or a split); structure elsewhere is shared. */
function mapLeaf(node: LayoutNode, id: string, fn: (leaf: LeafNode) => LayoutNode): LayoutNode {
  if (node.kind === "leaf") {
    return node.id === id ? fn(node) : node;
  }
  return { ...node, a: mapLeaf(node.a, id, fn), b: mapLeaf(node.b, id, fn) };
}

/** Maps every leaf through `fn`, PRESERVING the node reference when nothing changed
 *  (fn returns the same leaf and both children are unchanged). Reference stability is
 *  load-bearing: setSession re-validates on every store update, and a validate() that
 *  always produced a fresh tree would reconcile — and rebuild terminals — on every
 *  status tick, re-entering itself to a stack overflow. */
function mapAllLeaves(node: LayoutNode, fn: (leaf: LeafNode) => LeafNode): LayoutNode {
  if (node.kind === "leaf") {
    return fn(node);
  }
  const a = mapAllLeaves(node.a, fn);
  const b = mapAllLeaves(node.b, fn);
  if (a === node.a && b === node.b) {
    return node;
  }
  return { ...node, a, b };
}

/**
 * Removes the leaf `leafId`, collapsing its parent split so the sibling fills the
 * freed space. Returns the new tree, or null if `leafId` is the only pane (the last
 * pane can't be closed — a session always shows at least one terminal).
 */
export function closeLeaf(root: LayoutNode, leafId: string): LayoutNode | null {
  const remove = (node: LayoutNode): LayoutNode | null => {
    if (node.kind === "leaf") {
      return node.id === leafId ? null : node;
    }
    const a = remove(node.a);
    const b = remove(node.b);
    // A split has a single target at most, so at most one side collapses to null;
    // return the surviving sibling in that case (the collapse).
    if (a === null) {
      return b;
    }
    if (b === null) {
      return a;
    }
    if (a === node.a && b === node.b) {
      return node;
    }
    return { ...node, a, b };
  };
  // remove(root) is null only when root itself is the target leaf.
  return remove(root);
}

/** Drops every leaf bound to `tab` except the one with id `keepId`, collapsing each
 *  removal. Enforces the one-tab-one-pane invariant after a split/replace. */
function dedupeExcept(root: LayoutNode, tab: number, keepId: string): LayoutNode {
  const dupes = leaves(root).filter((l) => l.tab === tab && l.id !== keepId);
  let cur = root;
  for (const d of dupes) {
    cur = closeLeaf(cur, d.id) ?? cur;
  }
  return cur;
}

/**
 * Splits the leaf `leafId` along `edge`, placing a NEW pane bound to `tab` on that
 * edge's side (left/right → a row; top/bottom → a column). If `tab` is already shown
 * in another pane it is MOVED here (the one-tab-one-pane invariant). A "center" edge
 * is not a split — it replaces the pane's tab (see replaceTab).
 */
export function splitLeaf(root: LayoutNode, leafId: string, edge: Edge, tab: number): LayoutNode {
  if (edge === "center") {
    return replaceTab(root, leafId, tab);
  }
  const dir: SplitDir = edge === "left" || edge === "right" ? "row" : "column";
  const newFirst = edge === "left" || edge === "top";
  const fresh: LeafNode = { kind: "leaf", id: nextId("leaf"), tab };
  const grown = mapLeaf(root, leafId, (leaf) => {
    const [a, b] = newFirst ? [fresh, leaf] : [leaf, fresh];
    return { kind: "split", id: nextId("split"), dir, ratio: 0.5, a, b };
  });
  return dedupeExcept(grown, tab, fresh.id);
}

/**
 * The tab a NEW half should open when the pane's OWN tab is dragged onto its edge —
 * i.e. when the dragged tab is already what the target pane displays (#1901).
 *
 * Such a split cannot show the dragged tab twice: `splitLeaf` dedupes it right back
 * out (one tab, one pane), collapsing the split and making the drag read as a no-op,
 * which is the reported bug. So the new half opens a DIFFERENT tab and the dragged
 * one stays put — two distinct tabs side by side, never A|A.
 *
 * A candidate is any OTHER tab that is not already shown in another pane. Excluding
 * the ones on screen is what keeps this non-destructive: binding a tab that is live
 * elsewhere would MOVE it here (the same dedupe), closing a pane the user opened on
 * purpose to gain nothing. Preference order: `prefer` (the recently-focused tabs,
 * most-recent first), then the next tab in order, wrapping — which IS "the first
 * other tab" once the dragged tab is the last one.
 *
 * Returns null when there is no other tab to show — a single-tab session, or one
 * whose every other tab is already visible. The caller then leaves the layout
 * untouched: nothing can fill the new half, and duplicating the tab is the exact
 * outcome this exists to prevent.
 */
export function companionTab(
  root: LayoutNode,
  leafId: string,
  tab: number,
  tabCount: number,
  prefer: number[] = [],
): number | null {
  const shownElsewhere = new Set(leaves(root).filter((l) => l.id !== leafId).map((l) => l.tab));
  const usable = (c: number) => c !== tab && c >= 0 && c < tabCount && !shownElsewhere.has(c);
  for (const p of prefer) {
    if (usable(p)) {
      return p;
    }
  }
  for (let i = 1; i <= tabCount; i++) {
    const c = ((tab % tabCount) + i) % tabCount;
    if (usable(c)) {
      return c;
    }
  }
  return null;
}

/** Rebinds the pane `leafId` to show `tab` (a center-drop, a tab-bar click, or a 1-9
 *  key on the focused pane). Moves the tab here if it was shown elsewhere. */
export function replaceTab(root: LayoutNode, leafId: string, tab: number): LayoutNode {
  const target = findLeaf(root, leafId);
  if (!target || target.tab === tab) {
    // No-op when the pane is gone or already shows this tab — but still dedupe in
    // case the same tab lingers in another pane.
    return dedupeExcept(root, tab, leafId);
  }
  const updated = mapLeaf(root, leafId, (leaf) => ({ ...leaf, tab }));
  return dedupeExcept(updated, tab, leafId);
}

/**
 * Whether two trees would render the SAME split DOM: same shape, the same leaf ids
 * in the same order, and the same split directions and ratios.
 *
 * This is what "does the DOM need rebuilding" actually asks, and it is deliberately
 * NOT reference identity. Most tree ops here preserve node references when nothing
 * changed (mapAllLeaves), but not all do: setRatio rebuilds every SplitNode it walks,
 * so persisting a divider drag hands back a fresh root describing the layout already
 * on screen — the drag applied the ratio to the live DOM as it went. Treating that as
 * a change re-inserts every pane container, and re-inserting a container detaches it,
 * which drops the scroll offset of its descendants and rewinds a scrolled terminal
 * (#1894, and the resize residual its local Codex review found).
 *
 * A leaf's `tab` is excluded on purpose. The DOM keyed off a leaf is its pane
 * CONTAINER, which is keyed by leaf id; which tab that pane is bound to is settled
 * separately by reconcile's identity/staleAddress check, and it rebuilds the terminal
 * inside the container without disturbing the container itself. Comparing tab here
 * would rebuild the whole DOM whenever a tab merely moved ordinal — re-arming the very
 * rewind this exists to prevent, on any out-of-band reorder.
 */
export function sameLayout(a: LayoutNode | null, b: LayoutNode | null): boolean {
  if (a === b) {
    return true; // the common no-op resync settles without a walk
  }
  if (a === null || b === null) {
    return false;
  }
  if (a.kind === "leaf" || b.kind === "leaf") {
    return a.kind === "leaf" && b.kind === "leaf" && a.id === b.id;
  }
  return (
    a.id === b.id &&
    a.dir === b.dir &&
    a.ratio === b.ratio &&
    sameLayout(a.a, b.a) &&
    sameLayout(a.b, b.b)
  );
}

/** Sets the split `splitId`'s divider ratio, clamped to [0.1, 0.9] so a pane can be
 *  shrunk but never collapsed to nothing by a drag. */
export function setRatio(root: LayoutNode, splitId: string, ratio: number): LayoutNode {
  const clamped = Math.min(0.9, Math.max(0.1, ratio));
  const rec = (node: LayoutNode): LayoutNode => {
    if (node.kind === "leaf") {
      return node;
    }
    if (node.id === splitId) {
      return { ...node, ratio: clamped };
    }
    return { ...node, a: rec(node.a), b: rec(node.b) };
  };
  return rec(root);
}

/**
 * Reconciles a layout against the session's current tab list: clamps every leaf's
 * tab into [0, tabCount-1] (a tab closed elsewhere would otherwise stream a
 * nonexistent tab) and drops the resulting duplicate panes, keeping the first. Used
 * when another client grows/shrinks the tab list out from under a split.
 */
export function validate(root: LayoutNode, tabCount: number): LayoutNode {
  const max = Math.max(0, tabCount - 1);
  const clamped = mapAllLeaves(root, (leaf) => (leaf.tab > max ? { ...leaf, tab: max } : leaf));
  const seen = new Set<number>();
  let cur = clamped;
  for (const l of leaves(clamped)) {
    if (seen.has(l.tab)) {
      cur = closeLeaf(cur, l.id) ?? cur;
    } else {
      seen.add(l.tab);
    }
  }
  return cur;
}


/** Whether two ordered tab-identity lists are element-wise equal (the mid-drag
 *  tab-set-change check). */
function sameTabs(a: string[], b: string[]): boolean {
  if (a.length !== b.length) {
    return false;
  }
  return a.every((v, i) => v === b[i]);
}

/** Whether the tab list was REBOUND between two snapshots — i.e. whether any pane
 *  might now be showing a different tab than it was built for (#1779).
 *
 *  The subtle case this exists for: a resync can change WHICH tab lives at an index
 *  without changing the tab COUNT, when another client closes a tab and creates a
 *  replacement before this browser fetches. The layout tree validates on count
 *  alone, so it comes back structurally identical and a tree-only check concludes
 *  there is nothing to do — leaving a terminal attached to a tab_id that no longer
 *  exists, or an iframe pointed at a dead target. Comparing identity/kind/target
 *  element-wise is what catches it.
 *
 *  Exported for direct unit coverage: it is pure, and it is the guard the
 *  stale-pane bug turns on. */
export function tabsRebound(
  prevIds: string[],
  prevKinds: number[],
  prevTargets: (string | undefined)[],
  ids: string[],
  kinds: number[],
  targets: (string | undefined)[],
): boolean {
  return (
    !sameTabs(prevIds, ids) ||
    !sameTabs(prevKinds.map(String), kinds.map(String)) ||
    !sameTabs(
      prevTargets.map((t) => t ?? ""),
      targets.map((t) => t ?? ""),
    )
  );
}

/** Re-points every leaf at wherever ITS OWN tab now sits, given the tab identity
 *  list before and after a resync (#1779).
 *
 *  A leaf stores an ORDINAL, but what the user put in that pane is a TAB. Those
 *  agree only until the tab list shifts underneath: if a pane shows tab B at index 2
 *  and another client closes a lower tab A and creates C, B is now at index 1 while
 *  the leaf still says 2 — which is C. Reconciling from the leaf's stale ordinal
 *  therefore rebinds the pane to C: a misroute, and precisely the one the stable id
 *  exists to prevent. Detecting that the tab set changed is not enough; the leaves
 *  have to be MOVED before anything reads them.
 *
 *  Surviving tabs follow their identity. A leaf whose identity is gone (its tab was
 *  really closed) keeps its ordinal and lets reconcile rebuild it against whatever
 *  now occupies that slot — the same degradation as a plain tab-count shrink. The one
 *  conflict is a dead leaf sitting on an ordinal a survivor has just claimed: the
 *  survivor wins (it is the pane that actually holds that tab) and the dead leaf is
 *  closed, since the tab it was showing no longer exists. That priority matters —
 *  validate() also drops duplicates, but it keeps the FIRST leaf in visual order,
 *  which would just as easily evict the survivor and keep the pane whose tab died.
 *
 *  Node references are preserved when nothing moves, so the common no-op resync does
 *  not churn a rebuild. */
export function remapByIdentity(root: LayoutNode, prevIds: string[], ids: string[]): LayoutNode {
  if (prevIds.length === 0) {
    return root; // nothing was bound yet — no leaf can be stale
  }
  // Where each surviving leaf's tab moved to. An identity absent from `ids` is a tab
  // that is really gone and gets no claim.
  const moved = new Map<string, number>();
  const claimed = new Set<number>();
  for (const leaf of leaves(root)) {
    const identity = prevIds[leaf.tab];
    if (identity === undefined || identity === "") {
      continue;
    }
    const next = ids.indexOf(identity);
    if (next >= 0) {
      moved.set(leaf.id, next);
      claimed.add(next);
    }
  }
  if (moved.size === 0) {
    return root;
  }
  let cur = mapAllLeaves(root, (leaf) => {
    const next = moved.get(leaf.id);
    return next === undefined || next === leaf.tab ? leaf : { ...leaf, tab: next };
  });
  // Drop a dead-identity leaf that now collides with a survivor's claim.
  for (const leaf of leaves(cur)) {
    if (!moved.has(leaf.id) && claimed.has(leaf.tab)) {
      cur = closeLeaf(cur, leaf.id) ?? cur;
    }
  }
  return cur;
}

/** Resolves a dropped tab payload to the ordinal it should bind, or null to CANCEL
 *  the drop (#1738/#1779).
 *
 *  Two branches, and which one runs is decided by whether the dragged tab HAS a
 *  real daemon id — never by whether some identity string happens to be non-empty:
 *
 *  - A real stable id is looked up in the CURRENT real-id list. Wherever that exact
 *    tab now sits is where the pane binds; if it was closed mid-drag it resolves to
 *    nothing and the drop cancels. A concurrent reorder cannot misroute it.
 *  - No id (a legacy/pre-#1738 tab) has no collision-proof handle, so its drag-time
 *    index is only trusted when the whole tab-set snapshot still matches (#1737).
 *    That guard is the ONLY protection such a tab has, which is why a synthesized
 *    `kind:name` must never reach the branch above and skip it: a legacy tab closed
 *    and recreated under the same kind/name mid-drag would otherwise resolve
 *    straight onto its replacement.
 *
 *  Exported for direct unit coverage: it is pure, and it is the misroute fix. */
export function resolveDragTab(
  drag: { id?: string; index: number; tabs: string[] },
  tabRealIds: string[],
  tabIds: string[],
  tabCount: number,
): number | null {
  if (drag.id) {
    const tab = tabRealIds.indexOf(drag.id);
    return tab < 0 ? null : tab; // resolved to nothing → the tab was closed mid-drag
  }
  const tab = drag.index;
  if (tab < 0 || tab >= tabCount || !sameTabs(drag.tabs, tabIds)) {
    return null;
  }
  return tab;
}

/** The drag payload a tab-bar drag stamps into the dataTransfer (ui.ts): the dragged
 *  tab's REAL daemon id when it has one (#1738) — what the drop resolves to a current
 *  ordinal, so a mid-drag reorder/close can't misroute — plus the ordinal index and an
 *  ordered identity snapshot, the guarded legacy fallback for a tab with no id. `id`
 *  is EMPTY for an id-less tab and never carries a synthesized identity (#1779): see
 *  resolveDragTab. */
export interface DragPayload {
  id?: string;
  index: number;
  tabs: string[];
}
