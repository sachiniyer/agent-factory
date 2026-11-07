// The Config pane's draft contract across rebuilds (#6692, #6861):
//
//   - typed-but-unsaved text in ANY field survives every rebuild — every config
//     text row, not only the most recently typed one (#6692's one-shared-pair
//     mechanism kept a single slot and silently dropped every earlier draft);
//   - a draft that was committed is cleared and never replayed — the Hosts add
//     form's capture/restore ran on every rebuild with no notion of "this draft
//     was already sent", so the just-saved values repainted onto the pristine
//     form and a second submit re-sent them as a duplicate (#6861);
//   - a failed save keeps the draft, so the refused text can be corrected.
//
// The rebuild machinery under test (capture → render → restore) is plain DOM,
// so these run under fakedom like hosts_section.test.ts — no browser needed.
// instanceof against the HTML* classes resolves to FakeElement, which the
// pane's own classList/attribute checks then discriminate exactly the way the
// real markup discriminates them.

import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";

import { FakeElement, FakeEvent, installFakeDom } from "./fakedom.js";
import { ConfigPane, type ConfigActions, type ConfigStatus } from "./config.js";
import { emptyAccountsState, type AccountsState } from "./accounts.js";
import { emptyQuotaState, type QuotaState } from "./quota.js";
import { emptyHostsState, type HostAddDraft, type HostsState } from "./hosts.js";
import type { ConfigEntry } from "./types.js";

const HTML_CLASSES = ["HTMLElement", "HTMLInputElement", "HTMLSelectElement", "HTMLButtonElement"] as const;

let dom: ReturnType<typeof installFakeDom>;
let restoreGlobals: () => void;

beforeEach(() => {
  dom = installFakeDom();
  const g = globalThis as Record<string, unknown>;
  const priors = HTML_CLASSES.map((name) => ({ name, prior: Object.getOwnPropertyDescriptor(g, name) }));
  for (const { name } of priors) {
    g[name] = FakeElement;
  }
  restoreGlobals = () => {
    for (const { name, prior } of priors) {
      if (prior) Object.defineProperty(g, name, prior);
      else delete g[name];
    }
  };
});

afterEach(() => {
  restoreGlobals();
  dom.restore();
});

const entry = (over: Partial<ConfigEntry> = {}): ConfigEntry => ({
  key: "default_program",
  type: "string",
  default: "claude",
  purpose: "The agent a new session runs.",
  tier: 1,
  tier_name: "core",
  settable: true,
  value: "claude",
  requires_restart: true,
  ...over,
});

const accounts = (): AccountsState => ({ ...emptyAccountsState(), loaded: true });
const quota = (): QuotaState => emptyQuotaState();
const hosts = (over: Partial<HostsState> = {}): HostsState => ({
  ...emptyHostsState(),
  editable: true,
  ...over,
});

const actions = (over: Partial<ConfigActions> = {}): ConfigActions => ({
  save: () => {},
  openAssistant: () => {},
  explain: async () => ({ ok: false, error: "" }),
  accounts: { register: () => {}, login: () => {} },
  hosts: { check: () => {}, add: () => {}, remove: () => {} },
  setTheme: () => {},
  disconnect: () => {},
  ...over,
});

/** The pane's root as the FakeElement it is under this harness. */
const el = (pane: ConfigPane): FakeElement => pane.el as unknown as FakeElement;

/** One config text row's live input, by the `aria-label = e.key` render gives it. */
const configInput = (pane: ConfigPane, key: string): FakeElement => {
  const found = el(pane).querySelector(`.af-config-list input[aria-label="${key}"]`);
  assert.ok(found !== null, `no text input rendered for ${key}`);
  return found;
};

/** One add-form field, by the HOST_INPUT_ATTR the section marks it with. */
const hostField = (pane: ConfigPane, key: string): FakeElement => {
  const found = el(pane).querySelector(`[data-host-input="${key}"]`);
  assert.ok(found !== null, `no add-form field rendered for ${key}`);
  return found;
};

/** Types `text` into an input the way the browser reports it. */
const type = (input: FakeElement, text: string): void => {
  input.value = text;
  input.dispatchEvent(new FakeEvent("input"));
};

const TWO_ROWS = [
  entry({ key: "default_program", value: "claude" }),
  entry({ key: "network.listen_addr", value: ":8443", purpose: "Web bind" }),
];

const HOST_ROWS = [
  entry({ key: "hosts", type: "table", value: "{}" }),
  entry({ key: "default_host", value: "" }),
];

