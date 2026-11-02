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

/** The sibling subtree of the leaf `leafId` — the OTHER child of the leaf's parent
 *  split — or null when the leaf IS the whole tree (a single-leaf layout, which
 *  closeLeaf already rejects as the un-closable last pane).
 *
 *  This is the subtree closeLeaf() substitutes in place of the collapsed parent to
 *  absorb the closed pane's screen region, so it is the natural focus successor when
 *  the closed pane held the keyboard: the pane that grew into the freed space is
 *  where an operator's eye and hand already are, and focusing a leaf in a DIFFERENT
 *  root branch (the previous leaves(root)[0] formula) sends keystrokes to an
 *  unrelated session. Reference-stable: the returned node survives closeLeaf()
 *  unchanged (remove() hands the sibling back as-is), so it can be read on the tree
 *  BEFORE the close and still name the same subtree after.
 *
 *  Returns the sibling subtree itself (which may be a single leaf OR a split whose
 *  own first leaf leaves() selects); which leaf of a multi-leaf sibling subtree gets
 *  focus is a separate, deliberately narrower policy the caller applies. */
export function siblingSubtreeOf(root: LayoutNode, leafId: string): LayoutNode | null {
  if (root.kind === "leaf") {
    return null; // a single-leaf tree has no sibling
  }
  // The leaf is a DIRECT child of this split: the sibling is the other child.
  if (root.a.kind === "leaf" && root.a.id === leafId) {
    return root.b;
  }
  if (root.b.kind === "leaf" && root.b.id === leafId) {
    return root.a;
  }
  // Otherwise the leaf lives inside one child; descend into the side that contains
  // it. The sibling (the other child of the leaf's parent) sits inside that same
  // child, not in this split's other half.
  if (findLeaf(root.a, leafId)) {
    return siblingSubtreeOf(root.a, leafId);
  }
  return siblingSubtreeOf(root.b, leafId);
}

/** The leaf that should take the keyboard when `droppedId` disappears from `prev`
 *  WITHOUT going through closePane (an out-of-band shrink — reconcile only sees the
 *  new tree). The same successor rule closePane applies: the first surviving leaf of
 *  the dropped leaf's sibling subtree, the pane that grew into its region (#5983).
 *  When that whole subtree is gone too, the next-nearest ancestor's sibling is tried
 *  before giving up, so the successor is always the closest survivor and never an
 *  unrelated root-first pane. null when `prev` has no surviving relative of the
 *  dropped leaf; the caller then falls back to the first leaf of the new tree. */
