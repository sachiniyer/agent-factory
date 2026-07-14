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
import { isArchived, isWorking } from "./status.js";
import type { SessionData, TaskData } from "./types.js";

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
  /** Live sessions currently working (no status dot, #1765) — the glance signal. */
  workingCount: number;
  /** Total sessions (live + archived) attributed to the project's repo root. */
  totalCount: number;
  /** Scheduled tasks in this project — so a task-only repo (a project with no
   *  sessions) still lists with a meaningful glance and stays reachable. */
  taskCount: number;
}

/** The repo basename (last path segment) — the switcher's compact project label,
 *  mirroring the TUI project picker's short name. Falls back to the full root if it
 *  has no segments (never expected from a real worktree path). */
export function projectName(root: string): string {
  const parts = root.replace(/\/+$/, "").split("/");
  return parts[parts.length - 1] || root;
}

/**
 * Derives the switcher's project list, mirroring the daemon's LIVE-only project
 * contract (app/switch_project.go buildProjectListFrom) and extending it to tasks: a
 * repo is a project if it has any LIVE (non-archived) session OR any scheduled task.
 * One summary per distinct root, sorted by path for a stable menu.
 *
 * Two deliberate consequences:
 *  - An ARCHIVED-only repo (no live session, no task) is NOT a project — it drops out
 *    the moment its last session is archived, exactly as the daemon's DeleteProject
 *    reversible contract intends (deleting a project archives its live sessions, and
 *    the project disappears). So the switcher never shows a stale archived-only entry
 *    whose delete would be a silent no-op.
 *  - A TASK-only repo (a task but no session) IS a project, so its tasks stay
 *    reachable (the Tasks view scopes to it); its rail is the empty state.
 *
 * Archived sessions still count toward a project's totalCount (they show in the rail
 * when the project is live/task-derived), but never on their own define one. Rows /
 * tasks with no repo_path / project_path are skipped (they can't be attributed).
 */
export function projectSummaries(sessions: SessionData[], tasks: TaskData[]): ProjectSummary[] {
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
  const taskCounts = new Map<string, number>();
  for (const t of tasks) {
    const root = t.project_path;
    if (!root) {
      continue;
    }
    taskCounts.set(root, (taskCounts.get(root) ?? 0) + 1);
  }
  // A repo is a project if it has any LIVE session OR any task. Archived-only repos
  // (rows present but all archived, and no task) are excluded.
  const roots = new Set<string>();
  for (const [root, rows] of byRoot) {
    if (rows.some((s) => !isArchived(s))) {
      roots.add(root);
    }
  }
  for (const root of taskCounts.keys()) {
    roots.add(root);
  }
  return [...roots].sort().map((root) => {
    const rows = byRoot.get(root) ?? [];
    const live = rows.filter((s) => !isArchived(s));
    const working = live.filter((s) => isWorking(s)).length;
    return {
      root,
      name: projectName(root),
      path: root,
      liveCount: live.length,
      workingCount: working,
      totalCount: rows.length,
      taskCount: taskCounts.get(root) ?? 0,
    };
  });
}

/** The one-line glance shown beside a switcher menu item, mirroring the mockup's
 *  "7 sessions · 2 working". Prefers the live-session glance; a task-only project
 *  (no live sessions) reads as its task count. */
export function projectMeta(p: ProjectSummary): string {
  if (p.liveCount === 0) {
    return p.taskCount > 0 ? `${p.taskCount} task${p.taskCount === 1 ? "" : "s"}` : "no sessions yet";
  }
  const base = `${p.liveCount} session${p.liveCount === 1 ? "" : "s"}`;
  return p.workingCount > 0 ? `${base} · ${p.workingCount} working` : base;
}

/** A friendly full label for a project (basename + parent), reused from the modal
 *  picker so the switcher and the pickers never diverge. */
export function projectFullLabel(root: string): string {
  return projectLabel(root);
}

/** The repo roots offered by the new-session / add-task pickers: the UNION of every
 *  repo af has a SESSION in AND every repo it has a TASK in, sorted. Including task
 *  repos is what lets a TASK-ONLY project be the target of a new task (or session) —
 *  a session-only picker would omit it, so adding a task while a task-only project is
 *  selected would silently target another repo, or be blocked when there is no session
 *  repo at all (redesign PR2, Greptile follow-on Fix 1). */
export function pickerProjects(sessions: SessionData[], tasks: TaskData[]): string[] {
  const roots = new Set<string>();
  for (const s of sessions) {
    const root = s.worktree?.repo_path;
    if (root) {
      roots.add(root);
    }
  }
  for (const t of tasks) {
    if (t.project_path) {
      roots.add(t.project_path);
    }
  }
  return [...roots].sort();
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

/** The set of valid project roots (a project has any live session or any task). A
 *  selected root must be in this set or it is stale — reconcileProject falls back. */
function validRoots(sessions: SessionData[], tasks: TaskData[]): Set<string> {
  return new Set(projectSummaries(sessions, tasks).map((p) => p.root));
}

/**
 * The default project on first load / when the selection is stale: the project of the
 * MOST-RECENTLY-ACTIVE live session (the greatest created_at), so the client opens on
 * the project the user most likely just worked in. When no live sessions exist it
 * falls back to a task-only project (the newest task's), then the first project by
 * path, then null when there are no projects at all.
 */
export function defaultProject(sessions: SessionData[], tasks: TaskData[]): string | null {
  const summaries = projectSummaries(sessions, tasks);
  if (summaries.length === 0) {
    return null;
  }
  const valid = new Set(summaries.map((p) => p.root));
  // Prefer the most-recently-created LIVE session whose repo is a real project.
  let best: SessionData | null = null;
  for (const s of sessions) {
    const root = s.worktree?.repo_path;
    if (!root || isArchived(s) || !valid.has(root)) {
      continue;
    }
    if (!best || (s.created_at ?? "") > (best.created_at ?? "")) {
      best = s;
    }
  }
  if (best?.worktree?.repo_path) {
    return best.worktree.repo_path;
  }
  // No live sessions: fall back to the newest task's project (a task-only default).
  let bestTask: TaskData | null = null;
  for (const t of tasks) {
    if (!t.project_path || !valid.has(t.project_path)) {
      continue;
    }
    if (!bestTask || (t.created_at ?? "") > (bestTask.created_at ?? "")) {
      bestTask = t;
    }
  }
  return bestTask?.project_path ?? summaries[0]?.root ?? null;
}

/**
 * Reconciles the selected project against the current session + task sets: keep the
 * current selection if it is still a real project; else resume the persisted choice
 * if it is; else pick the default. Called on connect AND on every session/task change,
 * so a project that vanishes (its last live session archived AND no tasks) falls back
 * gracefully instead of leaving the rail pinned to a dead root.
 */
export function reconcileProject(
  sessions: SessionData[],
  tasks: TaskData[],
  persisted: string | null,
  current: string | null,
): string | null {
  const valid = validRoots(sessions, tasks);
  if (current && valid.has(current)) {
    return current;
  }
  if (persisted && valid.has(persisted)) {
    return persisted;
  }
  return defaultProject(sessions, tasks);
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
