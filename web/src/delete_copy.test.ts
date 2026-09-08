import assert from "node:assert/strict";
import { test } from "node:test";
import { deletionConfirmationBody } from "./modals.js";
import { isOffBoxWorkspace } from "./ui.js";

for (const backend_type of ["docker", "ssh", "sandbox", "remote"]) {
  test(`${backend_type} deletion warns about sandbox work before local ownership`, () => {
    const offBox = isOffBoxWorkspace({ backend_type });
    assert.equal(offBox, true);
    for (const externalWorktree of [false, true]) {
      const copy = deletionConfirmationBody({ offBox, externalWorktree, branchCreatedByUs: false });
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
    assert.match(deletionConfirmationBody({ offBox, externalWorktree: true, branchCreatedByUs: true }), /checkout and branch stay/);
    assert.match(deletionConfirmationBody({ offBox, externalWorktree: false, branchCreatedByUs: true }), /Uncommitted changes and unpushed commits are lost/);
    assert.match(deletionConfirmationBody({ offBox, externalWorktree: false, branchCreatedByUs: false }), /branch and its commits stay/);
  }
});
