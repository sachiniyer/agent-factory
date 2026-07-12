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
 *  re-Snapshot. Partial events (archived/restored carry only {id,title}, with no
 *  liveness to reconstruct) set needsResync so index.ts refetches authoritative
 *  state; the common created/updated/killed deltas apply in place (needsResync
 *  false) for an instant, poll-free update. */
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
 *  - archived/restored carry {id,title} and flip a liveness the event can't
 *    convey → signal a resync so the caller refetches Snapshot;
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
