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
//     the center replaces the pane's tab. Drop zones are shown on dragover. Dropping a
//     pane's OWN tab on its edge is the one exception: the dragged tab stays put and
//     the new half opens a DIFFERENT tab (companionTab), because one tab cannot fill
//     both halves — see the drop handler (#1901).
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
  companionTab,
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
  sameLayout,
  sameTabs,
  setRatio,
  singleLeaf,
  type SplitNode,
  splitLeaf,
  TAB_DND_MIME,
  tabsRebound,
  validate,
} from "./layout.js";
import { probeWebTab } from "./api.js";
import { icon } from "./icon.js";
// isLoopbackWebUrl is deliberately NOT imported any more: #1817 folded that test into
// iframeIsProxied, which also answers it for a vscode tab (always proxied, and with no
// target to classify). Calling it directly here would re-fork the question.
import {
  cacheBustedWebSrc,
  iframeIdentity,
  iframeIsProxied,
  type IframeSpec,
  nextReloadNonce,
  paneAddressUsesOrdinal,
  webProxyPath,
} from "./tabaddr.js";
import { tabIcon, tabLabel } from "./tablabel.js";
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
  /** The decorative kind icon beside the label (#1813) — the same pair the tab bar
   *  draws, from the same two functions (tablabel.ts). */
  glyph: HTMLElement;
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
  // Whether the mounted web pane is the ARCHIVED placeholder rather than a live
  // frame (#1809 follow-up). reconcile compares it against the session's current
  // archived state so an archive/restore that leaves the tab list untouched still
  // swaps the pane — without it, neither the target nor the tab index changes and
  // the rebuild guard would keep a live iframe on an archived session.
  webArchived: boolean;
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

/** A retained layout, plus the tab identities its leaf ordinals were bound against.
 *
 *  The two are inseparable, which is why they are one record. A leaf holds an
 *  ORDINAL, and an ordinal only names a tab RELATIVE to the identity list current
 *  when it was bound. Retaining the tree alone leaves a value that silently decays:
 *  once that session's roster shifts, every ordinal in it points somewhere new.
 *  The shown session was already remapped on each setSession (#1779), but a session
 *  the user isn't looking at had no such path — and #1815 made an out-of-band close
 *  reach the client live, so its retained panes could be rebound to a neighbouring
 *  tab on return (post-merge Codex finding). Keeping the ids with the tree makes
 *  "remap before you read it" checkable rather than remembered. */
interface RetainedLayout {
  tree: LayoutNode;
  ids: string[];
}

/** The host[:port] of a web-tab target ("localhost:3300"), for the dead-server
 *  message (#1813) — the part a developer acts on. Falls back to the raw target if
 *  it does not parse; the daemon normalizes every URL it stores, so that cannot
 *  reach here, but an error message is the wrong place to throw. */
function hostLabel(target: string): string {
  try {
    return new URL(target).host;
  } catch {
    return target;
  }
}

export class SplitView {
  // Retained layout per session id (in-memory; a nice-to-have to persist across
  // reload is out of scope for v1). Keyed by the stable session id.
  private readonly trees = new Map<string, RetainedLayout>();
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
  // Each tab's NAME, parallel to tabIds — what the pane headers render (with the
  // kind, through tablabel.ts). Before #1813 the panes had no access to this at all
  // and drew a positional "Tab N" instead: an ordinal that says nothing when several
  // panes are open, which is the only time a pane header is read.
  private tabNames: string[] = [];
  // Whether the shown session is archived (#1809 follow-up). An archived session is
  // inert: the daemon refuses to proxy its preserved web tab, so the pane renders an
  // archived placeholder instead of a frame that could only fail — or, worse, could
  // proxy a stale loopback port that now hosts something else.
  private archived = false;
  private tree: LayoutNode | null = null;
  private focusedId: string | null = null;

  // The tree the CURRENT split DOM was built from, so reconcile can tell a real
  // layout change from a resync that left the layout alone (#1815 scroll fix).
  //
  // Re-inserting a pane's container — even the very same element, into the very
  // same parent — detaches it, and the browser drops the scroll offset of every
  // scrollable descendant on detach. xterm keeps its own scroll position (ydisp)
  // but its .xterm-viewport silently rewinds to 0, and a viewport pinned at the
  // top emits no scroll event, so wheel-up goes dead until the next chunk of
  // output resyncs it — i.e. exactly while a quiet pane is being read.
  //
  // Compared with sameLayout, NOT by reference: a fresh root does not imply a
  // changed layout. setRatio rebuilds every SplitNode it walks, so persisting a
  // divider drag produces a new root for a layout already on screen, and a
  // reference check would rebuild on the next resync — the same rewind, one
  // gesture later. The stale nodes the live dividers still capture are harmless:
  // a divider resolves its split by ID when it persists, and the only state it
  // reads back (ratio) is what it wrote during that same drag.
  private builtTree: LayoutNode | null = null;

  // Counts explicit layout/focus mutations, for the stale-async guard — see
  // layoutGeneration(), which is the documented contract.
  private layoutGen = 0;

  // Debounces the "focus left every pane" report so a click that moves focus A→B
  // (blur A, then focus B) doesn't flap the nav mode through rail and back.
  private blurTimer: number | null = null;

  // Last values reported via onLayout, so a no-op reconcile never re-fires it (which
  // would re-enter the store→rerender→setSession loop).
  private lastFocusedTab = -1;
  private lastShown = "";
  private lastPaneCount = 0;

