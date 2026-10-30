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
/** A hard stop for a runaway discovery (a daemon that never ends its cursor). */
const DISCOVERY_MAX_PAGES = 200;

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
  const a = Date.parse(row.created_at ?? "");
  const b = Date.parse(cursor.created_at);
  if (Number.isNaN(a) || Number.isNaN(b)) return true;
  if (a !== b) return a > b;
  return (row.id ?? "") > cursor.id;
}

/** Re-pages a key's archive from its head until the page reaches the boundary
 *  (`cursor`, or the end when the key is exhausted: null). Returns the rows read,
 *  including the page that crosses the boundary; the caller dedupes by id. */
export async function discoverHead(
  cursor: SnapshotCursor | null,
  fetchPage: (before: SnapshotCursor | null, limit: number) => Promise<{ sessions: SessionData[]; next: SnapshotCursor | null }>,
): Promise<SessionData[]> {
  const out: SessionData[] = [];
  let before: SnapshotCursor | null = null;
  for (let page = 0; page < DISCOVERY_MAX_PAGES; page++) {
    const got = await fetchPage(before, DISCOVERY_PAGE);
    out.push(...got.sessions);
    if (got.next === null || got.sessions.length === 0) break;
    const last = got.sessions[got.sessions.length - 1];
    if (cursor !== null && !aheadOfCursor(last, cursor)) break;
    before = got.next;
  }
  return out;
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
export async function reconcileWindow(i: ReconcileInput): Promise<SessionData[]> {
  const byId = new Map<string, SessionData>(i.held.map((s) => [s.id ?? "", s]));
  if (!discoveryNeeded(i.dirty, i.archive, i.summary)) return [...byId.values()];
  byId.clear();
  const keep = (row: SessionData): void => {
    if (i.isArchived(row) && !i.live.has(row.id)) byId.set(row.id ?? "", row);
  };
  for (const row of await rereadHeld(i.held.flatMap((s) => (s.id ? [s.id] : [])), i.fetchById)) keep(row);
  for (const [key, cursor] of Object.entries(i.archive.cursors)) {
    if (cursor === undefined) continue;
    const project = key === "" ? null : key;
    for (const row of await discoverHead(cursor, (before, limit) => i.fetchPage(project, before, limit))) keep(row);
  }
  return [...byId.values()];
}
