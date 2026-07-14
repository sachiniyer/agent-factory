// Tests for the single-project IA core (redesign PR2): deriving the switcher's
// project summaries (from sessions AND tasks), scoping a session list to one project,
// picking a sensible default, and reconciling the selection when the session/task set
// changes. Pure like sessions.ts / nav.ts — no DOM, no daemon — so the derivation and
// fallback rules are pinned independently of the shell wiring.

import { test } from "node:test";
import assert from "node:assert/strict";

import {
  defaultProject,
  projectMeta,
  projectName,
  projectSummaries,
  reconcileProject,
  scopeToProject,
} from "./project.js";
import { Liveness, type SessionData, type TaskData } from "./types.js";

function sess(title: string, over: Partial<SessionData> = {}): SessionData {
  return { title, branch: "b", liveness: Liveness.Ready, ...over };
}

/** A live "working" session (a spinning status dot). */
function working(title: string, root: string, created = "2026-01-01T00:00:00Z"): SessionData {
  return sess(title, { id: title, worktree: { repo_path: root }, liveness: Liveness.Running, created_at: created });
}

/** A live "ready" session. */
function ready(title: string, root: string, created = "2026-01-01T00:00:00Z"): SessionData {
  return sess(title, { id: title, worktree: { repo_path: root }, liveness: Liveness.Ready, created_at: created });
}

/** An archived session. */
function archived(title: string, root: string, created = "2026-01-01T00:00:00Z"): SessionData {
  return sess(title, { id: title, worktree: { repo_path: root }, liveness: Liveness.Archived, created_at: created });
}

/** A task in a project. */
function task(id: string, root: string, created = "2026-01-01T00:00:00Z"): TaskData {
  return { id, prompt: "p", project_path: root, program: "", enabled: true, created_at: created };
}

test("projectName: the repo basename is the compact switcher label", () => {
  assert.equal(projectName("/home/me/src/agent-factory"), "agent-factory");
  assert.equal(projectName("/home/me/src/agent-factory/"), "agent-factory", "trailing slash trimmed");
});

test("projectSummaries: groups by repo root, sorted by path, with counts", () => {
  const s = projectSummaries(
    [working("a", "/repos/b"), ready("c", "/repos/a"), working("d", "/repos/b"), archived("e", "/repos/b")],
    [],
  );
  assert.deepEqual(
    s.map((p) => p.root),
    ["/repos/a", "/repos/b"],
    "sorted by path",
  );
  const b = s[1];
  assert.equal(b?.liveCount, 2, "two live sessions in /repos/b (the archived one excluded from live)");
  assert.equal(b?.workingCount, 2, "both live /repos/b sessions are working");
  assert.equal(b?.totalCount, 3, "totalCount includes the archived row");
});

test("projectSummaries: an ARCHIVED-only repo is NOT a project (matches the daemon's live-only contract)", () => {
  const s = projectSummaries([archived("gone", "/repos/shelved")], []);
  assert.equal(s.length, 0, "an all-archived repo with no tasks drops out — no stale entry / no-op delete");
});

test("projectSummaries: a TASK-only repo IS a project (Fix 1: reachable via its tasks)", () => {
  const s = projectSummaries([], [task("t1", "/repos/tasked")]);
  assert.equal(s.length, 1, "a repo with a task but no session still lists as a project");
  assert.equal(s[0]?.liveCount, 0);
  assert.equal(s[0]?.taskCount, 1);
  assert.equal(projectMeta(s[0]!), "1 task");
});

test("projectSummaries: a repo counted once whether it has sessions, tasks, or both", () => {
  const s = projectSummaries([ready("a", "/repos/x")], [task("t1", "/repos/x"), task("t2", "/repos/x")]);
  assert.equal(s.length, 1);
  assert.equal(s[0]?.liveCount, 1);
  assert.equal(s[0]?.taskCount, 2);
});

test("projectSummaries: sessions/tasks without a repo root are skipped", () => {
  assert.equal(projectSummaries([sess("orphan", { id: "1" })], [task("t", "")]).length, 0);
});

