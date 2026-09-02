// The pure session-list reducer behind the sidebar (#1592 Phase 5 PR3, id-keyed
// in PR5). It holds the browser analogue of the TUI's read-only projection logic:
// how a Snapshot and a stream of /v1/events deltas fold into the rail's session
// list, with no DOM and no I/O. Keeping it pure means the exact create/kill/update/
// archive semantics the sidebar depends on are unit-tested (sessions.test.ts)
// independently of the DOM wiring in index.ts.
//
// Identity for the LIST reducer is the STABLE session id (session.id), the same
// key SELECTION uses (pickSelection) and the attach terminal dials
// (/v1/sessions/{id}/stream). Since #1592 Phase 5 PR5 EVERY lifecycle event —
// created/updated AND the delete-class killed/archived/restored — carries the id
// (daemon/control_server.go stamps it), so keying by id is total. This fixes the
// cross-repo title-collision bug: two sessions can share a title in different
// repos, and a kill of one must remove exactly that row, not both. A session that
// somehow carries no id (a legacy/disk-only record that never appears in a live
// Snapshot) degrades to a title key — the same title fallback the daemon keeps
// (killTargetStableID → "" → match-by-title).

import type { SessionData, WireEvent } from "./types.js";

/** The result of applying one event: the next list plus whether the caller must
 *  re-Snapshot. Full session projections apply in place. Older partial archive or
 *  restore events set needsResync because their liveness cannot be reconstructed;
 *  the browser never derives daemon-owned lifecycle state itself. */
export interface ApplyResult {
  sessions: SessionData[];
  needsResync: boolean;
}

/** The stable identity key of a session or a delete-class event payload: the id
 *  when present, else the title (the legacy/disk-only fallback matching the
 *  daemon's own title fallback). Two rows only collide when they share BOTH an
 *  empty id and a title — the pre-#1195 case the daemon never mints anymore. */
export function sessionKey(s: { id?: string; title: string }): string {
  return s.id && s.id !== "" ? `id ${s.id}` : `title ${s.title}`;
}

/** Inserts or replaces a session by its stable key, returning a new array (never
 *  mutates the input — the store swaps references so subscribers see a fresh
 *  state). */
export function upsertSession(list: SessionData[], s: SessionData): SessionData[] {
  const key = sessionKey(s);
  const i = list.findIndex((x) => sessionKey(x) === key);
  if (i === -1) {
    return [...list, s];
  }
  const next = list.slice();
  next[i] = s;
  return next;
}

/** Removes a session matching the given key (id-first, title-fallback), returning
 *  a new array. Takes the whole event payload so the id disambiguates a
 *  cross-repo duplicate title. */
export function removeSession(list: SessionData[], target: { id?: string; title: string }): SessionData[] {
  const key = sessionKey(target);
  return list.filter((x) => sessionKey(x) !== key);
}

/**
 * Folds one events-plane delta into the list, mirroring the daemon's event
 * contract (agentproto/message.go, daemon/control_server.go):
 *  - created/updated carry the full projection → upsert by id in place;
 *  - killed carries {id,title} → remove the row keyed by id;
 *  - archived/restored carry the daemon's full projection → upsert immediately;
 *    tolerate an older {id,title}-only event by asking for a Snapshot instead;
 *  - task.* are not a sidebar concern → no-op.
 * The list is returned unchanged (same reference) for events that don't touch it.
 */
export function applyEvent(list: SessionData[], ev: WireEvent): ApplyResult {
  switch (ev.type) {
    case "session.created":
    case "session.updated":
      if (ev.data && ev.data.title) {
        return { sessions: upsertSession(list, ev.data), needsResync: false };
      }
      return { sessions: list, needsResync: false };
    case "session.killed":
      if (ev.data && (ev.data.id || ev.data.title)) {
        return { sessions: removeSession(list, ev.data), needsResync: false };
      }
      return { sessions: list, needsResync: false };
    case "session.archived":
    case "session.restored":
      if (ev.data && ev.data.title && ev.data.liveness !== undefined) {
        return { sessions: upsertSession(list, ev.data), needsResync: false };
      }
      return { sessions: list, needsResync: true };
    case "projects.changed":
      // A project was deleted as a whole (#1735): the per-session archived events
      // already flip each row, but resync so the derived projects view (and any
      // dropped root_agents opt-in) settle against authoritative state.
      return { sessions: list, needsResync: true };
    default:
      return { sessions: list, needsResync: false };
  }
}

/** Keeps a valid selection keyed by session id: preserves the current id if a row
 *  with it still exists (e.g. after an in-place update), else null (e.g. after the
 *  selected session is killed). Never auto-selects, so selection stays a deliberate
 *  user act — matching the read-only, no-surprise projection model. */
export function pickSelection(list: SessionData[], currentId: string | null): string | null {
  if (currentId && list.some((s) => s.id === currentId)) {
    return currentId;
  }
  return null;
}

/** Clamps the active tab index to the selected session's LIVE tab list (#1592
 *  Phase 5 PR7). When the list changes out from under the client — another client
 *  created/closed a tab, or a tab died — an index past the end falls back to the
 *  last tab, and a vanished/absent selection falls back to the agent tab (0). This
 *  keeps the visible tab AND the streamed tab (index.ts syncTerminal reads it) in
 *  sync with the daemon projection, so the terminal never streams a stale/absent
 *  tab. The count floors at 1: a pre-#930 record with no tabs is one implicit
 *  agent tab. */
