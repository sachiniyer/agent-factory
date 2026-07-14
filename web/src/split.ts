// The split-pane view (feat(web): drag-and-drop split tabs) — the imperative half of
// the per-instance split layout, the way terminal.ts is the imperative half of the
// single attach terminal it generalizes. It owns:
//
//   - a per-instance layout TREE (layout.ts): which tabs are shown, split how, at
//     what ratios. The default is a single leaf, so a session that is never split
//     renders exactly like the pre-split single terminal.
//   - one live AttachTerminal PER LEAF: multiple concurrent xterms, each with its own
//     WS to /v1/sessions/{id}/stream?tab=<idx>, each self-healing + fit/resizing
//     independently. The daemon's ptyBroker already supports N concurrent
//     subscribers, so this is purely a client-side layout feature.
//   - drag-and-drop splitting: dropping a tab (dragged from the tab bar) on a pane's
//     edge splits in that direction with the dragged tab in the new pane; dropping on
//     the center replaces the pane's tab. Drop zones are shown on dragover.
//   - a draggable divider per split to resize (persists the ratio), a × to close a
//     pane (collapsing its split so the sibling fills), and click-to-focus.
//
// Focus is per-pane and feeds the #1694 nav model: the FOCUSED pane owns the keyboard
// (its status shows in the pane header, keys go to it), and index.ts wires an Alt+j/k
// cycle + Alt+w close through here. Retained layouts are kept per session in memory,
// so switching instances shows each instance's own split; the live terminals exist
// only for the currently-shown instance (rebuilt from the retained tree on return),
// mirroring how the single terminal was disposed+rebuilt on every selection change.

import {
  closeLeaf,
  type Edge,
  findLeaf,
  type LayoutNode,
  type LeafNode,
  leafCount,
  leaves,
  replaceTab,
  setRatio,
  singleLeaf,
  type SplitNode,
  splitLeaf,
  TAB_DND_MIME,
  validate,
} from "./layout.js";
import { AttachTerminal, type TerminalStatus } from "./terminal.js";

/** How close (as a fraction of the pane) to an edge the pointer must be for the drop
 *  to split rather than replace — the outer 30% band on each side is an edge zone. */
const EDGE_BAND = 0.3;

export interface SplitCallbacks {
  /** The FOCUSED pane's connection state, for the pane-header status line. */
  onStatus(status: TerminalStatus): void;
  /** Real DOM focus of any pane's terminal, for the nav-vs-terminal mode (#1693):
   *  true when a pane's xterm gains the keyboard, false when focus leaves every pane. */
  onFocusChange(focused: boolean): void;
  /** The layout mirror the store keeps for the tab bar: which tab the focused pane
   *  shows (activeTab), which tabs are shown across all panes, and the pane count.
   *  Fired only on a real change, so it can safely write the store without looping. */
  onLayout(info: { focusedTab: number; shownTabs: number[]; paneCount: number }): void;
}

/** One live pane: its persistent DOM (reused across tree re-renders so the xterm is
 *  never torn down by a sibling's split) and the terminal bound to its current tab. */
interface Pane {
  leafId: string;
  container: HTMLElement;
  host: HTMLElement;
  label: HTMLElement;
  overlay: HTMLElement;
  term: AttachTerminal | null;
  tab: number;
  status: TerminalStatus;
}

function el(tag: string, cls: string): HTMLElement {
  const node = document.createElement(tag);
  node.className = cls;
  return node;
}

export class SplitView {
  // Retained layout per session id (in-memory; a nice-to-have to persist across
  // reload is out of scope for v1). Keyed by the stable session id.
  private readonly trees = new Map<string, LayoutNode>();
  private readonly panes = new Map<string, Pane>();

  private sessionId: string | null = null;
  private token: string | null = null;
  private tabCount = 1;
  private tree: LayoutNode | null = null;
  private focusedId: string | null = null;

  // Debounces the "focus left every pane" report so a click that moves focus A→B
  // (blur A, then focus B) doesn't flap the nav mode through rail and back.
  private blurTimer: number | null = null;

  // Last values reported via onLayout, so a no-op reconcile never re-fires it (which
  // would re-enter the store→rerender→setSession loop).
  private lastFocusedTab = -1;
  private lastShown = "";
  private lastPaneCount = 0;

