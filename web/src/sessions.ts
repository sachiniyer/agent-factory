// The pure session-list reducer behind the sidebar (#1592 Phase 5 PR3). It holds
// the browser analogue of the TUI's read-only projection logic: how a Snapshot and
// a stream of /v1/events deltas fold into the rail's session list, with no DOM and
// no I/O. Keeping it pure means the exact create/kill/update/archive semantics the
// sidebar depends on are unit-tested (sessions.test.ts) independently of the DOM
// wiring in index.ts.
//
// Identity is the session title: titles are unique in af (one worktree, one row),
// and the killed/archived/restored events carry ONLY the title (see
// daemon/control_server.go), so the title is the one key every event shares.

import type { SessionData, WireEvent } from "./types.js";

/** The result of applying one event: the next list plus whether the caller must
 *  re-Snapshot. Partial events (archived/restored carry only a title, with no
 *  liveness to reconstruct) set needsResync so index.ts refetches authoritative
 *  state; the common created/updated/killed deltas apply in place (needsResync
 *  false) for an instant, poll-free update. */
export interface ApplyResult {
  sessions: SessionData[];
  needsResync: boolean;
}

/** Inserts or replaces a session by title, returning a new array (never mutates
 *  the input — the store swaps references so subscribers see a fresh state). */
export function upsertSession(list: SessionData[], s: SessionData): SessionData[] {
  const i = list.findIndex((x) => x.title === s.title);
  if (i === -1) {
    return [...list, s];
  }
  const next = list.slice();
  next[i] = s;
  return next;
}

/** Removes a session by title, returning a new array. */
export function removeSession(list: SessionData[], title: string): SessionData[] {
  return list.filter((x) => x.title !== title);
}

/**
 * Folds one events-plane delta into the list, mirroring the daemon's event
 * contract (agentproto/message.go, daemon/control_server.go):
 *  - created/updated carry the full projection → upsert in place;
 *  - killed carries only the title → remove the row;
 *  - archived/restored carry only the title and flip a liveness the event can't
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
      if (ev.data && ev.data.title) {
        return { sessions: removeSession(list, ev.data.title), needsResync: false };
      }
      return { sessions: list, needsResync: false };
    case "session.archived":
    case "session.restored":
      return { sessions: list, needsResync: true };
    default:
      return { sessions: list, needsResync: false };
  }
}

/** Keeps a valid selection: preserves the current one if it still exists, else
 *  null. Never auto-selects, so selection stays a deliberate user act — matching
 *  the read-only, no-surprise projection model. */
export function pickSelection(list: SessionData[], current: string | null): string | null {
  if (current && list.some((s) => s.title === current)) {
    return current;
  }
  return null;
}
