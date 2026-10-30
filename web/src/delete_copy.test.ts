import assert from "node:assert/strict";
import { test } from "node:test";
import { deletionConfirmationBody } from "./modals.js";
import { isOffBoxWorkspace } from "./ui.js";

for (const backend_type of ["docker", "ssh", "sandbox", "remote"]) {
  test(`${backend_type} deletion warns about sandbox work before local ownership`, () => {
    const offBox = isOffBoxWorkspace({ backend_type });
    assert.equal(offBox, true);
    for (const externalWorktree of [false, true]) {
      const copy = deletionConfirmationBody({ archived: false, offBox, externalWorktree, branchCreatedByUs: false });
      assert.match(copy, /removes the sandbox/);
      assert.match(copy, /Unpushed commits and uncommitted changes are lost/);
      assert.match(copy, /Archive publishes the branch first/);
      assert.doesNotMatch(copy, /commits stay|checkout and branch stay/);
      assert.ok([...copy].length <= 160);
    }
  });
}

test("local ownership variants retain their distinct deletion consequences", () => {
  for (const backend_type of [undefined, "local"]) {
    const offBox = isOffBoxWorkspace({ backend_type });
    assert.equal(offBox, false);
    assert.match(deletionConfirmationBody({ archived: false, offBox, externalWorktree: true, branchCreatedByUs: true }), /checkout and branch stay/);
    assert.match(deletionConfirmationBody({ archived: false, offBox, externalWorktree: false, branchCreatedByUs: true }), /Uncommitted changes and unpushed commits are lost/);
    assert.match(deletionConfirmationBody({ archived: false, offBox, externalWorktree: false, branchCreatedByUs: false }), /branch and its commits stay/);
  }
});

for (const offBox of [true, false]) {
  test(`archived ${offBox ? "sandbox" : "local"} deletion offers Restore, not Archive`, () => {
    const workspace = { archived: true, offBox, externalWorktree: false, branchCreatedByUs: false };
    const copy = deletionConfirmationBody(workspace);
    assert.match(copy, /Restore/);
    assert.doesNotMatch(copy, /Archive publishes|Archive to keep/);
    assert.match(copy, offBox ? /branch stays published/ : /archived worktree/);
    assert.match(copy, offBox ? /record/ : /branch and its commits stay/);
  });
}

test("archived local deletion still respects af-created branch ownership", () => {
  const copy = deletionConfirmationBody({ archived: true, offBox: false, externalWorktree: false, branchCreatedByUs: true });
  assert.match(copy, /archived worktree and af-created branch/);
  assert.match(copy, /unpushed commits are lost/);
  assert.match(copy, /Restore instead/);
  assert.doesNotMatch(copy, /branch.*stay|Archive to keep/);
});

// #6233: the delete confirm names a branch af does not own that survives the
// delete, and says it and its commits are kept; an unreadable HEAD is said, not guessed.
test("deletion copy names a user branch that survives", () => {
  const base = { archived: false, offBox: false, externalWorktree: false, branchCreatedByUs: true };
  const copy = deletionConfirmationBody({ ...base, keptBranch: { name: "feature/done-flag" } });
  assert.match(copy, /Branch 'feature\/done-flag' is checked out in the worktree and af does not own it/);
  assert.match(copy, /the branch and its commits are kept in the repo\./);
  assert.doesNotMatch(deletionConfirmationBody(base), /kept in the repo/);
  assert.match(deletionConfirmationBody({ ...base, keptBranch: { unreadable: true } }), /Could not read the worktree's current branch/);
});

test("kept-branch copy is only for local worktrees af removes", () => {
  const kept = { name: "feature/done-flag" };
  assert.doesNotMatch(deletionConfirmationBody({ archived: false, offBox: true, externalWorktree: false, branchCreatedByUs: true, keptBranch: kept }), /feature\/done-flag/);
  assert.doesNotMatch(deletionConfirmationBody({ archived: false, offBox: false, externalWorktree: true, branchCreatedByUs: true, keptBranch: kept }), /feature\/done-flag/);
});