  constructor(
    private readonly host: HTMLElement,
    private readonly cb: SplitCallbacks,
  ) {}

  /**
   * Shows `sessionId`'s layout, building/rebuilding terminals as needed. Called on
   * every selection/tab change: a NEW session rebuilds from its retained tree (or a
   * fresh single leaf bound to `initialTab`); the SAME session only re-validates the
   * tree against the current tab list (a tab closed elsewhere). Cheap on a no-op.
   */
  setSession(sessionId: string | null, token: string | null, tabCount: number, initialTab: number): void {
    this.token = token;
    if (sessionId === null || token === null) {
      this.teardown();
      this.sessionId = null;
      this.tree = null;
      this.report();
      return;
    }
    if (sessionId === this.sessionId) {
      // Same session: reconcile the retained tree against a possibly-changed tab list.
      this.tabCount = tabCount;
      const before = this.tree;
      this.tree = validate(this.tree ?? singleLeaf(initialTab), tabCount);
      if (this.trees.get(sessionId) !== this.tree) {
        this.trees.set(sessionId, this.tree);
      }
      if (before !== this.tree) {
        this.reconcile();
        this.report();
      }
      return;
    }
    // A different session: drop the old session's live terminals (its tree stays
    // retained) and build the new one's.
    this.teardown();
    this.sessionId = sessionId;
    this.tabCount = tabCount;
    const retained = this.trees.get(sessionId);
    this.tree = validate(retained ?? singleLeaf(initialTab), tabCount);
    this.trees.set(sessionId, this.tree);
    // Focus the first pane of the newly shown session by default.
    this.focusedId = leaves(this.tree)[0]?.id ?? null;
    this.reconcile();
    this.report();
  }

  /** Rebinds the FOCUSED pane to show `tab` (a 1-9 key or a tab-bar click on the
   *  focused pane). No-op without a focused pane. Does not steal DOM focus. */
  setFocusedTab(tab: number): void {
    if (!this.tree || !this.focusedId) {
      return;
    }
    this.tree = replaceTab(this.tree, this.focusedId, tab);
    this.commit();
  }

  /** Gives the keyboard to the focused pane's terminal (attach). */
  focus(): void {
    const pane = this.focusedId ? this.panes.get(this.focusedId) : null;
    pane?.term?.focus();
  }

  /** Takes the keyboard away from every pane (back to rail nav). */
  blur(): void {
    for (const pane of this.panes.values()) {
      pane.term?.blur();
    }
  }

  /** Moves pane focus by `delta` (wrapping) and attaches the newly focused pane. */
  cyclePane(delta: 1 | -1): void {
    if (!this.tree) {
      return;
    }
    const ids = leaves(this.tree).map((l) => l.id);
    if (ids.length <= 1) {
      this.focus();
      return;
    }
    const cur = this.focusedId ? ids.indexOf(this.focusedId) : -1;
    const next = ids[(cur + delta + ids.length) % ids.length];
    if (next) {
      this.focusPane(next);
      this.focus();
    }
  }

  /** Closes the focused pane, collapsing its split. No-op when it is the only pane
   *  (a session always shows at least one terminal — close the TAB instead). */
  closeFocusedPane(): void {
    if (this.focusedId) {
      this.closePane(this.focusedId);
    }
  }

  /** Whether the layout currently has more than one pane. */
  isSplit(): boolean {
    return this.tree ? leafCount(this.tree) > 1 : false;
  }

  /** Tears down every live terminal, clears the host, AND drops the retained
   *  per-instance trees (logout). Keeping the trees across a logout would leave them
   *  pointing at torn-down panes, so a re-login would resurrect a stale split instead
   *  of the single-leaf default — so a fresh login starts clean. (Instance→instance
   *  switches never call this; they go through setSession, which keeps the trees.) */
  dispose(): void {
    this.teardown();
    this.trees.clear();
    this.sessionId = null;
    this.tree = null;
    // Reset the onLayout dedup trackers so the first select after a fresh login always
    // re-reports its (single-leaf) layout to the store.
    this.lastFocusedTab = -1;
    this.lastShown = "";
    this.lastPaneCount = 0;
  }

  // --- internal: mutation commit --------------------------------------------

