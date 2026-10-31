// The loaded archive window (#6206-#6209). Per project key the client holds a
// newest-first prefix of the archive, everything newer than the key's cursor, plus
// any strays (a deep-linked old session). This module is the pure half of keeping
// that window true across a resync: re-reading what is held in bounded batches and
// re-paging the head to find rows archived while the client was away. Network reads
// are injected so the logic is testable.
import type { SessionData, SnapshotCursor } from "./types.js";

/** Ids per by-id read. The window is read in batches of this size at any window
 *  size; there is no size above which reconciliation switches off (#6208). */
export const REREAD_BATCH = 500;
/** Rows per request when re-paging the head of a window. */
export const DISCOVERY_PAGE = 200;

/** The held rows that still exist, read by id in batches. A row the daemon no
 *  longer returns (deleted elsewhere) is simply absent. */
export async function rereadHeld(
  ids: string[], fetchById: (ids: string[]) => Promise<SessionData[]>,
): Promise<SessionData[]> {
  const out: SessionData[] = [];
  for (let i = 0; i < ids.length; i += REREAD_BATCH) {
    out.push(...(await fetchById(ids.slice(i, i + REREAD_BATCH))));
  }
  return out;
}

/** Whether a row sorts strictly newer than the cursor in (created_at desc, id desc)
 *  order, that is, inside the window the cursor bounds. */
export function aheadOfCursor(row: SessionData, cursor: SnapshotCursor): boolean {
  const order = compareTimestamps(row.created_at ?? "", cursor.created_at);
  if (order === null) return true;
  if (order !== 0) return order > 0;
  return (row.id ?? "") > cursor.id;
}

/** Orders two RFC 3339 timestamps at the daemon's precision. Date.parse truncates
 *  to milliseconds, which would let two instants inside one millisecond tie and fall
 *  back to the id order where the daemon (olderThanCursor) compares them first.
 *  Null when either cannot be parsed. */
function compareTimestamps(a: string, b: string): number | null {
  const split = (s: string): { sec: number; frac: string } => ({
    sec: Date.parse(s.replace(/\.\d+/, "")),
    frac: (/\.(\d+)/.exec(s)?.[1] ?? "").padEnd(9, "0"),
  });
  const x = split(a);
  const y = split(b);
  if (Number.isNaN(x.sec) || Number.isNaN(y.sec)) return null;
  if (x.sec !== y.sec) return x.sec > y.sec ? 1 : -1;
  if (x.frac === y.frac) return 0;
  return x.frac > y.frac ? 1 : -1;
}

/** Re-pages a key's archive from its head until the page reaches the boundary
 *  (`cursor`, or the end when the key is exhausted: null). Returns the rows read,
 *  including the page that crosses the boundary (the caller dedupes by id), and
 *  whether it got there: `complete` is false when `maxPages` ran out first, so the
 *  caller must not treat the window as reconciled. */
export async function discoverHead(
  cursor: SnapshotCursor | null,
  fetchPage: (before: SnapshotCursor | null, limit: number) => Promise<{ sessions: SessionData[]; next: SnapshotCursor | null }>,
  maxPages: number,
): Promise<{ rows: SessionData[]; complete: boolean }> {
  const rows: SessionData[] = [];
  let before: SnapshotCursor | null = null;
  for (let page = 0; page < maxPages; page++) {
    const got = await fetchPage(before, DISCOVERY_PAGE);
    rows.push(...got.sessions);
    if (got.next === null || got.sessions.length === 0) return { rows, complete: true };
    const last = got.sessions[got.sessions.length - 1];
    if (cursor !== null && !aheadOfCursor(last, cursor)) return { rows, complete: true };
    before = got.next;
  }
  return { rows, complete: false };
}

/** Whether a resync has reason to re-page the window's head: the stream was not
 *  live since the last reconcile, or the daemon's archive figures moved. */
export function discoveryNeeded(
  dirty: boolean,
  previous: { total: number; byProject: Record<string, number> },
  next: { total: number; byProject: Record<string, number> },
): boolean {
  if (dirty || previous.total !== next.total) return true;
  const keys = new Set([...Object.keys(previous.byProject), ...Object.keys(next.byProject)]);
  for (const k of keys) if ((previous.byProject[k] ?? 0) !== (next.byProject[k] ?? 0)) return true;
  return false;
}

export interface ReconcileInput {
  /** Archived rows held now that are not in the live snapshot. */
  held: SessionData[];
  live: ReadonlySet<string | undefined>;
  /** The window as the client knows it: summary and per-key cursors. */
  archive: { total: number; byProject: Record<string, number>; cursors: Record<string, SnapshotCursor | null | undefined> };
  /** The daemon's summary from the snapshot being committed. */
  summary: { total: number; byProject: Record<string, number> };
  /** The stream was not live since the last reconcile. */
  dirty: boolean;
  fetchById: (ids: string[]) => Promise<SessionData[]>;
  fetchPage: (project: string | null, before: SnapshotCursor | null, limit: number) => Promise<{ sessions: SessionData[]; next: SnapshotCursor | null }>;
  isArchived: (s: SessionData) => boolean;
}

/** The archived rows a resync commits beside the live snapshot. While events cannot
 *  have been missed it keeps the held rows as they are and reads nothing; otherwise
 *  it re-reads the held rows by id (batched) and re-pages each loaded key's head. */
export async function reconcileWindow(i: ReconcileInput): Promise<{ rows: SessionData[]; complete: boolean }> {
  const byId = new Map<string, SessionData>(i.held.map((s) => [s.id ?? "", s]));
  if (!discoveryNeeded(i.dirty, i.archive, i.summary)) return { rows: [...byId.values()], complete: true };
  byId.clear();
  const keep = (row: SessionData): void => {
    if (i.isArchived(row) && !i.live.has(row.id)) byId.set(row.id ?? "", row);
  };
  let complete = true;
  for (const row of await rereadHeld(i.held.flatMap((s) => (s.id ? [s.id] : [])), i.fetchById)) keep(row);
  for (const [key, cursor] of Object.entries(i.archive.cursors)) {
    if (cursor === undefined) continue;
    const project = key === "" ? null : key;
    // Bounded by the archive's own size, not a constant: a window of any size is
    // walked to its boundary. Running out first leaves the window unreconciled.
    const maxPages = Math.ceil(i.summary.total / DISCOVERY_PAGE) + 2;
    const found = await discoverHead(cursor, (before, limit) => i.fetchPage(project, before, limit), maxPages);
    for (const row of found.rows) keep(row);
    if (!found.complete) complete = false;
  }
  return { rows: [...byId.values()], complete };
}
