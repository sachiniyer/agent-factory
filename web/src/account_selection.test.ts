import assert from "node:assert/strict";
import { test } from "node:test";
import { createSession } from "./api.js";
import { AccountSelection } from "./account_selection.js";
import type { AccountsResponse } from "./types.js";

const registry: AccountsResponse = {
  agents: ["claude", "codex"], defaults: { claude: "personal" },
  entries: ["personal", "work"].map(name => ({
    agent: "claude", name, dir: `/accounts/${name}`, registration_only: false, logged_in: true,
  })),
};

test("named account survives a same-agent project reload and is sent on create", async (t) => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick("work");
  assert.equal(selection.render(null, "claude"), "");
  let sent: Record<string, unknown> = {};
  t.mock.method(globalThis, "fetch", async (_url: string, init: RequestInit) => {
    sent = JSON.parse(String(init.body));
    return new Response(JSON.stringify({ data: { title: "probe", branch: "af/probe" }, error: null }));
  });
  await createSession({ title: "probe", repoPath: "/other-project", program: "", prompt: "",
    account: selection.render(registry, "claude") }, "test-token");
  assert.equal(sent.account, "work");
  assert.equal(selection.picked, true);
});

test("removed account falls back to inheritance and clears the deliberate choice", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick("work");
  const changed = { ...registry, entries: registry.entries.filter(row => row.name !== "work") };
  assert.equal(selection.render(changed, "claude"), "");
  assert.equal(selection.picked, false);
});

test("another agent cannot inherit a same-spelled identity; default choice survives reload", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick("work");
  assert.equal(selection.render(registry, "codex"), "");
  assert.equal(selection.picked, false);
  selection.render(registry, "claude");
  selection.pick("");
  selection.render(null, "claude");
  assert.equal(selection.render(registry, "claude"), "");
  assert.equal(selection.picked, true);
});

test("failed policy reload cannot erase a named identity", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick("work");
  selection.render(null, "claude", true);
  assert.equal(selection.namedChoicePending, true);
  assert.equal(selection.render(registry, "claude"), "work");
  selection.pick("");
  assert.equal(selection.namedChoicePending, false);
});

test("an unresolved agent is not evidence that a named account disappeared", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick("work");
  assert.equal(selection.render(registry, ""), "");
  assert.equal(selection.namedChoicePending, true);
  assert.equal(selection.render(registry, "claude"), "work");
});