export function clampActiveTab(list: SessionData[], selectedId: string | null, activeTab: number): number {
  if (!selectedId) {
    return 0;
  }
  const sel = list.find((s) => s.id === selectedId);
  if (!sel) {
    return 0;
  }
  const n = sel.tabs && sel.tabs.length > 0 ? sel.tabs.length : 1;
  return Math.min(Math.max(activeTab, 0), n - 1);
}

/** The IDENTITY of the tab a pane should end on once the tab at `closedIndex` is
 *  closed, given the roster (`ids`, from ui.tabIdentity) and active index as they
 *  stand when the close is ISSUED. Returns "" when there is nothing to follow.
 *
 *  Closing a tab shifts every higher tab down by one, and the tempting way to
 *  re-point the pane is to subtract that shift from the active index. But an
 *  ordinal only names a tab relative to ONE roster, and a close spans two — the
 *  pre-close list it is computed against and the post-close list it is applied to.
 *  Anything else that moves the roster in between (a concurrent close from another
 *  client, now delivered live by #1815) invalidates the arithmetic silently, and the
 *  pane lands on a neighbour. Naming the TAB instead survives any reshuffle: the
 *  caller resolves this identity to its current ordinal in the post-close roster,
 *  and a -1 there is the honest "it's gone too" rather than a wrong guess.
 *
 *  This is the store-side twin of layout.remapByIdentity, which does the same for
 *  the pane tree (#1779): a pane follows a TAB, never a slot. */
export function tabToKeepOnClose(ids: string[], closedIndex: number, activeIndex: number): string {
  // Closing the ACTIVE tab: there is no surviving tab to follow, so fall back to its
  // left neighbour — picked from the PRE-close list, the only roster in which "the
  // tab left of the one being closed" is still a well-defined statement.
  const keepIndex = closedIndex === activeIndex ? activeIndex - 1 : activeIndex;
  return ids[keepIndex] ?? "";
}

/** The gesture a post-await rebind belongs to. Both await a round trip and then
 *  re-point the focused pane; only a create also attaches the keyboard to it. */
export type TabRebindVerb = "create" | "close";

/** Why a post-await rebind was refused. The refusal itself is #2000's, and correct;
 *  naming it is what stops it reading as a hang (#3663). */
export type TabRebindRefusal = "selection-moved" | "layout-moved" | "tab-gone";

/** Either the tab ordinal the focused pane should end on, or the reason it must stay
 *  where it is. */
export type TabRebindOutcome = { kind: "rebind"; idx: number } | { kind: "refused"; reason: TabRebindRefusal };

/** The tab ordinal a post-await pane rebind should land on, or the refusal that
 *  leaves the pane where it is. The store-side twin of the layout guard, and the
 *  shared decision createSessionTab and closeSessionTab both route through (#2000).
 *
 *  Both verbs await a round trip and then re-point the FOCUSED pane. splitView.trees
 *  is per-session and setFocusedTab carries no session of its own, so a `targetIdx`
 *  resolved against the roster the verb mutated must not be applied once the user has
 *  formed a NEWER intent during the await:
 *
 *   - selection guard: the user selected another session (`currentSelId` moved off the
 *     pinned `pinnedSelId`), so the ordinal names a tab in a session that is no longer
 *     on screen — applying it re-points and attaches the wrong session's pane. This is
 *     the #1815 finding, in the one place (create) its guard was never applied.
 *   - generation guard: the user focused another pane or changed the layout, bumping
 *     `layoutGeneration`; re-pointing from an intent formed before that yanks it back.
 *
 *  `targetIdx < 0` is the honest "the tab is gone" — the created/kept tab was closed
 *  out-of-band during the await, or its session vanished — and the pane stays where
 *  syncSplit's identity remap already settled it, never a guess. Mirrors
 *  closeSessionTab's `next >= 0` and tabToKeepOnClose's -1.
 *
 *  It answers WHICH of the three refused, rather than a bare -1, because the caller
 *  has to tell the user something: the tab bar grew or shrank while the pane stayed
 *  put, and a refusal nobody reports is indistinguishable from a hang — the #3663
 *  signature. The selection is checked first: leaving the session subsumes whatever
 *  the layout did meanwhile, and is the fact worth telling them. */
export function rebindTargetAfterAwait(
  pinnedGen: number,
  pinnedSelId: string,
  currentGen: number,
  currentSelId: string | null,
  targetIdx: number,
): TabRebindOutcome {
  if (currentSelId !== pinnedSelId) {
    return { kind: "refused", reason: "selection-moved" };
  }
  if (currentGen !== pinnedGen) {
    return { kind: "refused", reason: "layout-moved" };
  }
  if (targetIdx < 0) {
    return { kind: "refused", reason: "tab-gone" };
  }
  return { kind: "rebind", idx: targetIdx };
}

/** What to tell the user when a rebind was refused: what the gesture DID do, then why
 *  the pane did not follow. Never an error — nothing failed, and in two of the three
 *  cases the refusal is the user's own newer intent being honoured. The wording is
 *  what makes the outcome distinguishable from a hang, so it names the pane's state
 *  rather than reporting a fault (#3663). */
export function tabRebindRefusalNotice(reason: TabRebindRefusal, verb: TabRebindVerb): string {
  const done = verb === "create" ? "Tab created" : "Tab closed";
  switch (reason) {
    case "selection-moved":
      return `${done} on the session you left · this pane kept its tab`;
    case "layout-moved":
      return `${done} · the pane you focused meanwhile kept its tab`;
    case "tab-gone":
      return verb === "create"
        ? `${done}, but it is no longer in the tab list · the pane kept its tab`
        : `${done} · the tab the pane would have moved to is gone too`;
  }
}