test("projectMeta: the cross-project glance pluralizes and shows the working count", () => {
  assert.equal(projectMeta(projectSummaries([ready("a", "/r")], [])[0]!), "1 session");
  assert.equal(projectMeta(projectSummaries([ready("a", "/r"), ready("b", "/r")], [])[0]!), "2 sessions");
  assert.equal(
    projectMeta(projectSummaries([working("a", "/r"), ready("b", "/r")], [])[0]!),
    "2 sessions · 1 working",
  );
});

test("scopeToProject: only the selected repo's sessions (live + archived); null → none", () => {
  const all = [ready("a", "/x"), ready("b", "/y"), archived("c", "/x")];
  assert.deepEqual(
    scopeToProject(all, "/x").map((s) => s.title),
    ["a", "c"],
    "includes the archived row of the scoped repo",
  );
  assert.deepEqual(scopeToProject(all, null), [], "no project selected scopes to nothing");
});

test("defaultProject: the most-recently-active (latest created_at) live session's repo", () => {
  const all = [
    ready("old", "/repos/old", "2026-01-01T00:00:00Z"),
    ready("new", "/repos/new", "2026-06-01T00:00:00Z"),
  ];
  assert.equal(defaultProject(all, []), "/repos/new");
  assert.equal(defaultProject([], []), null, "no sessions, no tasks → no default");
});

test("defaultProject: falls back to a task-only project when nothing is live", () => {
  // An archived-only repo is NOT a project, so the default lands on the task project.
  assert.equal(defaultProject([archived("a", "/repos/shelved")], [task("t", "/repos/tasked")]), "/repos/tasked");
});

test("reconcileProject: keeps a still-valid current selection", () => {
  const all = [ready("a", "/x"), ready("b", "/y")];
  assert.equal(reconcileProject(all, [], null, "/y"), "/y", "current wins when still a real project");
});

test("reconcileProject: resumes the persisted choice when current is stale/absent", () => {
  const all = [ready("a", "/x"), ready("b", "/y")];
  assert.equal(reconcileProject(all, [], "/y", null), "/y", "persisted resumes on a fresh load");
  assert.equal(reconcileProject(all, [], "/y", "/gone"), "/y", "a vanished current falls back to persisted");
});

test("reconcileProject: a persisted task-only project resolves once its tasks load", () => {
  // Before tasks load (tasks=[]) the persisted task-only repo isn't valid yet, so it
  // falls back; once the tasks arrive it becomes selectable and persisted wins.
  assert.equal(reconcileProject([ready("a", "/x")], [], "/tasked", null), "/x", "falls back before tasks load");
  assert.equal(
    reconcileProject([ready("a", "/x")], [task("t", "/tasked")], "/tasked", null),
    "/tasked",
    "resolves to the task-only project once its task is known",
  );
});

test("reconcileProject: falls back to the default when neither current nor persisted is valid", () => {
  const all = [ready("a", "/x", "2026-01-01T00:00:00Z"), ready("b", "/y", "2026-06-01T00:00:00Z")];
  assert.equal(reconcileProject(all, [], "/gone", "/also-gone"), "/y", "the most-recently-active project");
  assert.equal(reconcileProject([], [], "/gone", "/gone"), null, "no projects → null");
});

test("reconcileProject: a project whose last live session archived AND has no tasks drops (goes away)", () => {
  // Deleting a project archives its live sessions; with no tasks it is no longer a
  // project, so the selection falls back — the "actually goes away" delete behavior.
  const before = [ready("a", "/x"), ready("b", "/y", "2026-06-01T00:00:00Z")];
  assert.equal(reconcileProject(before, [], null, "/x"), "/x");
  const afterDelete = [archived("a", "/x"), ready("b", "/y", "2026-06-01T00:00:00Z")];
  assert.equal(reconcileProject(afterDelete, [], null, "/x"), "/y", "the archived-only /x is gone; falls back to /y");
});
