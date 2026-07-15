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
  type DragPayload,
  type Edge,
  findLeaf,
  type LayoutNode,
  type LeafNode,
  leafCount,
  leaves,
  remapByIdentity,
  replaceTab,
  resolveDragTab,
  setRatio,
  singleLeaf,
  type SplitNode,
  splitLeaf,
  TAB_DND_MIME,
  tabsRebound,
  validate,
} from "./layout.js";
import { isLoopbackWebUrl, paneAddressUsesOrdinal, webProxyPath } from "./tabaddr.js";
import { AttachTerminal, type TerminalStatus } from "./terminal.js";
import { currentXtermTheme } from "./theme.js";
import { TabKind } from "./types.js";

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
  // The tab IDENTITY this pane's terminal/iframe was actually built against. The
  // ordinal alone is not enough to tell whether a pane is still showing the tab it
  // was built for: a close+create elsewhere can swap a DIFFERENT tab into the same
  // index, leaving the pane attached to a dead tab_id (#1779). Reconcile compares
  // this against the current identity at pane.tab and rebuilds on a mismatch.
  identity: string;
  status: TerminalStatus;
  // A web/iframe pane (TabKind.Web) has no AttachTerminal: it mounts an iframe in
  // `host` instead. webUrl is the target currently mounted (so reconcile can tell
  // a same-tab no-op from a target change), and webDispose tears down the iframe's
  // listeners/timers. Both null for a terminal pane.
  webUrl: string | null;
  webDispose: (() => void) | null;
}

function el(tag: string, cls: string): HTMLElement {
  const node = document.createElement(tag);
  node.className = cls;
  return node;
}

/** The delay before an unresponsive DIRECT external frame reveals its fallback.
 *  Overridable via window.__afWebtabFallbackMs for deterministic tests. */
function webFallbackMs(): number {
  const override = (globalThis as { __afWebtabFallbackMs?: number }).__afWebtabFallbackMs;
  return typeof override === "number" ? override : 2500;
}

export class SplitView {
  // Retained layout per session id (in-memory; a nice-to-have to persist across
  // reload is out of scope for v1). Keyed by the stable session id.
  private readonly trees = new Map<string, LayoutNode>();
  private readonly panes = new Map<string, Pane>();

