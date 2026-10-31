import assert from "node:assert/strict";
import { test } from "node:test";
import { parseRoute, serializeRoute, parseProjectRoute, serializeProjectRoute, stashRoute, restoreRoute } from "./route.js";

test("session routes round-trip stable IDs, including encoded characters", () => {
  for (const session of ["abc-123", "a/b", "a b", "é?#%"])
    assert.deepEqual(parseRoute(serializeRoute({ session })), { session });
  assert.equal(serializeRoute(null), "");
  for (const hash of ["", "#", "#/session/", "#/session/a/b", "#/task/a", "#/session/%", "#/session/%00", "#/session/%20"])
    assert.equal(parseRoute(hash), null, hash);
});

test("project routes round-trip repo roots, including slashes and spaces", () => {
  for (const project of ["/home/u/repo", "/a b/c", "/é?#%"])
    assert.deepEqual(parseProjectRoute(serializeProjectRoute({ project })), { project });
  for (const hash of ["", "#/project/", "#/project/a/b", "#/session/a", "#/project/%", "#/project/%00", "#/project/%20"])
    assert.equal(parseProjectRoute(hash), null, hash);
  // A project link is not a session link.
  assert.equal(parseRoute(serializeProjectRoute({ project: "/r" })), null);
});

test("login stash keeps a project link across sign-in", () => {
  const saved = storage();
  const hash = serializeProjectRoute({ project: "/home/u/repo" });
  stashRoute(hash, saved);
  assert.equal(restoreRoute("", saved), hash);
  stashRoute("#/junk", saved);
  assert.equal(restoreRoute("", saved), "");
});

function storage() {
  const values = new Map<string, string>();
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => { values.set(key, value); },
    removeItem: (key: string) => { values.delete(key); },
  };
}

test("login stash survives landing on / and is consumed once", () => {
  const saved = storage();
  stashRoute("#/session/a", saved);
  assert.equal(restoreRoute("", saved), "#/session/a");
  assert.equal(restoreRoute("", saved), "");
  stashRoute("#/session/a", saved);
  assert.equal(restoreRoute("#/session/b", saved), "#/session/b");
  assert.equal(restoreRoute("", saved), "");
  stashRoute("#/session/a", saved);
  stashRoute("", saved);
  assert.equal(restoreRoute("", saved), "");
});

test("disabled storage does not prevent opening a fragment", () => {
  const fail = () => { throw new Error("disabled"); };
  const saved = { getItem: fail, setItem: fail, removeItem: fail };
  assert.doesNotThrow(() => stashRoute("#/session/a", saved));
  assert.equal(restoreRoute("#/session/a", saved), "#/session/a");
});