  // The tabs this session's focus has passed through, most-recent FIRST, as stable
  // daemon ids — what a self-split prefers to open in its new half (#1901), so the
  // pane you get back is the tab you were last on rather than an arbitrary neighbour.
  //
  // By id, never ordinal: the roster shifts underneath (#1779), so an ordinal
  // remembered across a close would name a different tab by the time it is read — the
  // misroute the stable id exists to prevent. A tab with no daemon id simply never
  // enters the list; the next-in-order fallback still covers it.
  private tabMru: string[] = [];
  // The stable id of the tab lastFocusedTab named WHEN IT WAS RECORDED. Resolving that
  // ordinal later would ask the current roster about a past position and get the wrong
  // tab, so the id is captured at the same moment as the ordinal.
  private lastFocusedTabId = "";

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
    archived = false,
    tabNames: string[] = [],
  ): void {
    this.token = token;
    // Snapshot what the panes are currently bound to BEFORE overwriting it, so the
    // same-session branch can tell an identity change from a no-op (#1779).
    const prevIds = this.tabIds;
    const prevKinds = this.tabKinds;
    const prevTargets = this.tabTargets;
    const prevNames = this.tabNames;
    this.tabIds = tabIds;
    this.tabRealIds = tabRealIds;
    this.tabTargets = tabTargets;
    this.tabKinds = tabKinds;
    this.tabNames = tabNames;
    // An archive/restore of the SHOWN session must re-render its web panes even when
    // the tab list is identical (#1809 follow-up) — archiving a session whose only
    // extra tab is a web tab leaves the count untouched, so the same-session path
    // below would otherwise see an unchanged tree and skip the swap.
    const archivedChanged = archived !== this.archived;
    this.archived = archived;
    const tabCount = tabIds.length > 0 ? tabIds.length : 1;
    if (sessionId === null || token === null) {
      this.teardown();
      this.sessionId = null;
      this.tree = null;
      // Deselect is a session boundary too: the history belongs to the session being
      // left, so it must not survive to the next selection (which may not go through
      // the different-session branch first). report() then reinitializes from the empty
      // tree — lastFocusedTabId back to "".
      this.forgetFocusHistory();
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
      const settled = remapByIdentity(this.tree ?? singleLeaf(initialTab), prevIds, tabIds);
      this.tree = validate(settled, tabCount);
      // Always re-retain: `ids` must track the roster even when the remap left the
      // tree identical, or the record would claim ordinals were bound against a list
      // that has since moved on — the decay the record exists to prevent.
      this.retain(sessionId, this.tree, tabIds);
      // Reconcile on a changed tab IDENTITY, not just a changed tree (#1779) — see
      // tabsRebound. reconcile() is a no-op per pane whose identity still matches,
      // so an unrelated snapshot still costs nothing. An archive/restore flip (#1809)
      // is a third trigger: it changes what a web pane may RENDER without touching
      // any identity, so neither the tree nor tabsRebound would catch it.
      const rebound = tabsRebound(prevIds, prevKinds, prevTargets, tabIds, tabKinds, tabTargets);
      // A RENAME (#1813) is the fourth trigger, and it is its own case for the same
      // reason the archive flip is: it changes what a pane RENDERS while touching
      // nothing any other check looks at. The tree is identical, no identity or
      // target moved, so both `before !== this.tree` and tabsRebound say "nothing to
      // do" — and the header would keep the old name until something unrelated
      // happened to force a reconcile. Reconciling here is cheap and rebuilds
      // nothing: every pane's identity still matches, so the pass falls through to
      // the label refresh at the end and stops.
      const renamed = !sameTabs(prevNames, tabNames);
      if (before !== this.tree || rebound || archivedChanged || renamed) {
        this.reconcile();
        this.report();
      }
      return;
    }
    // A different session: drop the old session's live terminals (its tree stays
    // retained) and build the new one's.
    this.teardown();
    // The focus history describes the session being LEFT: its ids name that session's
    // tabs, so carrying them over would have the new session's first self-split prefer
    // a tab that isn't its own (preferredTabs would resolve them against the wrong
    // roster). "Previously focused" is scoped to the session, so it starts empty here.
    this.forgetFocusHistory();
    this.sessionId = sessionId;
    this.tabCount = tabCount;
    // Remap the retained tree onto the CURRENT roster before validating it. Its
    // ordinals were bound against the tab list as it stood when the user last looked
    // at this session; anything that changed the roster since (an agent's tab-create,
    // another window's close — now delivered live by #1815) moved the tabs out from
    // under them. validate() alone only clamps a too-high ordinal, so a pane would
    // come back bound to whatever tab slid into its slot.
    this.tree = validate(this.retainedTree(sessionId, tabIds) ?? singleLeaf(initialTab), tabCount);
    this.retain(sessionId, this.tree, tabIds);
    // Focus the first pane of the newly shown session by default.
    this.focusedId = leaves(this.tree)[0]?.id ?? null;
    this.reconcile();
    this.report();
  }

  /** The tab `sessionId` will actually be shown on once selected: the focused pane's
   *  tab for the session already on screen, the retained layout's first pane for one
   *  shown before (setSession focuses exactly that leaf), and 0 for a session never
   *  shown — it gets a fresh single leaf.
   *
   *  Selection asks this instead of asserting 0. Trees are RETAINED across session
   *  switches, so "reset activeTab to 0 on select" states something about a pane that
   *  already disagrees — and report(), the only writer of activeTab, dedups on the
   *  focused tab, so a re-entry that settles on the SAME index never corrects it. The
   *  bar then highlights Agent over a pane showing tab N, and the next close computes
   *  its shift from the stale 0 and yanks the pane to Agent (#1855). Reading the
   *  settled tab keeps the store's claim and the pane's binding the same statement. */
  settledTab(sessionId: string, tabIds: string[]): number {
    if (sessionId === this.sessionId) {
      return this.tree && this.focusedId ? (findLeaf(this.tree, this.focusedId)?.tab ?? 0) : 0;
    }
    // Read the REMAPPED tree, exactly as setSession will: this answers "which tab will
    // that pane show once selected?", and the two must agree or the bar highlights one
    // tab while the pane binds another (#1855). Passing the same ids setSession gets is
    // what keeps the answers identical.
    const retained = this.retainedTree(sessionId, tabIds);
    return retained ? (leaves(retained)[0]?.tab ?? 0) : 0;
  }

  /** The retained tree for `sessionId`, remapped onto `tabIds` — the only supported
   *  way to read one. Returns null for a session never shown. */
  private retainedTree(sessionId: string, tabIds: string[]): LayoutNode | null {
    const rec = this.trees.get(sessionId);
    if (!rec) {
      return null;
    }
    return remapByIdentity(rec.tree, rec.ids, tabIds);
  }

  /** Retains `tree` for `sessionId` together with the identities it is bound against. */
  private retain(sessionId: string, tree: LayoutNode, ids: string[]): void {
    this.trees.set(sessionId, { tree, ids });
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

  /** Gives the keyboard to the focused pane's terminal (attach), returning whether
   *  a terminal actually took it. False means there was nothing to focus: a web or
   *  VS Code tab renders an iframe and carries no term (mountWebPane leaves
   *  pane.term null), as does an empty tree. Callers that put the app into
   *  "terminal" mode MUST honor a false return and fall back — see focusTerminal()
   *  in index.ts — or the app claims the keyboard is in a terminal that does not
   *  exist and swallows every rail key until Escape. */
  focus(): boolean {
    const pane = this.focusedId ? this.panes.get(this.focusedId) : null;
    if (!pane?.term) {
      return false;
    }
    pane.term.focus();
    return true;
  }

  /** Hands the keyboard to the focused pane, or takes it away from EVERY pane when
   *  that pane has no terminal to hand it to (a web/VS Code tab is an iframe).
   *
   *  The else-branch is load-bearing, and every internal "focus the pane we just moved
   *  to" site must go through here rather than calling focus() bare. Without it, focus()
   *  silently no-ops and the PREVIOUSLY focused pane's xterm keeps DOM focus: the header
   *  highlights the pane the user cycled TO while their keystrokes still reach the agent
   *  they cycled AWAY from — a silent wrong target, which is worse than the silent
   *  no-target focusTerminal() used to produce. blur() reports out through
   *  onFocusChange(false), which is how this class asks index.ts for rail mode: SplitView
   *  owns no store, so it cannot call focusRail() itself — the callback IS that path. */
  private refocus(): void {
    if (!this.focus()) {
      this.blur();
    }
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

  /** Refit every live terminal after app chrome changes the pane's usable surface.
   *  Web/VS Code leaves have no terminal and are intentionally inert. */
  refit(): void {
    for (const pane of this.panes.values()) {
      pane.term?.refit();
    }
  }

  /** Moves pane focus by `delta` (wrapping) and attaches the newly focused pane. */
  cyclePane(delta: 1 | -1): void {
    if (!this.tree) {
      return;
    }
    const ids = leaves(this.tree).map((l) => l.id);
    if (ids.length <= 1) {
      this.refocus();
      return;
    }
    const cur = this.focusedId ? ids.indexOf(this.focusedId) : -1;
    const next = ids[(cur + delta + ids.length) % ids.length];
    if (next) {
      this.focusPane(next);
      this.refocus();
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
    this.forgetFocusHistory();
  }

  /** How many EXPLICIT layout/focus mutations have been committed — a tab rebind
   *  (a 1-9 key or a tab-bar click), a drag-drop split, a pane close, or a change
   *  of WHICH PANE is focused (a click into another pane, Alt+j/k). Deliberately
   *  NOT bumped by setSession's roster reconcile, which only remaps each pane to
   *  follow its own tab and expresses no user intent.
   *
   *  That split is the point: an async caller which computes a tab index from a
   *  PRE-await snapshot (see index.ts closeSessionTab) captures this first and
   *  applies its result only if the value still matches. A slow close then can't
   *  clobber a tab the user selected while it was in flight — their newer intent
   *  wins — while the roster event that races the same close still passes the
   *  guard, because it bumps nothing.
   *
   *  Pane focus counts because the guarded write (setFocusedTab) targets the
   *  FOCUSED pane: an index computed against the pane that issued the close is
   *  meaningless once a different pane holds focus, and applying it there would
   *  rebind the pane the user just picked (post-merge Codex finding on #1815). */
  layoutGeneration(): number {
    return this.layoutGen;
  }

  // --- internal: mutation commit --------------------------------------------

  /** Persists the current tree for the session, re-renders, and reports the layout.
   *  Every explicit layout/focus mutation funnels through here, which is what makes
   *  it the one place to count them (see layoutGeneration). */
  private commit(): void {
    this.layoutGen++;
    if (this.sessionId && this.tree) {
      this.retain(this.sessionId, this.tree, this.tabIds);
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
    this.refocus();
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
    // The DOM is gone, so the next reconcile must build it whatever the tree says.
    this.builtTree = null;
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
    //
    // ONLY when the layout actually changed: re-inserting one that is already on
    // screen would detach every live pane and silently rewind its xterm viewport to
    // the top, killing wheel-scroll on a session.updated that touched nothing this
    // pane shows (a tab created in another window, #1812/#1815) — see builtTree.
    if (!sameLayout(this.tree, this.builtTree)) {
      const rootEl = this.buildNode(this.tree);
      rootEl.style.flex = "1 1 0";
      this.host.replaceChildren(rootEl);
      this.builtTree = this.tree;
    }

    const multi = desired.length > 1;
    this.host.classList.toggle("af-split-multi", multi);

    // (Re)create terminals for panes whose bound tab changed (or are brand new).
    for (const leaf of desired) {
      const pane = this.panes.get(leaf.id);
      if (!pane) {
        continue;
      }
      const spec = this.iframeSpecAt(leaf.tab);
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
      const staleAddress =
        pane.identity !== identity || (moved && paneAddressUsesOrdinal(spec ? spec.target : null, realId));
      if (spec !== null) {
        // A web/vscode tab: mount an iframe instead of an xterm. Rebuilding reloads
        // the frame and drops the dev server's in-page state — or a VS Code pane's
        // unsaved buffers — so it happens only on a real change: a different tab
        // here, a changed target, or a flip of the session's ARCHIVED state (#1809),
        // which swaps a live frame for the inert placeholder and back WITHOUT
        // changing the target, the ordinal, or the identity, so no other term here
        // would catch it. A merely-MOVED tab is not a real change any more: an
        // iframe pane's src encodes no ordinal (proxied → /v1/webtab/{session}/
        // {tabId}/…, #1810; external → the target URL), so it is followed rather
        // than rebuilt.
        if (
          pane.term ||
          pane.webUrl !== iframeIdentity(spec) ||
          staleAddress ||
          pane.webArchived !== this.archived
        ) {
          pane.term?.dispose();
          pane.term = null;
          pane.webDispose?.();
          pane.host.replaceChildren();
          pane.tab = leaf.tab;
          pane.identity = identity;
          this.mountWebPane(pane, spec, realId);
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
      // Publish WHAT this pane is bound to, alongside data-leaf's WHERE. The header
      // cannot answer it, before #1813 or after: an ordinal shifted with the roster
      // (#1779), and a name is a display string a rename can change out from under an
      // assertion — so the one question a split has to settle, are these two halves on
      // DIFFERENT tabs? (#1901), stays unobservable without this. "" for a legacy tab
      // with no daemon id: the honest answer, and the same one AttachTerminal falls
      // back on.
      pane.container.setAttribute("data-tab-id", realId);
      // The header names the TAB, not its position (#1813). Refreshed on EVERY
      // reconcile rather than only when a pane is created or rebound: a rename
      // changes this string and nothing else, so a label written once at build time
      // would keep showing the old name for the life of the pane.
      const named = { name: this.tabNames[leaf.tab] ?? "", kind: this.tabKinds[leaf.tab] ?? TabKind.Agent };
      pane.glyph.replaceChildren(icon(tabIcon(named.kind)));
      pane.label.textContent = tabLabel(named);
    }

    this.applyFocusClass();
  }

  private createPane(leaf: LeafNode): Pane {
    const container = el("div", "af-pane");
    container.setAttribute("data-leaf", leaf.id);
    const head = el("div", "af-pane-head");
    // The kind icon is a decorative sibling of the label, exactly as in the tab bar
    // (#1813): same two functions, same pair, so the two surfaces cannot drift.
    const glyph = el("span", "af-pane-glyph");
    glyph.setAttribute("aria-hidden", "true");
    const label = el("span", "af-pane-label");
    const closeBtn = document.createElement("button");
    closeBtn.type = "button";
    closeBtn.className = "af-pane-close";
    closeBtn.title = "Close pane";
    closeBtn.setAttribute("aria-label", "Close pane");
    closeBtn.append(icon("x"));
    closeBtn.addEventListener("click", (e) => {
      e.stopPropagation();
      this.closePane(leaf.id);
    });
    head.append(glyph, label, closeBtn);

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
      glyph,
      overlay,
      term: null,
      tab: -1,
      // No tab bound yet; tab:-1 already forces the first reconcile to build one.
      identity: "",
      status: "connecting",
      webUrl: null,
      webDispose: null,
      webArchived: false,
    };
    this.wireDrop(pane);
    return pane;
  }

  /** What the tab at `idx` should render as an iframe, or null when it is a
   *  terminal tab. See IframeSpec. Confirms the kind from the parallel tabKinds list (the identity
   *  is now the opaque stable id, #1738) so a stale/mismatched tabTargets entry can
   *  never turn a terminal tab into an iframe.
   *
   *  A vscode tab deliberately carries NO target: its editor is a daemon-managed
   *  per-session code-server on an ephemeral port, so the only address is the
   *  daemon proxy path, which is derived from the session + tab, not stored. */
  private iframeSpecAt(idx: number): IframeSpec | null {
    if (idx < 0 || idx >= this.tabIds.length) {
      return null;
    }
    const kind = this.tabKinds[idx];
    if (kind === TabKind.Web) {
      return { kind: TabKind.Web, target: this.tabTargets[idx] ?? "" };
    }
    if (kind === TabKind.VSCode) {
      return { kind: TabKind.VSCode, target: "" };
    }
    return null;
  }

  /** Mounts an iframe for a web tab into pane.host and records its teardown on the
   *  pane. A loopback target is loaded through the same-origin daemon proxy
   *  (/v1/webtab/...), which makes a localhost dev-server preview work even for a
   *  REMOTE viewer and sidesteps X-Frame-Options; an external URL is iframed
   *  directly (best-effort). A reload control and an "open in new tab" affordance
   *  are always present; for a direct external frame a load-timeout reveals a
   *  fallback when embedding is blocked. */
  private mountWebPane(pane: Pane, spec: IframeSpec, realId: string): void {
    const target = spec.target;
    const isVSCode = spec.kind === TabKind.VSCode;
    pane.webUrl = iframeIdentity(spec);
    pane.webArchived = this.archived;
    const sessionId = this.sessionId ?? "";
    // Proxied THROUGH the daemon, addressed by the tab's stable id (#1810). realId is
    // required: the proxy route has no ordinal form to fall back to, and every live
    // tab carries an id (the daemon backfills one on load), so an id-less tab is
    // unreachable in practice.
    //
    // A vscode tab is always proxied by kind — its code-server is loopback-only and
    // there is no target to classify — but it needs the id just the same, and unlike
    // a web tab it has NO direct-frame degradation: with no id there is no address at
    // all, which the unaddressable branch below renders honestly rather than as a
    // blank pane.
    const proxied = realId !== "" && iframeIsProxied(spec);
    // An archived session is inert (#1809 follow-up), so the frame is never pointed
    // at the target: the daemon refuses to proxy an archived session's tab, and for
    // a DIRECT external tab there is no daemon in the path to refuse — the frame
    // would load the live site out of a session the user has shelved. Blanking src
    // here (rather than only overlaying the placeholder) is what guarantees no
    // request is issued either way — including the spawn a vscode tab's first
    // request would otherwise trigger.
    const src = this.archived ? "" : proxied ? webProxyPath(sessionId, realId, target, this.token) : target;
    // The "open externally" href: for a proxied local preview or editor, the
    // same-origin proxy path (works for the remote viewer); for an external tab, the
    // site URL. Computed from the target rather than reused from `src`, which an
    // archived session deliberately blanks — the two only coincide while the session
    // is live. The archived branch below withdraws this link outright, so it is never
    // the thing that reaches an inert session's target.
    const openHref = proxied ? webProxyPath(sessionId, realId, target, this.token) : target;

    const wrap = el("div", "af-webpane");

    const bar = el("div", "af-webpane-bar");
    const reload = document.createElement("button");
    reload.type = "button";
    // Reload and open are the same KIND of control — a quiet secondary action on a
    // thin bar — so they share the app's ghost idiom (.af-ghost, as used by the
    // modal/terminal/task actions) and differ only in their glyph. The semantic
    // .af-webpane-* class stays as the identity hook for CSS and the driver test.
    reload.className = "af-ghost af-webpane-reload";
    reload.title = "Reload";
    reload.setAttribute("aria-label", isVSCode ? "Reload VS Code" : "Reload web tab");
    reload.append(icon("reload"));
    const urlText = el("span", "af-webpane-url");
    // A vscode tab has no URL worth showing (an ephemeral loopback port the user
    // can neither predict nor use); name what the pane IS instead.
    urlText.textContent = isVSCode ? "VS Code — session worktree" : target || "(no URL)";
    urlText.title = urlText.textContent;
    const open = document.createElement("a");
    open.className = "af-ghost af-webpane-open";
    open.href = openHref;
    open.target = "_blank";
    open.rel = "noopener noreferrer";
    open.append("Open", icon("external-link"));
    bar.append(reload, urlText, open);

    const frame = document.createElement("iframe");
    frame.className = "af-webframe";
    // The sandbox differs by kind, because WHAT is framed differs:
    //
    // A web tab frames an ARBITRARY target (an agent-supplied dev server, or any
    // external site), so it gets no allow-same-origin: it runs with an opaque
    // origin and cannot reach the parent SPA or read its bearer token, while
    // scripts/forms still run for a functional preview.
    //
    // A vscode tab frames a process the DAEMON ITSELF spawned — a code-server on
    // this session's worktree — and VS Code cannot run under an opaque origin
    // (localStorage, workers, and its service worker all require a real one). Be
    // precise about what this costs: allow-scripts + allow-same-origin on
    // same-origin content is effectively NO sandbox — the frame can reach the
    // parent. That is acceptable here only because the content is not
    // user-supplied and is already strictly more privileged than the SPA:
    // code-server runs as the user with a terminal and arbitrary code execution,
    // so anything able to serve through this frame already owns the machine. The
    // boundary is that the daemon controls what is served here, not the sandbox.
    frame.setAttribute(
      "sandbox",
      isVSCode
        ? "allow-scripts allow-forms allow-popups allow-modals allow-same-origin allow-downloads"
        : "allow-scripts allow-forms allow-popups allow-modals",
    );
    // The editor needs the clipboard for copy/paste to behave like a real VS Code;
    // an arbitrary framed site does not get it.
    if (isVSCode) {
      frame.setAttribute("allow", "clipboard-read; clipboard-write");
    }
    frame.setAttribute("referrerpolicy", "no-referrer");
    // The src is assigned by load() below, never here: a PROXIED frame must not be
    // pointed at its target until the probe says the target is alive (#1813).

    const fallback = el("div", "af-webpane-fallback");
    fallback.hidden = true;
    const fbMsg = el("div", "af-webpane-fallback-msg");
    fbMsg.textContent = "This site can't be embedded (it blocks framing).";
    const fbLink = document.createElement("a");
    fbLink.className = "af-webpane-fallback-link";
    fbLink.href = openHref;
    fbLink.target = "_blank";
    fbLink.rel = "noopener noreferrer";
    fbLink.append("Open in a new tab", icon("external-link"));
    // The retry affordance, shown ONLY in the dead-server state (#1813) — the other
    // fallbacks (archived, no-URL, blocked embedding) have nothing to retry: they
    // resolve by restoring a session, fixing a record, or leaving the frame.
    const fbRetry = document.createElement("button");
    fbRetry.type = "button";
    fbRetry.className = "af-ghost af-webpane-fallback-retry";
    fbRetry.textContent = "Retry";
    fbRetry.hidden = true;
    fallback.append(fbMsg, fbLink, fbRetry);

    wrap.append(bar, frame, fallback);
    pane.host.replaceChildren(wrap);

    // An ARCHIVED session's web tab is preserved but inert (#1809 follow-up): the
    // URL survives archive so a restore can render it again, but until then there is
    // nothing legitimate to show. The target is a bare loopback address from
    // whenever the tab was created — its dev server is long gone and the port may
    // now host something else — so the pane says so instead of framing it, and the
    // "open ↗" escape hatch is withdrawn (it would only hit the refusing proxy, or
    // reach a port that is no longer the preview). Checked before the no-URL case:
    // "restore it" is the actionable message for either.
    if (this.archived) {
      fallback.classList.add("af-webpane-archived");
      // Name what the pane actually is: this branch fences a VS Code pane too, and
      // "this web tab" would be wrong for one.
      fbMsg.textContent = isVSCode
        ? "This session is archived. Restore it to open VS Code."
        : "This session is archived. Restore it to load this web tab.";
      fbLink.hidden = true;
      open.hidden = true;
      // ↻ is withdrawn wherever it cannot act, on the same principle as the links
      // above: a control that is present and does nothing teaches the user the app is
      // broken and gives them nothing to act on. Here there is deliberately nothing to
      // reload — src is blanked above and the frame is never pointed anywhere — and
      // this branch returns before the click listener is even attached, so the button
      // is inert in the most literal sense. This hides rather than disables because
      // the message right above already carries the real next step ("Restore it"); a
      // disabled button could only repeat it somewhere less discoverable.
      reload.hidden = true;
      fallback.hidden = false;
      frame.hidden = true;
      pane.webDispose = null;
      return;
    }

    // Nothing addressable to frame — render a clean fallback rather than a blank
    // pane. The two kinds fail differently: a WEB tab has no target (a malformed
    // request, or an older persisted record), while a VSCODE tab has no target BY
    // DESIGN and instead needs the tab id its proxy path is keyed by (#1810). The id
    // is backfilled when the daemon loads the record, so a reload is the real fix.
    const unaddressable = isVSCode ? realId === "" : target.trim() === "";
    if (unaddressable) {
      fbMsg.textContent = isVSCode
        ? "This VS Code tab can't be addressed yet. Reload the page."
        : "This web tab has no URL.";
      fbLink.hidden = true;
      open.hidden = true;
      // Nothing addressable ⇒ nothing to reload, and (as in the archived branch) no
      // listener is attached past this return anyway. Note the VS Code message asks
      // for a reload of the PAGE, not of this pane: ↻ re-runs load() with the same
      // captured — still empty — id, so it could never be the fix it appears to
      // offer. The page reload is what re-fetches a snapshot with the id backfilled.
      reload.hidden = true;
      fallback.hidden = false;
      frame.hidden = true;
      pane.webDispose = null;
      return;
    }

    // --- load + health -------------------------------------------------------
    //
    // A PROXIED tab is health-checked from the PARENT before the frame is pointed at
    // it (#1813). The frame's origin is opaque (no allow-same-origin, above), so this
    // document can read neither its content nor its status, and `load` fires even for
    // a 502 — which is exactly how a dead dev server came to render the daemon's raw
    // JSON error envelope as the "preview". Probing first means the frame is never
    // pointed at a target already known to be down, so there is no JSON to paint
    // over: the failure state is structural. The probe is one same-origin request to
    // a loopback dev server, so the healthy path pays an RTT of single-digit ms.
    //
    // A DIRECT external tab cannot be probed — it is cross-origin, and a no-cors
    // fetch would hide the status anyway — so it keeps the load-timeout it has always
    // had, and only that.
    // Both are WEB-tab concerns and deliberately skip a VS CODE pane, which #1817
    // already owns end to end — I looked before wiring, and it needs neither:
    //
    //   - The dead-server PROBE exists for a race a vscode tab does not have. A web
    //     tab points at a dev server someone else started, so "the tab exists before
    //     the port answers" is its normal first state. A vscode tab's editor is spawned
    //     BY the daemon, on demand, and ensureVSCodeServer blocks until it is up.
    //   - #1817's failure states are already DESIGNED and already render in the pane:
    //     the daemon serves an HTML notice (install hint / "VS Code is still starting…"
    //     with a self-refresh / exited-while-starting) at 503. 503 is not in the probe's
    //     dead set {502,504}, so those pass through untouched even where the probe does
    //     run — but probing first would still delay them behind a round trip for no gain.
    //   - showDead() names a host taken from the target, and a vscode tab has NO target
    //     by design (its editor is an ephemeral loopback port resolved at proxy time).
    //     Pointing this at one renders "answering at  yet" — an empty host.
    //
    // The residue is a vscode socket that dies AFTER a successful spawn: the proxy's
    // ErrorHandler is kind-agnostic, so that 502s into the frame as a raw envelope —
    // #1817's pre-existing behavior, not something this change introduces or should
    // silently redesign mid-rebase. Reported as a follow-up.
    const webProxied = proxied && !isVSCode;
    const probePath = webProxied ? webProxyPath(sessionId, realId, target, null) : "";
    let disposed = false;
    // Supersedes an in-flight probe: a slow answer from an earlier attempt must never
    // overwrite what a later Retry concluded.
    let probeSeq = 0;

    const showDead = (): void => {
      fallback.classList.add("af-webpane-dead");
      fbMsg.textContent = `No dev server is answering at ${hostLabel(target)} yet.`;
      // BOTH open links are withdrawn in the dead state — they point at the same
      // proxied URL the probe just found 502ing, so following either would open a new
      // tab showing the daemon's raw 502 JSON, the exact thing this fallback replaces
      // (Codex P3). The bar's `open` is the one the fallback's fbLink already hides for
      // its own reason; the two must move together here. Retry stands in for both.
      fbLink.hidden = true;
      open.hidden = true;
      fbRetry.hidden = false;
      fallback.hidden = false;
      frame.hidden = true;
      // Drop the src so a frame from an earlier healthy load isn't left mounted (and
      // still fetching) behind the fallback after its server dies.
      frame.removeAttribute("src");
    };

    const showFrame = (): void => {
      fallback.classList.remove("af-webpane-dead");
      fallback.hidden = true;
      fbRetry.hidden = true;
      // Restore the bar's open link showDead withdrew: a live frame's proxied URL is
      // now worth opening again.
      open.hidden = false;
      frame.hidden = false;
    };

    // The external-tab surface (item A / Sachin): a direct external target is never
    // shown inline. We cannot tell a working embed from the browser's own
    // X-Frame-Options refusal — both fire the iframe `load` event and the frame's
    // opaque origin is unreadable — and the refusal page is the one thing Sachin ruled
    // out ("NEVER the browser's raw refusal"). So the designed fallback IS the surface:
    // a calm one-liner plus a WORKING open link, shown persistently, with the frame
    // never given a src. The bar's `open` link stays live too — here it is the GOOD
    // escape, not a route to a 502 — so this deliberately does not touch it.
    const showExternalFallback = (): void => {
      fallback.classList.add("af-webpane-external");
      fbMsg.textContent = "This site may block embedding.";
      fbLink.hidden = false;
      fbLink.replaceChildren("Open it in a new tab", icon("external-link"));
      fbRetry.hidden = true;
      // ↻ is withdrawn here for the same reason fbRetry is, and it is the same
      // judgement: this state is not a failure to recover from but the DESIGNED
      // surface, and the frame is deliberately never navigated — so "reload" names
      // nothing. Pressing it re-ran load(), which landed right back on this card:
      // no fetch, no navigation, no visible change, no explanation. The escape that
      // does work is the open link (this card's, and the bar's `open` — which this
      // branch deliberately leaves live, unlike showDead).
      //
      // Never restored: `external` is fixed for the life of this render (a target is
      // proxied or it is not), so this state never transitions to a live frame. The
      // pane is rebuilt from scratch if the tab's address ever changes.
      reload.hidden = true;
      fallback.hidden = false;
      frame.hidden = true;
    };

    // A direct external target (non-loopback web) is never proxied, so it is the one
    // case where the browser's own X-Frame-Options refusal could reach the pane.
    // Item A routes it to the persistent fallback instead of the frame.
    const external = !proxied;

    const load = async (bust = false): Promise<void> => {
      const seq = ++probeSeq;
      if (webProxied) {
        const health = await probeWebTab(probePath, this.token ?? "", webFallbackMs());
        if (disposed || seq !== probeSeq) {
          return; // torn down, or a newer Retry owns this pane now
        }
        if (health === "dead") {
          showDead();
          return;
        }
      }
      if (external) {
        // Never point an external frame at its target (item A): a browser refusal and
        // a working embed are indistinguishable here, and only the refusal is
        // forbidden, so the fallback is the whole surface and no external request is
        // made. The frame keeps no src.
        showExternalFallback();
        return;
      }
      showFrame();
      // A user-initiated reload of a PROXIED target is cache-busted (#1900): without
      // it, re-assigning the same URL may be answered from the browser's HTTP cache
      // (or an intermediary's) with exactly the stale page ↻ exists to escape. Built
      // from the pristine `src` each time, so the param is replaced, not accumulated.
      //
      // The value comes from the module-scope nextReloadNonce(), never a counter local
      // to this mount: the browser's cache outlives the pane, so a per-mount sequence
      // re-issues `_afreload=1` after every remount and gets answered from the entry the
      // FIRST ↻ of the previous mount created. The INITIAL mount still never busts —
      // that is `bust`, an argument, not the counter's starting value — so a fresh
      // preview's address stays clean.
      //
      // External targets are deliberately excluded — a presigned / CDN-token URL signs
      // over its query string, so a param would break the signature and 403 a preview
      // that works today. They fall back to the re-assign trick below, which is all
      // they ever had.
      //
      // A VS CODE pane is excluded for its own reason (see webProxied): the param buys
      // nothing there — the editor is not the thing a developer is iterating on, and
      // its notices are already served no-store — while code-server does read its own
      // query string (?folder=…), so injecting one is unforced risk for no gain.
      const next = bust && webProxied ? cacheBustedWebSrc(src, nextReloadNonce()) : src;
      // Re-assigning src is what forces a reload (contentWindow.reload throws
      // cross-origin), but assigning a URL the frame ALREADY holds does not navigate —
      // so clear it first in exactly that case. A cache-busted URL always differs, so
      // it navigates on its own; this is the external/initial path. Never clear a
      // fresh frame: assigning "" resolves against the document's base URL and would
      // load the SPA into itself.
      if (frame.getAttribute("src") === next) {
        frame.src = "";
      }
      frame.src = next;
    };

    reload.addEventListener("click", (e) => {
      e.stopPropagation();
      if (src === "") {
        return;
      }
      // re-probes (↻ is the natural "is it up yet?" after a dead state) and
      // cache-busts (#1900) — the two halves of "give me the CURRENT page".
      void load(true);
    });
    fbRetry.addEventListener("click", (e) => {
      e.stopPropagation();
      void load(true);
    });

    // No iframe `load` listener and no load-timeout any more (item A). Both existed
    // only for a direct external frame: the timeout revealed the fallback when no load
    // arrived, and onLoad hid it when one did. But an X-Frame-Options block page FIRES
    // load, so onLoad was exactly what un-hid the fallback and showed the browser's raw
    // refusal — the bug Sachin filed. External tabs now show the fallback outright and
    // never point the frame at a target, so there is no load event to interpret and
    // nothing to time out. Proxied web/vscode frames never used either (the probe, and
    // the daemon's own 503 notice, are their liveness signals).
    pane.webDispose = (): void => {
      disposed = true;
    };

    void load();
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
            this.retain(this.sessionId, this.tree, this.tabIds);
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
      // Dragging the pane's OWN tab onto its edge still splits — but the new half must
      // open a DIFFERENT tab (#1901). Binding the dragged tab on both sides is what the
      // one-tab-one-pane dedupe undoes, collapsing the split back to where it started.
      const onItsOwnPane = zone !== "center" && findLeaf(this.tree, pane.leafId)?.tab === tab;
      const opened = onItsOwnPane
        ? companionTab(this.tree, pane.leafId, tab, this.tabCount, this.preferredTabs())
        : tab;
      if (opened === null) {
        return; // no other tab to fill the new half — leave the layout as it stands
      }
      this.tree = zone === "center" ? replaceTab(this.tree, pane.leafId, tab) : splitLeaf(this.tree, pane.leafId, zone, opened);
      // Focus the pane holding the tab that just landed — the NEW half (VS Code focuses
      // the new split). On a self-split that half holds the COMPANION: the dragged tab
      // never moved, so focusing it would hand focus back to the pane the user started
      // from and the new pane would open unfocused.
      const landed = leaves(this.tree).find((l) => l.tab === opened);
      if (landed) {
        this.focusedId = landed.id;
      }
      this.commit();
      this.refocus();
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
    // A focus move is an explicit mutation for the stale-async guard, so count it
    // (see layoutGeneration). Not via commit(): the TREE is untouched here, and
    // reconciling/re-persisting it would be wasted work on every pane click. The
    // early return above means only a REAL change counts — re-focusing the pane
    // that already holds focus must not invalidate an in-flight close.
    this.layoutGen++;
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
    // Keep the focused-tab-id accurate FIRST — before the dedup guard below can bail.
    // report() follows every reconcile() and is the sole caller of the pure-focus path
    // (focusPane), so this is the one funnel that sees every way the focused tab can
    // change (#1901 Codex).
    this.syncFocusedTabId();
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

  /** Recomputes the focused pane's CURRENT tab id (by stable daemon id) and records the
   *  tab focus is LEAVING onto the MRU. THE funnel for keeping the focused-tab-id
   *  accurate, run at the TOP of report() — before its dedup guard — so it fires on
   *  every path report() covers: an explicit focus move (focusPane), a same-shape
   *  session switch, an in-place tab-identity update, and a tab change whose onLayout is
   *  deduped.
   *
   *  Recording only AFTER the dedup guard (the original #1901 code) saw just the first
   *  case. report() dedups on the focused ORDINAL, and a session switch or an identity
   *  swap leaves the ordinal put while the tab beneath it changes — so the id went stale
   *  and a self-split's "other side" pick read a neighbour, or a foreign session's tab
   *  (#1901 Codex findings 1-5). Reading the id from the live tree+roster here, every
   *  time, is what makes a stale value unrepresentable.
   *
   *  It is self-contained (reads the tree, not a passed ordinal) so a session switch's
   *  forgetFocusHistory()+reconcile()+report() reinitializes lastFocusedTabId to the NEW
   *  session's focused tab: the cleared "" makes the first sync a fresh start, never
   *  recorded as if the new tab were previously focused. */
  private syncFocusedTabId(): void {
    const focusedTab = this.focusedId
      ? (findLeaf(this.tree ?? { kind: "leaf", id: "", tab: 0 }, this.focusedId)?.tab ?? -1)
      : -1;
    const current = focusedTab >= 0 ? (this.tabRealIds[focusedTab] ?? "") : "";
    if (current === this.lastFocusedTabId) {
      return; // the focused tab's identity is unchanged — nothing to record
    }
    if (this.lastFocusedTabId !== "") {
      this.tabMru = [this.lastFocusedTabId, ...this.tabMru.filter((id) => id !== this.lastFocusedTabId)];
    }
    if (current !== "") {
      // A tab is not part of its own history: leaving it here would offer the tab the
      // pane already shows as that pane's own companion. companionTab rejects it on the
      // way out, but a preference list holding the CURRENT tab is a trap for the next
      // reader of preferredTabs, which reads as "where focus has been".
      this.tabMru = this.tabMru.filter((id) => id !== current);
    }
    this.lastFocusedTabId = current;
  }

  /** The recently-focused tabs as CURRENT ordinals, most-recent first. An id that no
   *  longer resolves (its tab was closed) drops out rather than binding a stranger. */
  private preferredTabs(): number[] {
    return this.tabMru.map((id) => this.tabRealIds.indexOf(id)).filter((i) => i >= 0);
  }

  /** Clears the focus history (a session switch, or a logout). Deliberately does NOT
   *  touch lastFocusedTab: that one is report()'s dedup key, and resetting it would
   *  re-fire onLayout for an unchanged layout — the store write #1855 turns on. */
  private forgetFocusHistory(): void {
    this.tabMru = [];
    this.lastFocusedTabId = "";
  }
}