// #6692: the pane kept ONE (editing, draft) pair, so typing in a second row
// overwrote the slot and the next rebuild repainted every earlier row from the
// daemon value — draft gone, "Unsaved" chip hidden, Save disabled, no sign the
// text was ever there.
test("drafts in TWO config rows both survive a rebuild (#6692)", () => {
  const pane = new ConfigPane(actions());
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(TWO_ROWS, "", null, accts, q, "system", hs);

  type(configInput(pane, "default_program"), "codex");
  type(configInput(pane, "network.listen_addr"), "127.0.0.1:9999");

  // Any unrelated rebuild — a refresh, a sibling's save, the advanced toggle.
  pane.update(TWO_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", hs);

  assert.equal(configInput(pane, "default_program").value, "codex",
    "the first row's draft must not be eaten by typing in a second");
  assert.equal(configInput(pane, "network.listen_addr").value, "127.0.0.1:9999");
});

// The whole pane, not a special case: the committed row's draft retires on its
// save edge — the daemon holds the value now — while every OTHER row's draft
// keeps surviving.
test("a saved row's draft retires on its success edge; sibling drafts survive", () => {
  const pane = new ConfigPane(actions());
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(TWO_ROWS, "", null, accts, q, "system", hs);

  type(configInput(pane, "default_program"), "codex");
  type(configInput(pane, "network.listen_addr"), "127.0.0.1:9999");

  // applyConfigValueNow's own sequence: the save's status edge lands first
  // (entries still old), then refreshConfig delivers the committed value.
  const saved: ConfigStatus = { key: "default_program", value: "codex", notice: "", error: "" };
  pane.update(TWO_ROWS, "", saved, accts, q, "system", hs);
  const refetched = TWO_ROWS.map((e) => (e.key === "default_program" ? { ...e, value: "codex" } : { ...e }));
  pane.update(refetched, "", saved, accts, q, "system", hs);

  assert.equal(configInput(pane, "default_program").value, "codex",
    "the saved row paints the daemon's committed value, never a stale draft");
  assert.equal(configInput(pane, "network.listen_addr").value, "127.0.0.1:9999",
    "a row that was never saved keeps its draft through its sibling's save");
});

// A refused save is the opposite edge: the rejected text stays so the user can
// correct it — for every drafted row, not just the refused one.
test("a refused save keeps every drafted row's text", () => {
  const pane = new ConfigPane(actions());
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(TWO_ROWS, "", null, accts, q, "system", hs);

  type(configInput(pane, "default_program"), "nightly");
  type(configInput(pane, "network.listen_addr"), "127.0.0.1:9999");

  const refused: ConfigStatus = { key: "default_program", value: "", notice: "", error: 'must be one of [stable, preview]' };
  pane.update(TWO_ROWS, "", refused, accts, q, "system", hs);

  assert.equal(configInput(pane, "default_program").value, "nightly",
    "the refused text must stay to be corrected");
  assert.equal(configInput(pane, "network.listen_addr").value, "127.0.0.1:9999");
});

// #6861: a successful add used to leave the committed values on screen —
// readHostDrafts snapshotted them and restoreHostDrafts replayed them onto the
// freshly rendered form — so a second "Add host" re-sent the same entry and the
// daemon refused the duplicate.
test("a successful hosts add clears the form; the rebuild leaves it empty (#6861)", () => {
  const sent: HostAddDraft[] = [];
  const pane = new ConfigPane(actions({
    hosts: { check: () => {}, add: (d) => { sent.push(d); }, remove: () => {} },
  }));
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(HOST_ROWS, "", null, accts, q, "system", hs);

  hostField(pane, "name").value = "box2";
  hostField(pane, "host").value = "gpu.tailnet";
  el(pane).querySelector(".af-hosts-add")!.click();
  assert.equal(sent.length, 1, "the submit must reach the shell's add");

  // doHostAdd's own order: the "saved host …" status, then refreshConfig with
  // the new table — two updates, and the form must be empty after both.
  const withHost = HOST_ROWS.map((e) =>
    e.key === "hosts" ? { ...e, value: '{"box2":{"host":"gpu.tailnet"}}' } : { ...e });
  const added = hosts({ status: { name: "", message: "saved host box2 (gpu.tailnet)", error: false } });
  pane.update(withHost, "", null, accts, q, "system", added);

  for (const key of ["name", "host", "user", "port", "identity_file", "workspace_root", "host_key_verification"]) {
    assert.equal(hostField(pane, key).value, "", `${key}: a committed draft must never be replayed`);
  }
  for (const key of ["use_ssh_config", "make_default"]) {
    assert.equal(hostField(pane, key).checked, false, `${key}: a committed flag must not stay set`);
  }

  // Pressing Add host again must not re-send the committed entry.
  el(pane).querySelector(".af-hosts-add")!.click();
  assert.equal(sent.length, 1, "a cleared form submits nothing — no duplicate add");
});

// The failure arm: the daemon's refusal keeps the draft so it can be corrected
// — the same rule as a refused config save, and the register form's own rule.
test("a refused hosts add keeps the draft for correction", () => {
  const sent: HostAddDraft[] = [];
  const pane = new ConfigPane(actions({
    hosts: { check: () => {}, add: (d) => { sent.push(d); }, remove: () => {} },
  }));
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(HOST_ROWS, "", null, accts, q, "system", hs);

  hostField(pane, "name").value = "box2";
  hostField(pane, "host").value = "gpu.tailnet";
  el(pane).querySelector(".af-hosts-add")!.click();
  assert.equal(sent.length, 1);

  const refused = hosts({ status: { name: "", message: 'host "box2" is already listed in config', error: true } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", refused);

  assert.equal(hostField(pane, "name").value, "box2", "a failed save keeps the draft");
  assert.equal(hostField(pane, "host").value, "gpu.tailnet", "a failed save keeps the draft");
});

// The send is async: the add's round trip can be in flight while the user is
// already typing the NEXT host. Text they typed since is a new draft, not a
// replay of the committed one — clearing it would be the same loss the fix
// exists to end. Only the fields still holding what was sent may retire.
test("text typed while the add was in flight survives its success edge", () => {
  const pane = new ConfigPane(actions({
    hosts: { check: () => {}, add: () => {}, remove: () => {} },
  }));
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(HOST_ROWS, "", null, accts, q, "system", hs);

  hostField(pane, "name").value = "box2";
  hostField(pane, "host").value = "gpu.tailnet";
  el(pane).querySelector(".af-hosts-add")!.click();

  // The request is still out; the user is already naming the next host.
  hostField(pane, "name").value = "box3";

  const added = hosts({ status: { name: "", message: "saved host box2 (gpu.tailnet)", error: false } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", added);

  assert.equal(hostField(pane, "name").value, "box3",
    "retyped text is the next draft — only committed text may be retired");
  assert.equal(hostField(pane, "host").value, "",
    "the untouched field still held committed text, so it retires");
});

// Codex P2: adds are not serialized — a second "Add host" can go out while
// the first is still in flight, and the answers may land in either order.
// Each success must retire ITS OWN send's draft; a single pending snapshot
// would retire the last send's values under any success, so an earlier send's
// success eats a draft a later refusal was supposed to keep for correction.
test("overlapping adds: each success retires its own send's draft", () => {
  const sent: HostAddDraft[] = [];
  const pane = new ConfigPane(actions({
    hosts: { check: () => {}, add: (d) => { sent.push(d); }, remove: () => {} },
  }));
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(HOST_ROWS, "", null, accts, q, "system", hs);

  hostField(pane, "name").value = "box2";
  hostField(pane, "host").value = "gpu.tailnet";
  el(pane).querySelector(".af-hosts-add")!.click();

  // The first send is still out; the user retypes and submits a second host.
  hostField(pane, "name").value = "box3";
  hostField(pane, "host").value = "fast.tailnet";
  el(pane).querySelector(".af-hosts-add")!.click();
  assert.equal(sent.length, 2);

  // The FIRST send's success lands: it answers box2's send, so it must not
  // touch box3's fields — that draft belongs to the send still in flight.
  const added2 = hosts({ status: { name: "", message: "saved host box2 (gpu.tailnet)", error: false } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", added2);

  assert.equal(hostField(pane, "name").value, "box3",
    "the first send's success must not eat the second send's draft");
  assert.equal(hostField(pane, "host").value, "fast.tailnet");

  // The second send is refused — its draft is still there to be corrected.
  const refused3 = hosts({ status: { name: "", message: 'host "box3" is already listed in config', error: true } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", refused3);
  assert.equal(hostField(pane, "name").value, "box3", "a refused send keeps its draft");
  assert.equal(hostField(pane, "host").value, "fast.tailnet");

  // And when the second send's own success lands, the form clears for real.
  const added3 = hosts({ status: { name: "", message: "saved host box3 (fast.tailnet)", error: false } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", added3);
  assert.equal(hostField(pane, "name").value, "", "the second send's own success retires its own draft");
  assert.equal(hostField(pane, "host").value, "");
});

// A check-all success writes the same `name: ""` non-error shape with
// `check: true` — it is a probe outcome, not an add commit, and must never be
// mistaken for one: treating it as a commit would eat a half-typed form.
test("a successful check-all does not retire an add-form draft", () => {
  const pane = new ConfigPane(actions());
  const accts = accounts();
  const q = quota();
  const hs = hosts();
  pane.update(HOST_ROWS, "", null, accts, q, "system", hs);

  hostField(pane, "name").value = "box2";
  hostField(pane, "host").value = "gpu.tailnet";

  const checkAll = hosts({ status: { name: "", message: "no hosts saved; add one below", error: false, check: true } });
  pane.update(HOST_ROWS.map((e) => ({ ...e })), "", null, accts, q, "system", checkAll);

  assert.equal(hostField(pane, "name").value, "box2", "a probe outcome is not a commit — the draft stays");
  assert.equal(hostField(pane, "host").value, "gpu.tailnet");
});