  private sessionId: string | null = null;
  private token: string | null = null;
  private tabCount = 1;
  // The instance's ordered tab identities (ui.tabIdentity), kept current on every
  // store update. A drop compares its drag-time snapshot against this to detect a
  // mid-drag tab-set change (concurrent close/create/reorder) and cancel.
  private tabIds: string[] = [];
  // The REAL daemon tab ids ("" where a tab has none), parallel to tabIds. Kept
  // apart from the identity list because only these may cross the wire as a
  // ?tab_id= or be trusted as a collision-proof identity (#1779) — see ui.tabRealId.
  private tabRealIds: string[] = [];
  // Per-tab-index iframe target for web tabs (TabKind.Web); undefined for a
  // terminal tab. Parallel to the tab list, refreshed on every setSession, so
  // reconcile can mount an iframe for a web leaf without extra plumbing.
  private tabTargets: (string | undefined)[] = [];
  // The kind of each tab, parallel to tabIds — kept because the tab identity is now
  // the opaque stable id (#1738), which no longer encodes the kind the way the old
  // "kind:name" identity did. webTargetAt reads it to tell a web/iframe tab from a
  // terminal one.
  private tabKinds: number[] = [];
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
  setSession(
    sessionId: string | null,
    token: string | null,
    tabIds: string[],
    initialTab: number,
    tabTargets: (string | undefined)[] = [],
    tabKinds: number[] = [],
    tabRealIds: string[] = [],
  ): void {
    this.token = token;
    // Snapshot what the panes are currently bound to BEFORE overwriting it, so the
    // same-session branch can tell an identity change from a no-op (#1779).
    const prevIds = this.tabIds;
    const prevKinds = this.tabKinds;
    const prevTargets = this.tabTargets;
    this.tabIds = tabIds;
    this.tabRealIds = tabRealIds;
    this.tabTargets = tabTargets;
    this.tabKinds = tabKinds;
    const tabCount = tabIds.length > 0 ? tabIds.length : 1;
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
      // Move each leaf to wherever ITS tab now sits BEFORE anything reads the tree
      // (#1779). A leaf holds an ordinal, but the pane holds a TAB; once the list
      // shifts, reconciling from the stale ordinal would rebind the pane to whatever
      // tab took that slot — the misroute this whole change exists to close. A pane
      // whose tab merely MOVED then finds its identity already matching and is left
      // streaming untouched; only a genuinely replaced tab rebuilds.
      // Move each leaf to wherever ITS tab now sits BEFORE anything reads the tree
      // (#1779). A leaf holds an ordinal, but the pane holds a TAB; once the list
      // shifts, reconciling from the stale ordinal would rebind the pane to whatever
      // tab took that slot — the misroute this whole change exists to close. A pane
      // whose tab merely MOVED then finds its identity already matching and is left
      // streaming untouched; only a genuinely replaced tab rebuilds.
      const settled = remapByIdentity(this.tree ?? singleLeaf(initialTab), prevIds, tabIds);
      this.tree = validate(settled, tabCount);
      if (this.trees.get(sessionId) !== this.tree) {
        this.trees.set(sessionId, this.tree);
      }
      // Reconcile on a changed tab IDENTITY, not just a changed tree (#1779) — see
      // tabsRebound. reconcile() is a no-op per pane whose identity still matches,
      // so an unrelated snapshot still costs nothing.
      const rebound = tabsRebound(prevIds, prevKinds, prevTargets, tabIds, tabKinds, tabTargets);
      if (before !== this.tree || rebound) {
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

  /** Re-applies the active xterm theme to every live pane (a theme toggle). New
   *  panes already pick it up via terminal.ts's currentXtermTheme(); this repaints
   *  the ones already open, including split panes. */
  applyTheme(): void {
    const theme = currentXtermTheme();
    for (const pane of this.panes.values()) {
      pane.term?.setTheme(theme);
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
      pane.webDispose?.();
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
        pane.webDispose?.();
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
      const webTarget = this.webTargetAt(leaf.tab);
      // The identity now living at this leaf's ordinal. remapByIdentity has already
      // moved the leaf to follow its own tab, so a mismatch here means a genuinely
      // DIFFERENT tab occupies this pane's slot — not merely that ordinals shifted.
      const identity = this.tabIds[leaf.tab] ?? "";
      const realId = this.tabRealIds[leaf.tab] ?? "";
      // Rebuild when what the pane ADDRESSES changes — never merely because its tab's
      // ordinal moved (#1779). A different tab in the slot always rebuilds; a shifted
      // ordinal rebuilds only when the pane's address actually embeds that ordinal
      // (see paneAddressUsesOrdinal), which is exactly when the old address would now
      // point at another tab.
      const moved = pane.tab !== leaf.tab;
      const staleAddress = pane.identity !== identity || (moved && paneAddressUsesOrdinal(webTarget, realId));
      if (webTarget !== null) {
        // A web/iframe tab: mount an iframe instead of an xterm. Rebuilding reloads
        // the frame and drops the dev server's in-page state, so it happens only on a
        // real change: a different tab here, a changed target, or a moved PROXIED tab
        // (whose src is /v1/webtab/{session}/{ordinal}/ and would otherwise proxy the
        // tab that took its old index).
        if (pane.term || pane.webUrl !== webTarget || staleAddress) {
          pane.term?.dispose();
          pane.term = null;
          pane.webDispose?.();
          pane.host.replaceChildren();
          pane.tab = leaf.tab;
          pane.identity = identity;
          this.mountWebPane(pane, webTarget);
          pane.status = "open";
          this.onPaneStatus(leaf.id, "open");
        } else if (moved) {
          // The same tab, merely at a new ordinal, addressed by a URL that does not
          // encode one: follow it without touching the live frame.
          pane.tab = leaf.tab;
        }
      } else if (!pane.term || staleAddress) {
        pane.term?.dispose();
        pane.webDispose?.();
        pane.webUrl = null;
        pane.host.replaceChildren();
        pane.tab = leaf.tab;
        pane.identity = identity;
        pane.status = "connecting";
        // Address the stream by the tab's REAL daemon id (#1738) at this ordinal, so
        // a reorder/close resolves to the right PTY server-side. It must come from
        // tabRealIds, NOT tabIds: the identity list carries a synthesized `kind:name`
        // for an id-less tab, and sending that as ?tab_id= is an unknown id the
        // daemon 404s — breaking a legacy tab that attaches fine by ordinal (#1779).
        // "" is the honest "no id", which makes AttachTerminal fall back to ?tab=.
        pane.term = new AttachTerminal(pane.host, this.sessionId, this.token, realId, leaf.tab, {
          onStatus: (s) => this.onPaneStatus(leaf.id, s),
          onFocusChange: (f) => this.onPaneFocus(leaf.id, f),
        });
      } else if (moved) {
        // The same tab, merely at a new ordinal, streamed by ?tab_id=: the terminal's
        // captured ordinal is inert (terminal.ts sends tab_id OR tab, never both), so
        // follow the tab without tearing down a live stream and its scrollback.
        pane.tab = leaf.tab;
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

    const pane: Pane = {
      leafId: leaf.id,
      container,
      host: paneHost,
      label,
      overlay,
      term: null,
      tab: -1,
      // No tab bound yet; tab:-1 already forces the first reconcile to build one.
      identity: "",
      status: "connecting",
      webUrl: null,
      webDispose: null,
    };
    this.wireDrop(pane);
    return pane;
  }

  /** The iframe target for the tab at `idx`, or null when it is not a web tab.
   *  Confirms the kind from the parallel tabKinds list (the identity is now the
   *  opaque stable id, #1738) so a stale/mismatched tabTargets entry can never turn a
   *  terminal tab into an iframe. */
  private webTargetAt(idx: number): string | null {
    if (idx < 0 || idx >= this.tabIds.length) {
      return null;
    }
    if (this.tabKinds[idx] !== TabKind.Web) {
      return null;
    }
    return this.tabTargets[idx] ?? "";
  }

  /** Mounts an iframe for a web tab into pane.host and records its teardown on the
   *  pane. A loopback target is loaded through the same-origin daemon proxy
   *  (/v1/webtab/...), which makes a localhost dev-server preview work even for a
   *  REMOTE viewer and sidesteps X-Frame-Options; an external URL is iframed
   *  directly (best-effort). A reload control and an "open in new tab" affordance
   *  are always present; for a direct external frame a load-timeout reveals a
   *  fallback when embedding is blocked. */
  private mountWebPane(pane: Pane, target: string): void {
    pane.webUrl = target;
    const sessionId = this.sessionId ?? "";
    const proxied = target !== "" && isLoopbackWebUrl(target);
    const src = proxied ? webProxyPath(sessionId, pane.tab, this.token) : target;
    // The "open externally" href: for a proxied local preview, the same-origin
    // proxy path (works for the remote viewer); for an external tab, the site URL.
    const openHref = proxied ? webProxyPath(sessionId, pane.tab, this.token) : target;

    const wrap = el("div", "af-webpane");

    const bar = el("div", "af-webpane-bar");
    const reload = document.createElement("button");
    reload.type = "button";
    reload.className = "af-webpane-reload";
    reload.title = "Reload";
    reload.setAttribute("aria-label", "Reload web tab");
    reload.textContent = "↻"; // ↻
    const urlText = el("span", "af-webpane-url");
    urlText.textContent = target || "(no URL)";
    urlText.title = target;
    const open = document.createElement("a");
    open.className = "af-webpane-open";
    open.href = openHref;
    open.target = "_blank";
    open.rel = "noopener noreferrer";
    open.textContent = "open ↗"; // ↗
    bar.append(reload, urlText, open);

    const frame = document.createElement("iframe");
    frame.className = "af-webframe";
    // No allow-same-origin: the frame runs with an opaque origin, so a proxied
    // (same-origin) dev server can't reach the parent SPA or read its bearer
    // token, while scripts/forms still run for a functional preview.
    frame.setAttribute("sandbox", "allow-scripts allow-forms allow-popups allow-modals");
    frame.setAttribute("referrerpolicy", "no-referrer");
    if (src !== "") {
      frame.src = src;
    }

    const fallback = el("div", "af-webpane-fallback");
    fallback.hidden = true;
    const fbMsg = el("div", "af-webpane-fallback-msg");
    fbMsg.textContent = "This site can't be embedded (it blocks framing).";
    const fbLink = document.createElement("a");
    fbLink.className = "af-webpane-fallback-link";
    fbLink.href = openHref;
    fbLink.target = "_blank";
    fbLink.rel = "noopener noreferrer";
    fbLink.textContent = "Open in a new tab ↗";
    fallback.append(fbMsg, fbLink);

    wrap.append(bar, frame, fallback);
    pane.host.replaceChildren(wrap);

    // A web tab with no target (a malformed request, or an older persisted record)
    // renders a clean fallback rather than a blank pane — there is nothing to frame
    // or open.
    if (target.trim() === "") {
      fbMsg.textContent = "This web tab has no URL.";
      fbLink.hidden = true;
      open.hidden = true;
      fallback.hidden = false;
      frame.hidden = true;
      pane.webDispose = null;
      return;
    }

    reload.addEventListener("click", (e) => {
      e.stopPropagation();
      if (src === "") {
        return;
      }
      fallback.hidden = true;
      frame.hidden = false;
      // Reassign src to force a reload (contentWindow.reload throws cross-origin).
      // Clear it first so re-setting the same URL still triggers a navigation.
      frame.src = "";
      frame.src = src;
    });

    let settled = false;
    const onLoad = (): void => {
      settled = true;
      fallback.hidden = true;
      frame.hidden = false;
    };
    frame.addEventListener("load", onLoad);

    // Only a DIRECT external frame can be blocked by X-Frame-Options; a same-origin
    // proxied preview always loads. Arm a load-timeout that reveals the fallback if
    // no load event arrives (refused / blocked). Best-effort: some blocked frames
    // still fire load, so the always-present "open" link is the guaranteed escape.
    let timer: ReturnType<typeof setTimeout> | null = null;
    if (!proxied && src !== "") {
      timer = setTimeout(() => {
        if (!settled) {
          fallback.hidden = false;
          frame.hidden = true;
        }
      }, webFallbackMs());
    }

    pane.webDispose = (): void => {
      if (timer !== null) {
        clearTimeout(timer);
      }
      frame.removeEventListener("load", onLoad);
    };
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

  /** Parses a drag payload from the dataTransfer, or null if it is absent/malformed
   *  (a foreign drag, or a corrupt payload → the drop is a no-op). */
  private parseDrag(raw: string | undefined): DragPayload | null {
    if (!raw) {
      return null;
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(raw);
    } catch {
      return null;
    }
    if (
      typeof parsed === "object" &&
      parsed !== null &&
      typeof (parsed as DragPayload).index === "number" &&
      Array.isArray((parsed as DragPayload).tabs)
    ) {
      const p = parsed as DragPayload;
      return { id: typeof p.id === "string" ? p.id : undefined, index: p.index, tabs: p.tabs };
    }
    return null;
  }

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
      const drag = this.parseDrag(e.dataTransfer?.getData(TAB_DND_MIME));
      if (!drag || !this.tree) {
        return;
      }
      // Resolve the dragged tab to the ordinal it should bind — by its STABLE id when
      // it has one, else the guarded legacy index. See resolveDragTab; null cancels.
      const tab = resolveDragTab(drag, this.tabRealIds, this.tabIds, this.tabCount);
      if (tab === null) {
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
