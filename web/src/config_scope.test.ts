// The Config view's project rows (#7009). The view used to render the GLOBAL
// value of a key the selected project overrides, looking authoritative. Now the
// row's marker line says what the project uses, the control still edits the
// global value, and a banner says which scope the rows were read in — including
// when the daemon could not resolve the project or predates the field.
//
// Rendered under fakedom like config_drafts.test.ts: the contract is which
// text lands where and what the controls are seeded with, both plain DOM.

import { afterEach, beforeEach, test } from "node:test";
import assert from "node:assert/strict";

import { FakeElement, FakeEvent, installFakeDom } from "./fakedom.js";
import { ConfigPane, type ConfigActions, type ConfigScope, editBaseline } from "./config.js";
import { emptyAccountsState } from "./accounts.js";
import { emptyQuotaState } from "./quota.js";
import { emptyHostsState } from "./hosts.js";
import type { ConfigEntry, ExplainConfigResponse } from "./types.js";

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

const PROJECT: ConfigScope = { requested: "/srv/app", root: "/srv/app", error: "" };
const GLOBAL: ConfigScope = { requested: "", root: "", error: "" };

function render(entries: ConfigEntry[], scope: ConfigScope = PROJECT, acts: ConfigActions = actions()): ConfigPane {
  const pane = new ConfigPane(acts);
  // An empty path keeps the header off `location.host`, which fakedom lacks.
  pane.update(entries, "", null,
    { ...emptyAccountsState(), loaded: true }, emptyQuotaState(), "system",
    { ...emptyHostsState(), editable: true }, scope);
  return pane;
}

const el = (pane: ConfigPane): FakeElement => pane.el as unknown as FakeElement;

const rowPart = (pane: ConfigPane, key: string, selector: string): FakeElement | null =>
  el(pane).querySelector(`.af-config-row[data-key="${key}"] ${selector}`);

const marked = (scope: string): ConfigEntry => entry({ value: "aider", scope, global_value: "codex" });

test("editBaseline is the global value on a scope-marked row and the value otherwise", () => {
  assert.equal(editBaseline(marked("project")), "codex");
  assert.equal(editBaseline(entry({ value: "aider" })), "aider");
  assert.equal(editBaseline(entry({ value: "aider", scope: "project" })), "",
    "a marked row with no global value edits an unset global key");
});

test("a scope-marked row says what the project uses and that the field edits the global value", () => {
  const cases: Record<string, string> = {
    "project": "project override · this project uses: aider · the field above edits the global value",
    "personal": "personal override · this project uses: aider · the field above edits the global value",
    "project+personal": "project + personal overrides · this project uses: aider · the field above edits the global value",
    "personal-ignored": "personal override — stored, not applied · this project uses the global value",
    // An open set: a layer a future daemon adds still reads as "not global".
    "team": "project-scoped override · this project uses: aider · the field above edits the global value",
  };
  for (const [scope, text] of Object.entries(cases)) {
    const pane = render([marked(scope)]);
    const line = rowPart(pane, "default_program", ".af-config-scope");
    assert.ok(line !== null, `no marker line for scope ${scope}`);
    assert.equal(line.textContent, text);
  }
});

test("an effective empty value reads as (unset) in the marker line", () => {
  const pane = render([entry({ value: "", scope: "project", global_value: "codex" })]);
  assert.equal(rowPart(pane, "default_program", ".af-config-scope")?.textContent,
    "project override · this project uses: (unset) · the field above edits the global value");
});

test("an unmarked row carries no marker line", () => {
  const pane = render([entry()]);
  assert.equal(rowPart(pane, "default_program", ".af-config-scope"), null);
});

test("a marked text row is seeded with the global value and saves only a real change", () => {
  const sent: string[] = [];
  const pane = render([marked("project")], PROJECT, actions({ save: (_k, v) => { sent.push(v); } }));
  const input = rowPart(pane, "default_program", "input")!;
  const save = rowPart(pane, "default_program", ".af-config-save")!;
  assert.equal(input.value, "codex", "the field edits the global file, so it opens on the global value");
  assert.equal(save.disabled, true, "an untouched marked row is not worth writing");

  // Typing the project's effective value IS a change to the global file.
  input.value = "aider";
  input.dispatchEvent(new FakeEvent("input"));
  assert.equal(save.disabled, false);
  save.click();
  assert.deepEqual(sent, ["aider"]);
});

test("a marked checkbox and picker reflect the global value they edit", () => {
  const pane = render([
    entry({ key: "auto_update", type: "bool", value: "false", scope: "project", global_value: "true" }),
    entry({ key: "default_program", enum: ["claude", "codex", "aider"], value: "aider", scope: "project", global_value: "codex" }),
  ]);
  assert.equal(rowPart(pane, "auto_update", "input")?.checked, true);
  const options = el(pane).querySelectorAll(`.af-config-row[data-key="default_program"] option`);
  assert.deepEqual(options.filter((o) => o.selected).map((o) => o.value), ["codex"]);
});

test("the banner names the scope the rows were read in", () => {
  const banner = (scope: ConfigScope): string | undefined =>
    el(render([entry()], scope)).querySelector(".af-config-banner")?.textContent;

  assert.equal(banner(PROJECT), "project /srv/app · values shown are what af uses there · edits write the global file");
  assert.equal(banner({ requested: "/srv/gone", root: "", error: "not a git repository: /srv/gone" }),
    "project values unavailable: not a git repository: /srv/gone — showing global values");
  assert.equal(banner({ requested: "/srv/app", root: "", error: "" }),
    "this daemon is too old to resolve project values — showing global values");
  assert.equal(banner(GLOBAL), undefined, "no project selected, no banner");
});

test("the explain note says when the trace is project-scoped", async () => {
  const resp: ExplainConfigResponse = {
    explanation: {
      key: "default_program", value: "aider", merge: "replace", precedence: ["global", "repo-shared"],
      candidates: [],
    },
    scope: "project",
    running_value_checked: false,
  };
  const pane = render([marked("project")], PROJECT, actions({ explain: async () => ({ ok: true, resp }) }));
  rowPart(pane, "default_program", ".af-config-explain-trigger")!.click();
  await new Promise((resolve) => setImmediate(resolve));
  const note = rowPart(pane, "default_program", ".af-config-explain-note")?.textContent ?? "";
  assert.match(note, / · project scope/);
});
