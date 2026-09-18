import assert from "node:assert/strict";
import { test } from "node:test";
import { createSession } from "./api.js";
import { AccountSelection } from "./account_selection.js";
import { AMBIENT_PIN_ACCOUNT } from "./account_scope.js";
import type { AccountsResponse } from "./types.js";

const registry: AccountsResponse = {
  agents: ["claude", "codex"], defaults: { claude: "personal" },
  pool_routing: true,
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

// #4404 review: the wire shape is computed from the RETAINED pick, not the
// select's current value — a failed reload replaces the DOM with one "" row,
// and serializing that would silently drop a deliberate ambient pin.
test("wireAccount sends the retained ambient pin through a failed reload", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  selection.pick(AMBIENT_PIN_ACCOUNT);
  // The reload fails: the select now shows only "Accounts unavailable" ("").
  selection.render(null, "claude", true);

  assert.deepEqual(selection.wireAccount(),
    { account: "", accountAmbient: true, accountAuto: false },
    "the pin survives the DOM losing every row that could express it");
});

test("wireAccount marks the routable ask, and only that ask, account_auto", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude");
  assert.deepEqual(selection.wireAccount(),
    { account: "", accountAmbient: false, accountAuto: true },
    "an untouched field is this client's routable ask");

  selection.pick("");
  assert.deepEqual(selection.wireAccount(),
    { account: "", accountAmbient: false, accountAuto: true },
    "the routable first row picked deliberately is the same ask");

  selection.pick("work");
  assert.deepEqual(selection.wireAccount(),
    { account: "work", accountAmbient: false, accountAuto: false },
    "a named account is a pin, not a routing request");

  selection.pick(AMBIENT_PIN_ACCOUNT);
  assert.deepEqual(selection.wireAccount(),
    { account: "", accountAmbient: true, accountAuto: false },
    "an explicit ambient pick opts out of routing, not into it");
});

// #4404 review: account_auto is sent only after the form SAW a routing daemon.
// The web decodes strictly on a pre-router daemon (no client-version header),
// so an unconditional bit fails every ordinary create there; and a failed or
// pending registry is not evidence of a router at all.
test("wireAccount opts in only once a routing daemon was observed", () => {
  const untouched = { account: "", accountAmbient: false };
  const selection = new AccountSelection();
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: false }, "nothing rendered yet");
  selection.render(null, "claude");
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: false }, "registry still loading");
  selection.render(null, "claude", true);
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: false }, "registry failed");
  selection.render({ ...registry, pool_routing: undefined }, "claude");
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: false }, "a pre-router daemon");
  selection.render(registry, "claude");
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: true }, "a routing daemon");
  selection.render(null, "claude", true);
  assert.deepEqual(selection.wireAccount(), { ...untouched, accountAuto: false },
    "a later failed reload withdraws the opt-in the failure row no longer describes");
});

test("wireAccount makes no routable ask on a backend the router skips", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude", false, false);
  assert.equal(selection.wireAccount().accountAuto, false, "ssh/sandbox/hook");
  selection.render(registry, "claude", false, null);
  assert.equal(selection.wireAccount().accountAuto, false, "a backend the catalog could not name");
  selection.render(registry, "claude", false, true);
  assert.equal(selection.wireAccount().accountAuto, true, "local or docker");
});

test("an ambient pin survives a backend move and back", () => {
  const selection = new AccountSelection();
  selection.render(registry, "claude", false, true);
  selection.pick(AMBIENT_PIN_ACCOUNT);
  selection.render(registry, "claude", false, false);
  selection.render(registry, "claude", false, true);
  assert.deepEqual(selection.wireAccount(), { account: "", accountAmbient: true, accountAuto: false },
    "moving the backend away and back must not turn a pin into a routable ask");
});