  /** Persists the current tree for the session, re-renders, and reports the layout. */
  private commit(): void {
    if (this.sessionId && this.tree) {
      this.trees.set(this.sessionId, this.tree);
    }
    this.reconcile();
    this.report();
  }

  private closePane(leafId: string): void {
    if (!this.tree) {
      return;
    }
    const next = closeLeaf(this.tree, leafId);
    if (next === null) {
      return; // the last pane can't be closed
    }
    this.tree = next;
    // Re-point focus if the closed pane held it.
    if (this.focusedId === leafId) {
      this.focusedId = leaves(this.tree)[0]?.id ?? null;
    }
    this.commit();
    this.focus();
  }

  // --- internal: reconcile tree → DOM + terminals ---------------------------

  private teardown(): void {
    for (const pane of this.panes.values()) {
      pane.term?.dispose();
    }
    this.panes.clear();
    this.host.replaceChildren();
    this.host.classList.remove("af-split-multi");
    this.focusedId = null;
  }

  /** Brings the live panes + DOM in line with the current tree: disposes gone panes,
   *  (re)creates terminals whose tab changed, rebuilds the split wrappers (reusing
   *  the persistent pane containers so surviving xterms are only reparented, never
   *  recreated), and refreshes focus/head chrome. */
  private reconcile(): void {
    if (!this.tree || !this.sessionId || this.token === null) {
      return;
    }
    const desired = leaves(this.tree);
    const wanted = new Set(desired.map((l) => l.id));

    // Drop panes no longer in the tree.
    for (const [id, pane] of this.panes) {
      if (!wanted.has(id)) {
        pane.term?.dispose();
        this.panes.delete(id);
      }
    }

    // Ensure a container exists for every desired leaf (DOM only, no terminal yet).
    for (const leaf of desired) {
      if (!this.panes.has(leaf.id)) {
        this.panes.set(leaf.id, this.createPane(leaf));
      }
    }

    // Keep a valid focus.
    if (!this.focusedId || !wanted.has(this.focusedId)) {
      this.focusedId = desired[0]?.id ?? null;
    }

    // Rebuild the split wrappers, inserting the persistent containers. Containers now
    // in the live DOM have real dimensions for the FitAddon.
    const rootEl = this.buildNode(this.tree);
    rootEl.style.flex = "1 1 0";
    this.host.replaceChildren(rootEl);

    const multi = desired.length > 1;
    this.host.classList.toggle("af-split-multi", multi);

    // (Re)create terminals for panes whose bound tab changed (or are brand new).
    for (const leaf of desired) {
      const pane = this.panes.get(leaf.id);
      if (!pane) {
        continue;
      }
      if (!pane.term || pane.tab !== leaf.tab) {
        pane.term?.dispose();
        pane.host.replaceChildren();
        pane.tab = leaf.tab;
        pane.status = "connecting";
        pane.term = new AttachTerminal(pane.host, this.sessionId, this.token, leaf.tab, {
          onStatus: (s) => this.onPaneStatus(leaf.id, s),
          onFocusChange: (f) => this.onPaneFocus(leaf.id, f),
        });
      }
      pane.container.classList.toggle("af-pane-multi", multi);
      pane.label.textContent = `Tab ${leaf.tab + 1}`;
    }

    this.applyFocusClass();
  }