export function focusSuccessorAfterDrop(
  prev: LayoutNode | null,
  droppedId: string,
  survivors: ReadonlySet<string>,
): string | null {
  // Sibling subtrees from the nearest ancestor outward.
  const siblings: LayoutNode[] = [];
  let node = prev;
  while (node && node.kind === "split") {
    const inA = findLeaf(node.a, droppedId);
    if (!inA && !findLeaf(node.b, droppedId)) {
      return null;
    }
    siblings.unshift(inA ? node.b : node.a);
    node = inA ? node.a : node.b;
  }
  for (const sib of siblings) {
    const hit = leaves(sib).find((l) => survivors.has(l.id));
    if (hit) {
      return hit.id;
    }
  }
  return null;
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

// --- Pane minimums (#6429) ----------------------------------------------------
//
// The web port of the TUI's pane floor: ui/layout/grid.go gives every workspace
// pane at least PaneMinWidth = 40 terminal columns and, when the terminal cannot
// give that to every open pane, hides the least-recently-focused ones with a
// "too narrow" status rather than drawing slivers. The web tree is richer — it
// nests row AND column splits, so "does it fit" is per-leaf, not per-pane-count —
// but the contract is the same: no rendered pane under the floor, and overflow
// degrades visibly (split.ts names the hidden count) instead of clipping.

/** The TUI's 40-column pane floor in px. One column is ~8px at the client's
 *  default terminal pitch, so a pane narrower than this cannot even show the
 *  TUI's smallest usable terminal. */
export const PANE_MIN_WIDTH_PX = 320;

/** The floor on the axis a column split squeezes. The TUI splits horizontally
 *  only, so there is no number to port; ~4 terminal rows keeps a stacked pane's
 *  scrollback readable and its header usable. */
export const PANE_MIN_HEIGHT_PX = 80;

/** Divider thickness along its split's axis (styles.css --af-space-1). */
export const PANE_DIVIDER_PX = 4;

/** The smallest size `node` may take along `dir` while every leaf inside it still
 *  meets its floor at the CURRENT ratios. Across the split's axis the children
 *  overlap, so the larger floor governs. Along it the children coexist in
 *  ratio shares of the content span: side a renders at ratio of the span, so
 *  keeping it at its floor needs minA / ratio — a skewed split needs far more
 *  room than the floors' naive sum, which is exactly the gap an ancestor
 *  divider drag must respect (#6560).
 *
 * `visible`, when passed, is the set of leaf ids the fit left rendered: a leaf
 *  outside it contributes 0, and a split side with nothing rendered takes no
 *  flex share at all — its divider is hidden too — so the other side alone
 *  governs. Without it every leaf counts, which is what made a drag clamp
 *  deadlock on a fit-hidden layout (#6573). */
export function minSubtreeSize(
  node: LayoutNode,
  dir: SplitDir,
  leafMinPx: number,
  dividerPx: number,
  visible?: ReadonlySet<string>,
): number {
  if (node.kind === "leaf") {
    return visible !== undefined && !visible.has(node.id) ? 0 : leafMinPx;
  }
  const a = minSubtreeSize(node.a, dir, leafMinPx, dividerPx, visible);
  const b = minSubtreeSize(node.b, dir, leafMinPx, dividerPx, visible);
  if (node.dir !== dir) {
    return Math.max(a, b);
  }
  if (a === 0 || b === 0) {
    return Math.max(a, b);
  }
  // A degenerate ratio (0 or 1, reachable only by corrupt retained state)
  // would make a side's demand infinite; clamping holds the drag legal rather
  // than freezing it outright.
  const r = Math.min(Math.max(node.ratio, 0.01), 0.99);
  return Math.max(a / r, b / (1 - r)) + dividerPx;
}

/** The rendered size of every leaf if `root` were tiled into `w`×`h` px: splits
 *  divide along their axis by ratio, each reserving `dividerPx` for its handle. */
export function leafSizes(root: LayoutNode, w: number, h: number, dividerPx: number): Map<string, { w: number; h: number }> {
  const out = new Map<string, { w: number; h: number }>();
  const walk = (node: LayoutNode, nw: number, nh: number): void => {
    if (node.kind === "leaf") {
      out.set(node.id, { w: nw, h: nh });
      return;
    }
    if (node.dir === "row") {
      const content = Math.max(0, nw - dividerPx);
      walk(node.a, content * node.ratio, nh);
      walk(node.b, content * (1 - node.ratio), nh);
    } else {
      const content = Math.max(0, nh - dividerPx);
      walk(node.a, nw, content * node.ratio);
      walk(node.b, nw, content * (1 - node.ratio));
    }
  };
  walk(root, w, h);
  return out;
}

/** The layout as it may actually be drawn inside `w`×`h` px: hides leaves until
 *  every remaining one clears the floors on both axes — or only one is left,
 *  which always shows (a single pane is exempt, like the TUI's pre-split pane A).
 *
 *  `protectedId` is the focused leaf: it is the last to hide, so the user's
 *  attention is never the thing that disappears. Other leaves hide in reverse
 *  visual order (rightmost/bottommost first), preferring violators — a sliver
 *  was unreadable anyway — and falling back to healthy leaves only when hiding
 *  one is the only way to give the protected leaf its floor.
 *
 *  The returned tree shares structure with `root` where leaves survive; the
 *  dropped ids come back as `hidden` so the caller can say how many panes the
 *  layout wanted but could not fit. */
export function fitToSize(
  root: LayoutNode,
  w: number,
  h: number,
  protectedId: string | null,
): { tree: LayoutNode; hidden: string[] } {
  let tree = root;
  const hidden: string[] = [];
  for (;;) {
    const sizes = leafSizes(tree, w, h, PANE_DIVIDER_PX);
    const ids = leaves(tree).map((l) => l.id);
    const violators = new Set(
      [...sizes].filter(([, s]) => s.w < PANE_MIN_WIDTH_PX || s.h < PANE_MIN_HEIGHT_PX).map(([id]) => id),
    );
    if (violators.size === 0 || ids.length <= 1) {
      break;
    }
    const droppable = [...ids].reverse().filter((id) => id !== protectedId);
    const drop = droppable.find((id) => violators.has(id)) ?? droppable[0];
    if (drop === undefined) {
      break; // only the protected leaf remains
    }
    const next = closeLeaf(tree, drop);
    if (next === null) {
      break; // defensive: a leaf that closeLeaf refuses was the last one
    }
    tree = next;
    hidden.push(drop);
  }
  return { tree, hidden };
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


/** Whether two ordered per-tab lists are element-wise equal — the primitive behind
 *  every "did the tab list change out from under us" check: the mid-drag
 *  tab-set-change guard (resolveDragTab), the rebind detector (tabsRebound), and
 *  split.ts's rename detector, which compares tab NAMES to decide whether the pane
 *  headers need repainting (#1813). Exported for that last caller and for direct
 *  unit coverage. */
export function sameTabs(a: string[], b: string[]): boolean {
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

const isSynth = (id: string): boolean => /^\d+:/.test(id);

/** The legacy `kind:name` form of every tab, parallel to the roster. A tab that
 *  has a real id still HAS a kind and a name, so this is what its id-less
 *  predecessor was tracked as — the exact key that pairs a backfill (#6001). */
export function synthIdentities(kinds: readonly number[], names: readonly string[]): string[] {
  return kinds.map((k, i) => `${k}:${names[i] ?? ""}`);
}

/** The synthesized identities in `prevIds` that a real id in `ids` replaced (#1779:
 *  the daemon resolves an id-less tab's real id after a snapshot tracked it as
 *  `kind:name`). A vanished synthesized id is ADOPTED when
 *   1. a new real id's own `kind:name` (`nextSynth`, parallel to `ids`) is exactly
 *      that id — the precise pairing, and the only one that can tell a closed
 *      legacy tab from its live neighbour (#6001); else
 *   2. a new real id remains unpaired in the same gap between identities both lists
 *      share. Here the ids carry no name, so WHICH vanished tab received it cannot
 *      be told; every candidate in that gap is kept alive rather than guessing a
 *      positional pairing that can bind the id to the CLOSED tab and drop the live
 *      pane's. A gap with no new real id adopts nothing: those tabs really closed. */
function adoptedSynthIdentities(
  prevIds: string[],
  ids: string[],
  nextSynth?: readonly string[],
): { exact: Set<string>; fallback: Set<string> } {
  const shared = new Set(prevIds.filter((id) => id !== "" && ids.includes(id)));
  const isNewReal = (id: string): boolean => id !== "" && !isSynth(id) && !prevIds.includes(id);
  const adopted = new Set<string>();
  const fallback = new Set<string>();
  // 1. exact pairing by the new tab's kind:name.
  const vanished = new Set(prevIds.filter((id) => id !== "" && isSynth(id) && !ids.includes(id)));
  const unpaired = new Set<number>();
  ids.forEach((id, i) => {
    if (!isNewReal(id)) {
      return;
    }
    const synth = nextSynth?.[i];
    if (synth !== undefined && vanished.has(synth) && !adopted.has(synth)) {
      adopted.add(synth);
    } else {
      unpaired.add(i);
    }
  });
  // 2. per gap between shared anchors, any unpaired new real id adopts the gap.
  const gapOfNew = new Set<string>();
  let key = "";
  ids.forEach((id, i) => {
    if (shared.has(id)) {
      key = id;
    } else if (unpaired.has(i)) {
      gapOfNew.add(key);
    }
  });
  key = "";
  for (const id of prevIds) {
    if (shared.has(id)) {
      key = id;
    } else if (vanished.has(id) && !adopted.has(id) && gapOfNew.has(key)) {
      fallback.add(id);
    }
  }
  return { exact: adopted, fallback };
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
 *  now occupies that slot — the same degradation as a plain tab-count shrink — UNLESS
 *  a pure shrink left that ordinal with no slot at all (the closed tab was the
 *  highest-ordinal one and nothing was created to take its place) AND the slot it
 *  would clamp down onto is already claimed by a survivor. Only then is the dead
 *  leaf closed here: validate() would otherwise clamp it down onto the survivor's
 *  ordinal and, keeping the FIRST leaf in visual order, evict that survivor —
 *  handing the pane's live terminal to the tab that just died. When the clamp
 *  target is NOT claimed — a backfilled survivor whose identity string changed
 *  (#1779) so `moved` doesn't track it, or an unshown tab whose pane a multi-pane
 *  layout legitimately keeps — validate() rebinds the leaf to that unclaimed slot
 *  harmlessly, so this loop leaves it alone. The one other conflict is a dead leaf
 *  sitting on an ordinal a survivor has just claimed: the survivor wins (it is the
 *  pane that actually holds that tab) and the dead leaf is closed, since the tab it
 *  was showing no longer exists. Both priorities matter for the same reason —
 *  validate() also drops duplicates, but it keeps the FIRST leaf in visual order,
 *  which would just as easily evict the survivor and keep the pane whose tab died.
 *
 *  Node references are preserved when nothing moves, so the common no-op resync does
 *  not churn a rebuild. */
export function remapByIdentity(
  root: LayoutNode,
  prevIds: string[],
  ids: string[],
  nextSynth?: readonly string[],
): LayoutNode {
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
  // Drop a dead-identity leaf whose ordinal no longer has a slot after a pure
  // shrink (the closed tab was the highest-ordinal one, nothing replaced it) — but
  // ONLY the DEAD one, so validate()'s clamp does not evict a SURVIVOR. validate()
  // clamps every out-of-range leaf to the last ordinal (`ids.length - 1`) and then
  // drops duplicates keeping the FIRST in visual order; that evicts a survivor at
  // that ordinal only when a dead leaf is also clamped onto it (and happens to come
  // first). Closing the dead leaf here pre-empts that eviction.
  //
  // A clamp collision has TWO shapes, and the dead leaf is in a different position
  // in each:
  //  1. The dead leaf is OUT OF RANGE and a survivor holds `max`. This is the common
  //     shrink (the closed tab was the highest-ordinal one). Close the dead leaf so
  //     validate() does not clamp it onto the survivor.
  //  2. A backfilled survivor is out of range and the DEAD leaf holds `max` (a
  //     different, lower tab closed while the snapshot also adopted a real id onto
  //     the survivor — #1779). Closing the out-of-range survivor here would discard
  //     its live terminal; the dead holder at `max` is the leaf to drop, so the
  //     survivor rebinds to `max` via validate() instead.
  // The holder at `max` may itself be a backfilled survivor whose identity string
  // changed (#1779): a synthesized `kind:name` that a later snapshot replaces with a
  // real id is the SAME tab to ui.ts, but `moved` matches by identity string, so the
  // backfill reads as "identity gone" and the leaf falls out of `moved`/`claimed`
  // while staying at its (in-range) ordinal. `claimed` (or any leaf at `max`) alone
  // cannot tell a survivor holder from a dead one, so the dead leaf is identified by
  // its identity: a real id gone from `ids` is a genuinely closed tab, while a
  // synthesized `kind:name` gone from `ids` is an ADOPTED survivor (not closed) as
  // long as a backfill happened (a real id appeared in `ids` that was not in
  // `prevIds`). A synthesized id gone with NO backfill is a closed legacy tab (pre
  // #1738, no id) and degrades like any other dead leaf. A real daemon id is hex or
  // `t-` (session/tab.go newTabID); a synthesized id is `${kind}:${name}`
  // (ui.ts tabIdentity), so a leading `digits:` distinguishes them.
  //
  // When `max` is empty (no leaf there), validate() rebinds an out-of-range leaf to
  // the unclaimed slot with no duplicate to drop — closing it would collapse a
  // multi-pane layout the new tab count still admits, so this loop leaves it alone
  // (the [A,_,C]→[A,B] case keeps C's pane, clamped to the unshown B).
  // `closeLeaf` returns null for the last pane, so `?? cur` leaves a sole dead leaf
  // in place to degrade as before (validate clamps it to max).
  const max = ids.length - 1;
  const maxLeaf = leaves(cur).find((l) => l.tab === max) ?? null;
  if (maxLeaf) {
    // Which synthesized identities were ADOPTED by a real id (backfilled), not merely
    // "some backfill happened somewhere": a session-wide flag let one id-less tab's
    // backfill mark every other missing synthesized tab alive, so a closed legacy tab
    // survived and the clamp evicted the real survivor's pane (#5982).
    const { exact, fallback } = adoptedSynthIdentities(prevIds, ids, nextSynth);
    const adopted = new Set([...exact, ...fallback]);
    // A !moved leaf is DEAD (its tab was genuinely closed) when its prev identity is a
    // real id that's gone from `ids`, or a synthesized `kind:name` that no real id
    // adopted (a closed pre-#1738 legacy tab). An adopted survivor (synthesized id
    // gone, a real id took its place) is NOT dead — its pane must be preserved.
    const deadIdentity = (tab: number): boolean => {
      const prev = prevIds[tab];
      if (prev === undefined || prev === "") {
        return true;
      }
      return !isSynth(prev) || !adopted.has(prev);
    };
    // A pane whose tab was matched EXACTLY (its real id kept, or its backfill paired
    // by kind:name) outranks a pane kept alive only by the ambiguous gap fallback:
    // when such a tentative pane is out of range and the slot it would clamp onto is
    // held by an exact survivor, validate() would keep whichever is visually first —
    // possibly the closed tab's pane (#6025). The exact one wins; close the other.
    const exactHolderAtMax = moved.has(maxLeaf.id) || exact.has(prevIds[maxLeaf.tab] ?? "");
    const tentative = (tab: number): boolean => fallback.has(prevIds[tab] ?? "");
    // (1) Close a dead leaf clamped out of range onto a survivor's slot at `max`.
    for (const leaf of leaves(cur)) {
      if (
        !moved.has(leaf.id) &&
        leaf.tab >= ids.length &&
        (deadIdentity(leaf.tab) || (exactHolderAtMax && tentative(leaf.tab)))
      ) {
        cur = closeLeaf(cur, leaf.id) ?? cur;
      }
    }
    // (2) A dead leaf HOLDING `max`, with a backfilled survivor still out of range
    //     that would clamp onto it: close the dead holder so the survivor rebinds to
    //     `max`, instead of closing the survivor and handing its pane to the dead tab.
    if (
      !moved.has(maxLeaf.id) &&
      deadIdentity(maxLeaf.tab) &&
      leaves(cur).some((l) => !moved.has(l.id) && l.tab >= ids.length && !deadIdentity(l.tab))
    ) {
      cur = closeLeaf(cur, maxLeaf.id) ?? cur;
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

/** What a touch drop on a pane resolved to (split.ts dropTabAt). Two questions
 *  need two answers (#4434): `landed` is the gesture-consumption fact — a pane
 *  was under the release — and `changed` is the mutation fact — the layout
 *  actually committed. A landed-but-rejected drop (stale payload, or the sole
 *  tab released on its own pane's edge) is still consumed, but nothing about
 *  the session's composition changed, so it must not retire user state the way
 *  a real tab move does. */
export interface TabDropResult {
  landed: boolean;
  changed: boolean;
}
