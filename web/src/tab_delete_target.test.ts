import { test } from "node:test";
import assert from "node:assert/strict";
import { captureTabDeleteTarget } from "./tab_delete_target.js";

const agent = { id: "agent-id", name: "agent", kind: 0 };

for (const id of [undefined, ""]) {
  test(`legacy tab ${JSON.stringify(id)}: unchanged object can be deleted`, () => {
    const target = { id, name: "shell", kind: 1 };
    const resolve = captureTabDeleteTarget(target);
    assert.equal(resolve([agent, target]), 1);
    assert.equal(resolve([agent, { name: "other", kind: 1 }, target]), 2);
  });
  test(`legacy tab ${JSON.stringify(id)}: same-name replacement is refused`, () => {
    const target = { id, name: "shell", kind: 1 };
    const resolve = captureTabDeleteTarget(target);
    assert.equal(resolve([agent, { ...target }]), -1);
  });
}

test("a stable ID follows a replacement projection and reorder, never a reused name", () => {
  const target = { id: "original-id", name: "shell", kind: 1 };
  const resolve = captureTabDeleteTarget(target);
  assert.equal(resolve([agent, { ...target, id: "replacement-id" }]), -1);
  assert.equal(resolve([agent, { name: "other", kind: 1 }, { ...target, name: "renamed" }]), 2);
});