  private createPane(leaf: LeafNode): Pane {
    const container = el("div", "af-pane");
    container.setAttribute("data-leaf", leaf.id);
    const head = el("div", "af-pane-head");
    const label = el("span", "af-pane-label");
    const closeBtn = document.createElement("button");
    closeBtn.type = "button";
    closeBtn.className = "af-pane-close";
    closeBtn.title = "Close pane";
    closeBtn.setAttribute("aria-label", "Close pane");
    closeBtn.textContent = "×";
    closeBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.closePane(leaf.id);
    });
    head.append(label, closeBtn);

    const paneHost = el("div", "af-pane-host");
    const overlay = el("div", "af-drop-overlay");
    container.append(head, paneHost, overlay);

    // Click-to-focus (#1694): mousedown fires before xterm's textarea focus, so the
    // pane is the focused one by the time keys flow.
    container.addEventListener("mousedown", () => this.focusPane(leaf.id));

    const pane: Pane = { leafId: leaf.id, container, host: paneHost, label, overlay, term: null, tab: -1, status: "connecting" };
    this.wireDrop(pane);
    return pane;
  }

  private buildNode(node: LayoutNode): HTMLElement {
    if (node.kind === "leaf") {
      return this.panes.get(node.id)?.container ?? el("div", "af-pane");
    }
    const a = this.buildNode(node.a);
    const b = this.buildNode(node.b);
    a.style.flex = `${node.ratio} 1 0`;
    b.style.flex = `${1 - node.ratio} 1 0`;
    const divider = this.buildDivider(node, a, b);
    const wrap = el("div", `af-split af-split-${node.dir}`);
    wrap.append(a, divider, b);
    return wrap;
  }

  private buildDivider(node: SplitNode, aEl: HTMLElement, bEl: HTMLElement): HTMLElement {
    const divider = el("div", `af-divider af-divider-${node.dir}`);
    divider.setAttribute("role", "separator");
    divider.addEventListener("pointerdown", (e: PointerEvent) => {
      e.preventDefault();
      const parent = divider.parentElement;
      if (!parent) {
        return;
      }
      divider.setPointerCapture(e.pointerId);
      document.body.classList.add(node.dir === "row" ? "af-resizing-col" : "af-resizing-row");
      const rect = parent.getBoundingClientRect();
      const onMove = (ev: PointerEvent) => {
        const ratio =
          node.dir === "row" ? (ev.clientX - rect.left) / rect.width : (ev.clientY - rect.top) / rect.height;
        const clamped = Math.min(0.9, Math.max(0.1, ratio));
        // Mutate the live node's ratio (the tree is not in the store, so an in-place
        // update is fine) and reflect it immediately; the terminals' ResizeObservers
        // re-fit on their own as the flex basis changes.
        node.ratio = clamped;
        aEl.style.flex = `${clamped} 1 0`;
        bEl.style.flex = `${1 - clamped} 1 0`;
      };
      const onUp = (ev: PointerEvent) => {
        divider.releasePointerCapture(ev.pointerId);
        divider.removeEventListener("pointermove", onMove);
        divider.removeEventListener("pointerup", onUp);
        document.body.classList.remove("af-resizing-col", "af-resizing-row");
        // Persist the final ratio into the retained tree by id.
        if (this.tree) {
          this.tree = setRatio(this.tree, node.id, node.ratio);
          if (this.sessionId) {
            this.trees.set(this.sessionId, this.tree);
          }
        }
      };
      divider.addEventListener("pointermove", onMove);
      divider.addEventListener("pointerup", onUp);
    });
    return divider;
  }

  // --- internal: drag-and-drop ----------------------------------------------

  private wireDrop(pane: Pane): void {
    const isTabDrag = (e: DragEvent) => e.dataTransfer?.types.includes(TAB_DND_MIME) ?? false;
    pane.container.addEventListener("dragover", (e: DragEvent) => {
      if (!isTabDrag(e)) {
        return;
      }
      e.preventDefault(); // allow the drop
      if (e.dataTransfer) {
        e.dataTransfer.dropEffect = "move";
      }
      this.showZone(pane, this.zoneAt(pane.container, e.clientX, e.clientY));
    });
    pane.container.addEventListener("dragleave", (e: DragEvent) => {
      // Ignore leave events that only cross into a child of the pane.
      const to = e.relatedTarget as Node | null;
      if (to && pane.container.contains(to)) {
        return;
      }
      this.hideZone(pane);
    });
    pane.container.addEventListener("drop", (e: DragEvent) => {
      if (!isTabDrag(e)) {
        return;
      }
      e.preventDefault();
      this.hideZone(pane);
      const raw = e.dataTransfer?.getData(TAB_DND_MIME);
      const tab = raw ? Number.parseInt(raw, 10) : Number.NaN;
      // Validate the dropped tab against the instance's LIVE tab count before mutating
      // the layout: a payload that went stale mid-drag (the tab was closed) or is
      // otherwise out of range must not bind a pane to a nonexistent tab (same
      // stale-index discipline as the tab-op fixes in #1698/#1710). An invalid drop is
      // a no-op — the layout is left exactly as it was.
      if (Number.isNaN(tab) || tab < 0 || tab >= this.tabCount || !this.tree) {
        return;
      }
      const zone = this.zoneAt(pane.container, e.clientX, e.clientY);
      this.tree = zone === "center" ? replaceTab(this.tree, pane.leafId, tab) : splitLeaf(this.tree, pane.leafId, zone, tab);
      // Focus the pane now showing the dropped tab (VS Code focuses the new split).
      const landed = leaves(this.tree).find((l) => l.tab === tab);
      if (landed) {
        this.focusedId = landed.id;
      }
      this.commit();
      this.focus();
    });
  }

  /** The drop zone for a pointer position over a pane: an edge (outer band) or the
   *  center. */
  private zoneAt(container: HTMLElement, x: number, y: number): Edge {
    const r = container.getBoundingClientRect();
    if (r.width === 0 || r.height === 0) {
      return "center";
    }
    const fx = (x - r.left) / r.width;
    const fy = (y - r.top) / r.height;
    const d = { left: fx, right: 1 - fx, top: fy, bottom: 1 - fy };
    const min = Math.min(d.left, d.right, d.top, d.bottom);
    if (min > EDGE_BAND) {
      return "center";
    }
    if (min === d.left) {
      return "left";
    }
    if (min === d.right) {
      return "right";
    }
    if (min === d.top) {
      return "top";
    }
    return "bottom";
  }

  private showZone(pane: Pane, zone: Edge): void {
    pane.overlay.className = `af-drop-overlay af-drop-show af-drop-${zone}`;
  }

  private hideZone(pane: Pane): void {
    pane.overlay.className = "af-drop-overlay";
  }

  // --- internal: focus + status ---------------------------------------------

  private focusPane(leafId: string): void {
    if (this.focusedId === leafId) {
      return;
    }
    this.focusedId = leafId;
    this.applyFocusClass();
    // The focused pane's status becomes the header status.
    const pane = this.panes.get(leafId);
    if (pane) {
      this.cb.onStatus(pane.status);
    }
    this.report();
  }

  private applyFocusClass(): void {
    for (const [id, pane] of this.panes) {
      pane.container.classList.toggle("af-pane-focused", id === this.focusedId);
    }
  }

  private onPaneStatus(leafId: string, status: TerminalStatus): void {
    const pane = this.panes.get(leafId);
    if (pane) {
      pane.status = status;
    }
    if (leafId === this.focusedId) {
      this.cb.onStatus(status);
    }
  }

  private onPaneFocus(leafId: string, focused: boolean): void {
    if (focused) {
      if (this.blurTimer !== null) {
        window.clearTimeout(this.blurTimer);
        this.blurTimer = null;
      }
      // A click straight into a pane makes it the focused one.
      if (this.focusedId !== leafId) {
        this.focusPane(leafId);
      }
      this.cb.onFocusChange(true);
      return;
    }
    // A blur: only report "focus left the terminal" once we know no OTHER pane
    // grabbed it (the A→B handoff blurs A before focusing B).
    if (this.blurTimer !== null) {
      window.clearTimeout(this.blurTimer);
    }
    this.blurTimer = window.setTimeout(() => {
      this.blurTimer = null;
      const active = document.activeElement;
      const stillInPane = active ? [...this.panes.values()].some((p) => p.host.contains(active)) : false;
      if (!stillInPane) {
        this.cb.onFocusChange(false);
      }
    }, 0);
  }

  /** Fires onLayout when the focused tab, the shown-tab set, or the pane count
   *  changed — never on a no-op, so writing the store from it can't loop. */
  private report(): void {
    const shownTabs = this.tree ? leaves(this.tree).map((l) => l.tab) : [];
    const focusedTab = this.focusedId ? (findLeaf(this.tree ?? { kind: "leaf", id: "", tab: 0 }, this.focusedId)?.tab ?? 0) : 0;
    const paneCount = shownTabs.length;
    const shownKey = shownTabs.join(",");
    if (focusedTab === this.lastFocusedTab && shownKey === this.lastShown && paneCount === this.lastPaneCount) {
      return;
    }
    this.lastFocusedTab = focusedTab;
    this.lastShown = shownKey;
    this.lastPaneCount = paneCount;
    this.cb.onLayout({ focusedTab, shownTabs, paneCount });
  }
}
