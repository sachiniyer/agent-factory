// Tests for the projects-view derivation (#1735). groupSessionsByProject must
// group only LIVE sessions by repo root — archived sessions belong to the rail's
// Archived group, not the projects view — so archiving a repo's sessions (what
// delete-project does) drops its project row, and restoring one brings it back.
// This mirrors the TUI's live-only buildProjectListFrom.

import { test } from "node:test";
import assert from "node:assert/strict";

import { groupSessionsByProject } from "./projects.js";
import { Liveness, type SessionData } from "./types.js";

function sess(title: string, over: Partial<SessionData> = {}): SessionData {
  return { title, branch: "b", liveness: Liveness.Ready, ...over };
}

test("groups live sessions by repo root, sorted by path", () => {
  const groups = groupSessionsByProject([
    sess("a", { id: "1", worktree: { repo_path: "/repos/b" } }),
    sess("c", { id: "2", worktree: { repo_path: "/repos/a" } }),
    sess("d", { id: "3", worktree: { repo_path: "/repos/b" } }),
  ]);
  assert.deepEqual(
    groups.map((g) => g.root),
    ["/repos/a", "/repos/b"],
  );
  assert.equal(groups[1]?.sessions.length, 2, "both /repos/b sessions grouped");
});

test("archived sessions do NOT define a project (live-only)", () => {
  const groups = groupSessionsByProject([
    sess("live", { id: "1", worktree: { repo_path: "/repos/keep" }, liveness: Liveness.Ready }),
    sess("shelved", { id: "2", worktree: { repo_path: "/repos/gone" }, liveness: Liveness.Archived }),
  ]);
  assert.deepEqual(
    groups.map((g) => g.root),
    ["/repos/keep"],
    "a repo whose only session is archived drops out of the projects view",
  );
});

test("a project with a live and an archived session shows only the live one", () => {
  const groups = groupSessionsByProject([
    sess("live", { id: "1", worktree: { repo_path: "/repos/x" }, liveness: Liveness.Running }),
    sess("shelved", { id: "2", worktree: { repo_path: "/repos/x" }, liveness: Liveness.Archived }),
  ]);
  assert.equal(groups.length, 1);
  assert.equal(groups[0]?.sessions.length, 1, "the archived row is excluded from the project group");
  assert.equal(groups[0]?.sessions[0]?.id, "1");
});

test("sessions without a repo_path are skipped", () => {
  const groups = groupSessionsByProject([sess("orphan", { id: "1" })]);
  assert.equal(groups.length, 0);
});
