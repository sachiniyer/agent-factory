// Unit coverage for the stable tab identity (#1738). tabIdentity is what the tab
// bar snapshots at dragstart and the drop resolves against: it must now be the
// daemon-minted STABLE id (never reused across a close+recreate), retiring the old
// "kind:name" identity — whose one gap was two tabs sharing a kind:name. The legacy
// kind:name is kept only as a fallback for a record with no id.

import { test } from "node:test";
import assert from "node:assert/strict";

import { tabIdentity } from "./ui.js";

test("tabIdentity is the daemon stable id when present", () => {
  assert.equal(tabIdentity({ id: "deadbeefcafef00d", name: "shell", kind: 1 }), "deadbeefcafef00d");
});

test("tabIdentity distinguishes two tabs that share a kind:name once they carry ids", () => {
  // The exact residual #1738 closes: a "shell" closed and a fresh "shell" created
  // share kind:name, but their stable ids differ — so the drag guard can no longer
  // be fooled into binding a pane to the replacement.
  const a = tabIdentity({ id: "id-old", name: "shell", kind: 1 });
  const b = tabIdentity({ id: "id-new", name: "shell", kind: 1 });
  assert.notEqual(a, b);
});

test("tabIdentity falls back to kind:name for a legacy tab with no id", () => {
  assert.equal(tabIdentity({ name: "shell", kind: 1 }), "1:shell");
  assert.equal(tabIdentity({ id: "", name: "agent", kind: 0 }), "0:agent");
});
