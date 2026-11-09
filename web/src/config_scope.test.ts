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

// A project read must never cost a row: the web always has a selected project,
// so a key that vanished in project scope would vanish for everyone. Every tier,
// every control kind, marked and unmarked rows alike — including an unknown
// scope — renders, and the set of rows equals the global render's.
test("every key renders in project scope, exactly as in global scope", () => {
  const rows: ConfigEntry[] = [
    entry({ key: "default_program", tier: 1, enum: ["claude", "codex"], value: "codex", scope: "project", global_value: "claude" }),
    entry({ key: "auto_update", tier: 1, type: "bool", value: "true" }),
    entry({ key: "appearance", tier: 1, enum: ["light", "dark", "system"], value: "system" }),
    entry({ key: "vscode_server_binary", tier: 2, tier_name: "common", value: "" }),
    entry({ key: "program_overrides", tier: 2, tier_name: "common", type: "table", value: "{}", scope: "project+personal", global_value: "{}" }),
    entry({ key: "branch_prefix", tier: 3, tier_name: "advanced", value: "af/", scope: "personal-ignored", global_value: "af/" }),
    entry({ key: "on_archive_command", tier: 3, tier_name: "advanced", value: "x", scope: "team", global_value: "" }),
    entry({ key: "network.listen_addr", tier: 3, tier_name: "advanced", value: "127.0.0.1:8443" }),
  ];
  const rendered = (scope: ConfigScope): string[] => {
    const pane = render(rows, scope);
    // The advanced tier folds by default; open it so every tier is in the DOM.
    el(pane).querySelector(".af-config-toggle")!.click();
    return el(pane).querySelectorAll(".af-config-list .af-config-row[data-key]").map((r) => r.getAttribute("data-key") ?? "");
  };
  const project = rendered(PROJECT);
  assert.deepEqual(project, rows.map((e) => e.key), "every key, in manifest order, in project scope");
  assert.deepEqual(project, rendered(GLOBAL), "a project read renders the same rows as the global read");
  for (const scope of [{ requested: "/srv/gone", root: "", error: "not a git repository: /srv/gone" }, { requested: "/srv/app", root: "", error: "" }]) {
    assert.deepEqual(rendered(scope), project, "a degraded or too-old read keeps every row too");
  }
});
