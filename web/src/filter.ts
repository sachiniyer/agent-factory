// The rail's status filter (feat: hide archived by default). The web client now
// shows the sessions a user is actually working with: every state EXCEPT archived is
// on by default, and the rail-head control narrows or widens that set — reveal the
// archive, or focus on just the working sessions.
//
// This is a pure CLIENT-SIDE DISPLAY filter over the projection the daemon already
// sends: archived sessions still arrive in every Snapshot, and nothing here changes
// the API shape or what the daemon computes (the same discipline project.ts follows
// for the single-project IA). It is DOM-free and I/O-free bar the localStorage
// helpers, so the partition + persistence rules are unit-tested (filter.test.ts)
// independently of the shell wiring, exactly as sessions.ts / project.ts / nav.ts are.
//
// The filter keys on RowKind — the status a row DISPLAYS (status.ts rowKind), not the
// raw liveness — so what the checkbox hides is exactly what the eye sees.

import { ROW_KIND_LABELS, type RowKind, rowKind } from "./status.js";
import type { SessionData } from "./types.js";

/** localStorage key for the persisted filter. localStorage (not sessionStorage) so
 *  the choice survives a reload/new tab — a durable UI preference like the theme
 *  (theme.ts) and the selected project (project.ts). */
const FILTER_KEY = "af-status-filter";

/** Which states the rail shows, one flag per RowKind. A total record (never a
 *  partial/Set) so every state is an explicit yes/no: adding a RowKind is then a
 *  type error here rather than a silently-hidden group. */
export type StatusFilter = Record<RowKind, boolean>;

/** The filter menu's display order: live states first (the ones you act on), then
 *  the degraded ones, then the archive last — mirroring the rail's own live-then-
 *  archived partition (status.ts compareSessionsForRail). */
export const FILTER_KINDS: readonly RowKind[] = ["working", "ready", "lost", "dead", "limit", "archived"];

/** The default: every state EXCEPT archived. Archived sessions are history — they
 *  accumulate without bound (a long-lived project carries hundreds) and burying the
 *  handful of live rows under them is what this feature exists to fix. Every other
 *  state is a session you might still act on, so it stays visible. */
const DEFAULTS: StatusFilter = {
  working: true,
  ready: true,
  lost: true,
  dead: true,
  limit: true,
  archived: false,
};

/** A fresh copy of the default filter (never the shared object — callers own theirs
 *  and the store swaps references). */
export function defaultFilter(): StatusFilter {
  return { ...DEFAULTS };
}

/** The label for one filter checkbox — the SAME word the row's own status label uses
 *  (status.ts ROW_KIND_LABELS), so a checkbox and the rows it governs always agree. */
export function filterLabel(kind: RowKind): string {
  return ROW_KIND_LABELS[kind];
}

/** The sessions the rail should show: those whose DISPLAYED status is checked.
 *  Returns a new array and preserves input order, so callers stay free to sort
 *  (orderedSessions) before or after filtering. */
export function filterSessions(list: SessionData[], filter: StatusFilter): SessionData[] {
  return list.filter((s) => filter[rowKind(s)]);
}

/** A copy of `filter` with one state flipped — the checkbox's update. */
export function withKind(filter: StatusFilter, kind: RowKind, on: boolean): StatusFilter {
  return { ...filter, [kind]: on };
}

/** How many of `list` each state accounts for, for the menu's per-state glance
 *  counts. Every kind is present (0 when none), so the menu renders a stable set of
 *  rows rather than shuffling as sessions come and go. */
export function kindCounts(list: SessionData[]): Record<RowKind, number> {
  const counts: Record<RowKind, number> = { working: 0, ready: 0, lost: 0, dead: 0, limit: 0, archived: 0 };
  for (const s of list) {
    counts[rowKind(s)]++;
  }
  return counts;
}

/** True when the filter is untouched (the default set). Drives the control's
 *  "narrowed" indicator: the default — archived hidden — is the normal state and must
 *  NOT read as a filter the user has to go undo. */
export function isDefaultFilter(filter: StatusFilter): boolean {
  return FILTER_KINDS.every((k) => filter[k] === DEFAULTS[k]);
}

/** How many sessions the filter is hiding from `list` — the number the empty/notice
 *  copy reports so hidden work is never silently missing. */
export function hiddenCount(list: SessionData[], filter: StatusFilter): number {
  return list.length - filterSessions(list, filter).length;
}

// --- persistence -----------------------------------------------------------

/**
 * The persisted filter, defaulting any state the stored value doesn't mention.
 *
 * Overlaying onto defaultFilter() (rather than trusting the stored object whole) is
 * what makes a NEW state safe to add: a browser holding a filter written before that
 * state existed has no flag for it, and inheriting the default (shown) is the only
 * non-astonishing answer — a missing key must never silently hide a group of
 * sessions the user has no idea exist. Same reason garbage/corrupt storage falls back
 * to the default instead of an empty filter that would blank the rail. Never throws:
 * the choice is a convenience, not load-bearing (private mode / blocked storage).
 */
export function loadFilter(): StatusFilter {
  const filter = defaultFilter();
  let raw: string | null = null;
  try {
    raw = localStorage.getItem(FILTER_KEY);
  } catch {
    return filter;
  }
  if (!raw) {
    return filter;
  }
  let stored: unknown;
  try {
    stored = JSON.parse(raw);
  } catch {
    return filter;
  }
  if (!stored || typeof stored !== "object" || Array.isArray(stored)) {
    return filter;
  }
  const rec = stored as Record<string, unknown>;
  for (const kind of FILTER_KINDS) {
    // Only a real boolean overrides a default — a truthy string ("false"!) or a null
    // from a hand-edited/older payload must not be coerced into a hide.
    if (typeof rec[kind] === "boolean") {
      filter[kind] = rec[kind] as boolean;
    }
  }
  return filter;
}

/** Persists the filter so a reload resumes it. Swallows storage errors (private mode
 *  / disabled storage): the choice is a convenience, not load-bearing. */
export function persistFilter(filter: StatusFilter): void {
  try {
    localStorage.setItem(FILTER_KEY, JSON.stringify(filter));
  } catch {
    // no-op: persistence is best-effort
  }
}
