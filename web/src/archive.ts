// The lazy archive (#6178). A long-lived daemon holds thousands of archived
// sessions and the first screen needs none of them, so the startup snapshot is
// live-only and the archive is read a page at a time when something asks for it.
// This module is the pure half: what the client knows about the archive it has not
// loaded, and how the rail's archived figures are read from that plus the rows it
// has. Nothing here touches the network or the DOM.
import type { ArchivedSummary } from "./api.js";
import { kindCounts } from "./filter.js";
import { isArchived } from "./status.js";
import type { OperatorKind } from "./status.js";
import type { SessionData, SnapshotCursor } from "./types.js";

export interface ArchiveState {
  /** True once a snapshot proved the daemon serves the lazy-archive shape. False
   *  for a daemon that predates it: every row, archived included, is then already
   *  in `sessions` and nothing here applies. */
  lazy: boolean;
  /** Archived sessions the daemon holds, in total and per project root. */
  total: number;
  byProject: Record<string, number>;
  /** Per project root ("" when no project is selected): the cursor of the next
   *  page, null once that project's archive is fully loaded, missing until its
   *  first page has been read. */
  cursors: Record<string, SnapshotCursor | null>;
  /** A page read is in flight. */
  loading: boolean;
  /** The last failed read, shown beside the control that retries it. */
  error?: string;
}

export function emptyArchive(): ArchiveState {
  return { lazy: false, total: 0, byProject: {}, cursors: {}, loading: false };
}

/** The archive state a fresh snapshot implies, keeping the page cursors the client
 *  already holds: a resync must not make it re-read pages it has. */
export function archiveFromSnapshot(previous: ArchiveState, summary: ArchivedSummary | undefined): ArchiveState {
  if (!summary) return emptyArchive();
  return { ...previous, lazy: true, total: summary.total, byProject: summary.byProject };
}

export function archiveKey(project: string | null): string {
  return project ?? "";
}

/** How many archived sessions the project holds: the daemon's count, or the rows
 *  already loaded when that is larger (an archive event ahead of the next resync). */
export function archivedCount(archive: ArchiveState, project: string | null, scoped: ReadonlyArray<SessionData>): number {
  const loaded = scoped.reduce((n, s) => n + (isArchived(s) ? 1 : 0), 0);
  if (!archive.lazy) return loaded;
  const known = project ? archive.byProject[project] ?? 0 : archive.total;
  return Math.max(known, loaded);
}

/** The rail filter menu's per-state counts with the archived figure read from the
 *  daemon's count rather than from the rows loaded so far. */
export function railKindCounts(
  archive: ArchiveState, project: string | null, scoped: ReadonlyArray<SessionData>,
): Record<OperatorKind, number> {
  const counts = kindCounts(scoped as SessionData[]);
  counts.archived = archivedCount(archive, project, scoped);
  return counts;
}

/** Whether the project's archive has pages left to read. */
export function archiveHasMore(archive: ArchiveState, project: string | null): boolean {
  return archive.lazy && archive.cursors[archiveKey(project)] != null;
}

/** Whether the project's first archived page still has to be read. */
export function archiveNeedsFirstPage(archive: ArchiveState, project: string | null): boolean {
  // An error parks the auto-load: the retry control is the only way to try again,
  // or a failing daemon would be hammered on every store update.
  if (!archive.lazy || archive.loading || archive.error) return false;
  const key = archiveKey(project);
  if (archive.cursors[key] !== undefined) return false;
  return (project ? archive.byProject[project] ?? 0 : archive.total) > 0;
}
