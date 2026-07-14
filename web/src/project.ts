// The single-project information architecture (redesign PR2). The web client is now
// SCOPED to one project (repo) at a time: the top-right switcher picks it, and the
// rail + tasks view show only that project's work. Projects are a pure CLIENT
// construct — derived by grouping the live session projection by repo root, exactly
// as the TUI's zero-config project list is (app/switch_project.go
// buildProjectListFrom) — so this never changes the daemon API shape.
//
// This module is the pure core of that IA: derive the project summaries the switcher
// menu shows (label, path, per-project session + working counts), scope a session
// list to one project, pick a sensible default, reconcile the selection when the
// session set changes, and persist the choice across reloads. It is DOM-free and
// I/O-free (bar the tiny localStorage helpers), so the derivation/scoping/reconcile
// rules are unit-tested (project.test.ts) independently of the shell wiring, exactly
// as sessions.ts / nav.ts are.

import { projectLabel } from "./modals.js";
import { isArchived, rowStatus } from "./status.js";
import type { SessionData } from "./types.js";

/** localStorage key for the persisted selected-project root (the repo path). Kept in
 *  localStorage (not sessionStorage) so the choice survives a full reload/new tab —
 *  the token lives in sessionStorage, but the project is a durable UI preference like
 *  the theme (theme.ts). */
const PROJECT_KEY = "af-project";

/** One project's cross-project glance for the switcher menu: its repo root (the
 *  stable id), a friendly name + full path, and the counts that make the menu the
 *  at-a-glance replacement for the old all-projects rail. */
export interface ProjectSummary {
  /** The repo root — the stable project id every project keys by. */
  root: string;
  /** The friendly short name (repo basename), shown as the switcher label. */
  name: string;
  /** The full repo path, shown as the menu item's secondary line. */
  path: string;
  /** Live (non-archived) sessions in this project — the "active work" count. */
  liveCount: number;
  /** Live sessions currently working (a spinning status dot) — the glance signal. */
  workingCount: number;
  /** Total sessions (live + archived) attributed to the project's repo root. */
  totalCount: number;
}

/** The repo basename (last path segment) — the switcher's compact project label,
 *  mirroring the TUI project picker's short name. Falls back to the full root if it
 *  has no segments (never expected from a real worktree path). */
export function projectName(root: string): string {
  const parts = root.replace(/\/+$/, "").split("/");
  return parts[parts.length - 1] || root;
}

/**
 * Derives the switcher's project list from the session projection: group EVERY
 * session (live + archived) that carries a repo root, one summary per distinct root,
 * sorted by path for a stable menu. A project exists as long as af has any session
 * in that repo — so a project whose sessions are all archived still appears (as "no
 * sessions yet" in the menu), the single-project analogue of the mockup's glance.
 * Sessions with no repo_path are skipped (they can't be attributed to a project),
 * the same rows the new-session picker omits.
 */
export function projectSummaries(sessions: SessionData[]): ProjectSummary[] {
  const byRoot = new Map<string, SessionData[]>();
  for (const s of sessions) {
    const root = s.worktree?.repo_path;
    if (!root) {
      continue;
    }
    const arr = byRoot.get(root) ?? [];
    arr.push(s);
    byRoot.set(root, arr);
  }
  return [...byRoot.keys()].sort().map((root) => {
    const rows = byRoot.get(root) ?? [];
    const live = rows.filter((s) => !isArchived(s));
    const working = live.filter((s) => rowStatus(s).spinning).length;
    return {
      root,
      name: projectName(root),
      path: root,
      liveCount: live.length,
      workingCount: working,
      totalCount: rows.length,
    };
  });
}

/** The one-line glance shown beside a switcher menu item, mirroring the mockup's
 *  "7 sessions · 2 working" / "no sessions yet". Counts LIVE sessions (active work);
 *  archived-only projects read as empty. */
export function projectMeta(p: ProjectSummary): string {
  if (p.liveCount === 0) {
    return "no sessions yet";
  }
  const base = `${p.liveCount} session${p.liveCount === 1 ? "" : "s"}`;
  return p.workingCount > 0 ? `${base} · ${p.workingCount} working` : base;
}

/** A friendly full label for a project (basename + parent), reused from the modal
 *  picker so the switcher and the pickers never diverge. */
export function projectFullLabel(root: string): string {
  return projectLabel(root);
}

/** The sessions attributed to `root` (live + archived), the rail's scoped list. A
 *  null root (no project selected — none exist) scopes to nothing. This is the ONE
 *  place the all-projects rail behavior is replaced: the rail renders this, never the
 *  full projection. */
export function scopeToProject(sessions: SessionData[], root: string | null): SessionData[] {
  if (!root) {
    return [];
  }
  return sessions.filter((s) => s.worktree?.repo_path === root);
}

/** The set of valid project roots (a project exists while af has any session in it).
 *  A selected root must be in this set or it is stale — reconcileProject falls back. */
function validRoots(sessions: SessionData[]): Set<string> {
  return new Set(projectSummaries(sessions).map((p) => p.root));
}

/**
 * The default project on first load / when the selection is stale: the project of the
 * MOST-RECENTLY-ACTIVE session (the greatest created_at, live sessions preferred), so
 * the client opens on the project the user most likely just worked in. Falls back to
 * the first project by path, then null when there are no projects at all.
 */
export function defaultProject(sessions: SessionData[]): string | null {
  const summaries = projectSummaries(sessions);
  if (summaries.length === 0) {
    return null;
  }
  // Prefer a live session's repo; only if there are no live sessions anywhere do we
  // fall back to the newest archived one, so the default lands on active work.
  const withRepo = sessions.filter((s) => s.worktree?.repo_path);
  const live = withRepo.filter((s) => !isArchived(s));
  const pool = live.length > 0 ? live : withRepo;
  let best: SessionData | null = null;
  for (const s of pool) {
    if (!best || (s.created_at ?? "") > (best.created_at ?? "")) {
      best = s;
    }
  }
  const root = best?.worktree?.repo_path;
  return root ?? summaries[0]?.root ?? null;
}

/**
 * Reconciles the selected project against the current session set: keep the current
 * selection if it is still a real project; else resume the persisted choice if it is;
 * else pick the default. Called on connect AND on every session-list change, so a
 * project that vanishes (its last session killed) falls back gracefully instead of
 * leaving the rail pinned to a dead root.
 */
export function reconcileProject(
  sessions: SessionData[],
  persisted: string | null,
  current: string | null,
): string | null {
  const valid = validRoots(sessions);
  if (current && valid.has(current)) {
    return current;
  }
  if (persisted && valid.has(persisted)) {
    return persisted;
  }
  return defaultProject(sessions);
}

// --- persistence -----------------------------------------------------------

/** The persisted selected-project root, or null if none is stored / storage is
 *  unavailable (private mode). Never throws — a missing choice just falls back to the
 *  default on load. */
export function loadProjectChoice(): string | null {
  try {
    const v = localStorage.getItem(PROJECT_KEY);
    return v && v !== "" ? v : null;
  } catch {
    return null;
  }
}

/** Persists the selected-project root so a reload resumes it. Swallows storage
 *  errors (private mode / disabled storage): the choice is a convenience, not load-
 *  bearing. */
export function persistProjectChoice(root: string): void {
  try {
    localStorage.setItem(PROJECT_KEY, root);
  } catch {
    // no-op: persistence is best-effort
  }
}
