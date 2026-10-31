import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, FakeDataTransfer, type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import { bodyEditorLoader } from "./backlog_body.js";
import { resetSerialState } from "./backlog_serial.js";
import type { BacklogData } from "./backlog_data.js";
import type { BacklogView, Budget, Item } from "./backlog_model.js";
import { BacklogPane, type BacklogHost, runnerText } from "./backlog_pane.js";

interface Call {
  method: string;
  body: Record<string, unknown>;
}

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: Call[];
let reply: (method: string) => Promise<unknown>;
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;

interface FakeEditor {
  el: FakeElement;
  text: string;
  change(text: string): void;
}
let editors: FakeEditor[];

beforeEach(() => {
  resetSerialState();
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  editors = [];
  reply = async () => ({});
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    const method = url.replace("/v1/", "");
    calls.push({ method, body: JSON.parse(String(init.body)) });
    let data: unknown;
    try {
      data = await reply(method);
    } catch (e) {
      return new Response(JSON.stringify({ data: null, error: { message: (e as Error).message } }), { status: 400 });
    }
    return new Response(JSON.stringify({ data, error: null }), { status: 200 });
  };
  bodyEditorLoader.load = async () => ({
    createDocsEditor: (parent: HTMLElement, opts: { text: string; onChange(t: string): void }) => {
      const el = doc.createElement("div");
      el.tabIndex = 0;
      (parent as unknown as FakeElement).append(el);
      const ed: FakeEditor = { el, text: opts.text, change: (t) => { ed.text = t; opts.onChange(t); } };
      editors.push(ed);
      return {
        getText: () => ed.text, setText: (t: string) => { ed.text = t; }, destroy: () => {},
        applyRemote: () => {}, goToNextQuestion: () => false, focus: () => el.focus(),
      } as never;
    },
  });
  mock.timers.enable({ apis: ["setTimeout"] });
});

afterEach(() => {
  mock.timers.reset();
  dom.restore();
  (globalThis as { fetch: unknown }).fetch = realFetch;
  bodyEditorLoader.load = realLoad;
});

const settle = (): Promise<void> => new Promise((r) => setImmediate(r));

function item(id: number, over: Partial<Item> = {}): Item {
  return { id, title: `item ${id}`, status: "draft", created_at: "", updated_at: "", runs: [], waits_on: [], thread: [], ...over };
}

function view(...items: Item[]): BacklogView {
  return {
    backlog: { schema_version: 1, next_id: 100, settings: { runner: false, max_running: 1, on_done: "keep" }, items },
    states: {}, runner: { state: "idle", used: 0, max: 1 },
  };
}

function data(views: Record<string, BacklogView>, generation = 1): BacklogData {
  return { views: new Map(Object.entries(views)), errors: new Map(), budget: null, generation };
}

function host(over: Partial<BacklogHost> = {}): BacklogHost & { notices: string[]; refreshes: number } {
  const h = {
    notices: [] as string[],
    refreshes: 0,
    token: () => "t",
    refresh: async () => { h.refreshes++; return true; },
    started: () => 0,
    notify: (m: string) => { h.notices.push(m); },
    openSession: () => {},
    archiveSession: () => {},
    restoreSession: () => {},
    loadSessions: async () => {},
    ...over,
  };
  return h;
}

function mount(h: BacklogHost): BacklogPane {
  const pane = new BacklogPane(h);
  doc.body.append(pane.el as unknown as FakeElement);
  return pane;
}

const el = (pane: BacklogPane): FakeElement => pane.el as unknown as FakeElement;
const rows = (pane: BacklogPane): FakeElement[] => el(pane).querySelectorAll(".af-backlog-row");
const rowIds = (pane: BacklogPane): number[] => rows(pane).map((r) => Number(r.dataset.itemId));
const row = (pane: BacklogPane, id: number): FakeElement => el(pane).querySelector(`.af-backlog-row[data-item-id="${id}"]`)!;
const key = (target: FakeElement, k: string, init: Partial<FakeEvent> = {}): void => {
  target.dispatchEvent(new FakeEvent("keydown", { key: k, ...init }));
};

test("runner text keeps the reason second, without its full stop", () => {
  const v = view();
  v.runner = { state: "off", reason: "Items run only when you start them.", used: 0, max: 1 };
  const budget: Budget = { share: 30, stop_at: 80, max_per_run: 10, usd_limit: 0, usd_spent: 0 };
  const r = runnerText(v, budget);
  assert.equal([r.state, ...r.parts].join(" · "),
    "Runner off · Items run only when you start them · 0 of 1 working · Backlog share 30% · stop at plan 80%");
  v.runner = { state: "stopped", reason: "Usage limit until 14:00", used: 1, max: 2 };
  const paused = runnerText(v, { ...budget, usd_limit: 5, usd_spent: 1.5 });
  assert.equal(paused.state, "Runner paused");
  assert.equal(paused.warn, true);
  assert.deepEqual(paused.parts, ["Usage limit until 14:00", "1 of 2 working", "Backlog share 30% · stop at plan 80%", "$1.50 of $5.00"]);
  assert.deepEqual(runnerText(view(), null).parts, ["0 of 1 working"]);
});

test("a project switch saves an unsaved body to the item's own project", async () => {
  const pane = mount(host());
  const d = data({ "/a": view(item(1, { body: "a body" })), "/b": view(item(1, { body: "b body" })) });
  pane.update(d, "/a", []);
  pane.select(1);
  await settle();
  editors[0].change("edited in a");
  pane.update(d, "/b", []);
  const saves = calls.filter((c) => c.method === "UpdateBacklogItem");
  assert.equal(saves.length, 1);
  assert.equal(saves[0].body.repo_path, "/a");
  assert.equal(saves[0].body.body, "edited in a");
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
});

test("the item chip's select waits for the project switch", () => {
  const pane = mount(host());
  const d = data({ "/a": view(item(1)), "/b": view(item(7)) });
  pane.update(d, "/a", []);
  pane.select(7, "/b");
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
  pane.update(d, "/b", []);
  assert.equal(el(pane).querySelector(".af-backlog-panel")?.getAttribute("aria-label"), "Item B-7");
});

test("a rerender keeps the body editor attached and focused", async () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { body: "x" }), item(2)) }), "/a", []);
  pane.select(1);
  await settle();
  const editor = editors[0].el;
  editor.focus();
  pane.update(data({ "/a": view(item(1, { body: "x", title: "renamed" }), item(2)) }, 2), "/a", []);
  assert.equal(doc.activeElement, editor);
  assert.equal(editor.isConnected, true);
});

test("the body saves after a pause, and a stored body does not overwrite an edit", async () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { body: "old" })) }), "/a", []);
  pane.select(1);
  await settle();
  editors[0].change("typing");
  pane.update(data({ "/a": view(item(1, { body: "from agent" })) }, 2), "/a", []);
  assert.equal(editors[0].text, "typing");
  mock.timers.tick(799);
  assert.equal(calls.filter((c) => c.method === "UpdateBacklogItem").length, 0);
  mock.timers.tick(1);
  const saves = calls.filter((c) => c.method === "UpdateBacklogItem");
  assert.deepEqual(saves.map((c) => c.body.body), ["typing"]);
  await settle();   // the save and the refresh after it have landed: the edit is acknowledged
  pane.update(data({ "/a": view(item(1, { body: "from agent" })) }, 3), "/a", []);
  assert.equal(editors[0].text, "from agent");
});

test("a refresh that read the old body mid-save does not revert the edit", async () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { body: "old" })) }), "/a", []);
  pane.select(1);
  await settle();
  let finish: () => void = () => {};
  reply = (method) => method === "UpdateBacklogItem" ? new Promise((r) => { finish = () => r({}); }) : Promise.resolve({});
  editors[0].change("typed");
  mock.timers.tick(800);                      // the debounce fires: the save is in flight
  pane.update(data({ "/a": view(item(1, { body: "old" })) }, 2), "/a", []);   // a refresh that started before it
  assert.equal(editors[0].text, "typed", "the timer is gone but the edit is not acknowledged");
  pane.select(1);
  assert.equal(editors[0].text, "typed", "re-rendering the panel for another reason keeps it too");
  finish();
  await settle();
  pane.update(data({ "/a": view(item(1, { body: "typed" })) }, 3), "/a", []);
  assert.equal(editors[0].text, "typed");
  pane.update(data({ "/a": view(item(1, { body: "from agent" })) }, 4), "/a", []);
  assert.equal(editors[0].text, "from agent", "acknowledged: the stored body shows again");
});

test("a failed body save keeps the edit instead of reverting to the stored body", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data({ "/a": view(item(1, { body: "old" })) }), "/a", []);
  pane.select(1);
  await settle();
  reply = async (method) => { if (method === "UpdateBacklogItem") throw new Error("daemon said no"); return {}; };
  editors[0].change("kept");
  mock.timers.tick(800);
  await settle();
  assert.deepEqual(h.notices, ["daemon said no"]);
  pane.update(data({ "/a": view(item(1, { body: "old" })) }, 2), "/a", []);
  assert.equal(editors[0].text, "kept");
});

function runItem(sessionID: string): Item {
  return item(1, { runs: [{ session_id: sessionID, state: "active", started_by: "user", started_at: "", title: "run" }] as never });
}
const runLine = (pane: BacklogPane): string => el(pane).querySelector(".af-backlog-run")?.textContent ?? "";

test("a run session the rail has not loaded is looked up before it is called Gone", async () => {
  const asked: string[][] = [];
  let answer: () => void = () => {};
  const h = host({ loadSessions: (ids) => { asked.push(ids); return new Promise<void>((r) => { answer = r; }); } });
  const pane = mount(h);
  pane.update(data({ "/a": view(runItem("s-arch")) }), "/a", []);
  pane.select(1);
  assert.deepEqual(asked, [["s-arch"]]);
  assert.match(runLine(pane), /Checking…/);
  assert.doesNotMatch(runLine(pane), /Gone/);
  pane.select(1);                      // another render does not ask again
  assert.equal(asked.length, 1);
  answer();
  await settle();
  assert.match(runLine(pane), /Gone/, "asked and still absent: the daemon does not know it");
});

test("a run session that was adopted into the list gets Open and is not asked about again", async () => {
  const asked: string[][] = [];
  const pane = mount(host({ loadSessions: async (ids) => { asked.push(ids); } }));
  pane.update(data({ "/a": view(runItem("s-arch")) }), "/a", []);
  pane.select(1);
  await settle();
  pane.update(data({ "/a": view(runItem("s-arch")) }, 2), "/a",
    [{ id: "s-arch", title: "t", liveness: "archived" } as never]);
  assert.match(runLine(pane), /Open/);
  assert.doesNotMatch(runLine(pane), /Gone/);
  assert.equal(asked.length, 1);
});

test("a lookup that cannot be answered says so instead of Gone", async () => {
  const pane = mount(host({ loadSessions: async () => { throw new Error("offline"); } }));
  pane.update(data({ "/a": view(runItem("s-x")) }), "/a", []);
  pane.select(1);
  await settle();
  assert.match(runLine(pane), /Unavailable/);
  assert.doesNotMatch(runLine(pane), /Gone/);
});

test("opening an item focuses its panel; Escape closes it and returns focus to the row", () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1), item(2)) }), "/a", []);
  key(row(pane, 2), "Enter");
  const panel = el(pane).querySelector(".af-backlog-panel")!;
  assert.equal(doc.activeElement, panel.querySelector(".af-backlog-close"));
  key(doc.activeElement, "Escape");
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
  assert.equal(doc.activeElement, row(pane, 2));
  row(pane, 1).focus();
  row(pane, 1).click();
  assert.equal(doc.activeElement, el(pane).querySelector(".af-backlog-close"));
});

test("the tree has one tab stop and ARIA tree keys", () => {
  const pane = mount(host());
  // 1 { 2 { 3 } }, 4
  pane.update(data({ "/a": view(item(1), item(2, { parent: 1 }), item(3, { parent: 2 }), item(4)) }), "/a", []);
  const stops = (): number[] => rows(pane).filter((r) => r.tabIndex === 0).map((r) => Number(r.dataset.itemId));
  assert.deepEqual(stops(), [1]);
  row(pane, 1).focus();
  key(row(pane, 1), "ArrowRight");
  assert.equal(doc.activeElement, row(pane, 2));
  assert.deepEqual(stops(), [2]);
  key(row(pane, 2), "ArrowLeft");
  assert.deepEqual(rowIds(pane), [1, 2, 4]);
  assert.equal(row(pane, 2).getAttribute("aria-expanded"), "false");
  assert.equal(doc.activeElement, row(pane, 2));
  key(row(pane, 2), "ArrowLeft");
  assert.equal(doc.activeElement, row(pane, 1));
  key(row(pane, 1), "ArrowRight");
  key(row(pane, 1), "ArrowDown");
  key(row(pane, 2), "ArrowRight");
  assert.deepEqual(rowIds(pane), [1, 2, 3, 4]);
  key(row(pane, 2), "End");
  assert.equal(doc.activeElement, row(pane, 4));
  key(row(pane, 4), "Home");
  assert.equal(doc.activeElement, row(pane, 1));
  assert.deepEqual(stops(), [1]);
});

test("a reorder shows at once and survives refreshes started before it committed", async () => {
  let started = 4;
  const h = host({ started: () => started });
  const pane = mount(h);
  const before = data({ "/a": view(item(1), item(2)) }, 4);
  pane.update(before, "/a", []);
  key(row(pane, 1), "ArrowDown", { altKey: true });
  assert.deepEqual(rowIds(pane), [2, 1]);
  pane.update(data({ "/a": view(item(1), item(2)) }, 4), "/a", []);
  assert.deepEqual(rowIds(pane), [2, 1]);
  started = 5;
  await settle();
  const move = calls.find((c) => c.method === "MoveBacklogItem");
  assert.deepEqual(move?.body, { repo_path: "/a", id: 1, after: 2 });
  assert.equal(h.refreshes, 1);
  pane.update(data({ "/a": view(item(1), item(2)) }, 5), "/a", []);
  assert.deepEqual(rowIds(pane), [2, 1]);
  pane.update(data({ "/a": view(item(2), item(1)) }, 6), "/a", []);
  assert.deepEqual(rowIds(pane), [2, 1]);
  pane.update(data({ "/a": view(item(1), item(2)) }, 7), "/a", []);
  assert.deepEqual(rowIds(pane), [1, 2]);
});

test("a refused reorder snaps back and says why", async () => {
  reply = async (method) => {
    if (method === "MoveBacklogItem") throw new Error("B-1 cannot move there");
    return {};
  };
  const h = host();
  const pane = mount(h);
  pane.update(data({ "/a": view(item(1), item(2)) }), "/a", []);
  key(row(pane, 1), "ArrowDown", { altKey: true });
  assert.deepEqual(rowIds(pane), [2, 1]);
  await settle();
  assert.deepEqual(rowIds(pane), [1, 2]);
  assert.match(h.notices[0], /cannot move there/);
});

test("a selected project with no backlog yet asks for one refresh", () => {
  const h = host();
  const pane = mount(h);
  pane.update(data({}), "/a", []);
  assert.match(el(pane).textContent, /Loading backlog…/);
  pane.update(data({}, 2), "/a", []);
  assert.equal(h.refreshes, 1);
});

test("the Move… menu stays open across a rerender", () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1), item(2)) }), "/a", []);
  pane.select(1);
  const details = el(pane).querySelector("details.af-backlog-move")!;
  details.open = true;
  details.dispatchEvent(new FakeEvent("toggle", {}, false));
  const select = details.querySelector('select[aria-label="Above…"]')!;
  select.focus();
  pane.update(data({ "/a": view(item(1), item(2, { title: "two" })) }, 2), "/a", []);
  const next = el(pane).querySelector("details.af-backlog-move")!;
  assert.notEqual(next, details);
  assert.equal(next.open, true);
  assert.equal(doc.activeElement, next.querySelector('select[aria-label="Above…"]'));
});

test("the pull request link renders only for http and https URLs", () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { pr: "javascript:alert(1)" }), item(2, { pr: "https://github.com/o/r/pull/1" })) }), "/a", []);
  pane.select(1);
  assert.equal(el(pane).querySelector(".af-backlog-pr"), null);
  pane.select(2);
  assert.equal(el(pane).querySelector(".af-backlog-pr")?.href, "https://github.com/o/r/pull/1");
});

test("a drag ends at the list, and a foreign drag is not a reorder", () => {
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1), item(2)) }), "/a", []);
  const dt = new FakeDataTransfer();
  row(pane, 1).dispatchEvent(new FakeEvent("dragstart", { dataTransfer: dt }));
  const over = new FakeEvent("dragover", { dataTransfer: dt, clientY: 18 });
  row(pane, 2).dispatchEvent(over);
  assert.equal(over.defaultPrevented, true);
  el(pane).querySelector(".af-backlog-list")!.dispatchEvent(new FakeEvent("drop", { dataTransfer: dt }));
  const foreign = new FakeDataTransfer();
  foreign.setData("text/plain", "a file");
  const stray = new FakeEvent("dragover", { dataTransfer: foreign, clientY: 18 });
  row(pane, 2).dispatchEvent(stray);
  assert.equal(stray.defaultPrevented, false);
  row(pane, 2).classList.add("af-backlog-drop-after");
  const inner = row(pane, 2).querySelector(".af-backlog-row-title")!;
  row(pane, 2).dispatchEvent(new FakeEvent("dragleave", { relatedTarget: inner }));
  assert.equal(row(pane, 2).classList.contains("af-backlog-drop-after"), true);
  row(pane, 2).dispatchEvent(new FakeEvent("dragleave", { relatedTarget: null }));
  assert.equal(row(pane, 2).classList.contains("af-backlog-drop-after"), false);
});

test("reorders go out one at a time, in the order they were made", async () => {
  const finish: (() => void)[] = [];
  reply = (method) => method === "MoveBacklogItem" ? new Promise((r) => { finish.push(() => r({})); }) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1), item(2), item(3)) }), "/a", []);
  key(row(pane, 3), "ArrowUp", { altKey: true });
  key(row(pane, 3), "ArrowUp", { altKey: true });
  assert.deepEqual(rowIds(pane), [3, 1, 2], "both moves show at once");
  await settle();
  const moves = (): Call[] => calls.filter((c) => c.method === "MoveBacklogItem");
  assert.equal(moves().length, 1, "the second request waits for the first");
  finish[0]();
  await settle();
  assert.equal(moves().length, 2);
  assert.notDeepEqual(moves()[0].body, moves()[1].body);
  finish[1]();
  await settle();
});

const titleInput = (pane: BacklogPane): FakeElement => el(pane).querySelector(".af-backlog-title-input")!;
function submitTitle(pane: BacklogPane, value: string): void {
  const input = titleInput(pane);
  input.value = value;
  input.dispatchEvent(new FakeEvent("change"));
}

test("a refresh that read the old item does not put the old title back over an unacknowledged rename", async () => {
  let finish: () => void = () => {};
  reply = (method) => method === "UpdateBacklogItem" ? new Promise((r) => { finish = () => r({}); }) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { title: "old" })) }), "/a", []);
  pane.select(1);
  submitTitle(pane, "new");
  pane.update(data({ "/a": view(item(1, { title: "old" })) }, 2), "/a", []);
  assert.equal(titleInput(pane).value, "new", "the stale snapshot does not revert the field");
  submitTitle(pane, "new revised");
  finish();
  await settle();
  assert.equal(calls.filter((c) => c.method === "UpdateBacklogItem").at(-1)?.body.title, "new revised",
    "the next edit builds on the draft, not the reverted text");
  finish();
  await settle();
  pane.update(data({ "/a": view(item(1, { title: "new revised" })) }, 3), "/a", []);
  assert.equal(titleInput(pane).value, "new revised");
});

test("a refused rename gives the field back its stored title", async () => {
  reply = async (method) => { if (method === "UpdateBacklogItem") throw new Error("no"); return {}; };
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { title: "old" })) }), "/a", []);
  pane.select(1);
  submitTitle(pane, "rejected");
  await settle();
  assert.equal(titleInput(pane).value, "old");
});

const updates = (): Call[] => calls.filter((c) => c.method === "UpdateBacklogItem");

test("title saves are serialized: a second rename waits for the first and goes out as the latest", async () => {
  const finish: (() => void)[] = [];
  reply = (method) => method === "UpdateBacklogItem" ? new Promise((r) => { finish.push(() => r({})); }) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { title: "old" })) }), "/a", []);
  pane.select(1);
  submitTitle(pane, "new");
  submitTitle(pane, "new revised");
  submitTitle(pane, "new revised again");
  assert.deepEqual(updates().map((c) => c.body.title), ["new"], "one request in flight");
  finish[0]();
  await settle();
  assert.deepEqual(updates().map((c) => c.body.title), ["new", "new revised again"], "one follow-up with the latest title");
  assert.equal(titleInput(pane).value, "new revised again");
  finish[1]();
  await settle();
});

test("removing two dependencies before the first write lands removes both", async () => {
  const finish: (() => void)[] = [];
  reply = (method) => method === "UpdateBacklogItem" ? new Promise((r) => { finish.push(() => r({})); }) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { waits_on: [2, 3] }), item(2), item(3)) }), "/a", []);
  pane.select(1);
  const remove = (n: number): void => {
    el(pane).querySelector(`button[aria-label="Remove B-${n}"]`)!.click();
  };
  remove(2);
  remove(3);                                   // built from the pending list, not the stored [2, 3]
  assert.deepEqual(updates().map((c) => c.body.waits_on), [[3]]);
  finish[0]();
  await settle();
  assert.deepEqual(updates().map((c) => c.body.waits_on), [[3], []], "the second removal is sent from the first's result");
  finish[1]();
  await settle();
});

test("a refused note comes back to the box, ahead of anything typed since", async () => {
  reply = async (method) => { if (method === "PostBacklogMessage") throw new Error("busy"); return {}; };
  const h = host();
  const pane = mount(h);
  pane.update(data({ "/a": view(item(1)) }), "/a", []);
  pane.select(1);
  const note = el(pane).querySelector('textarea[aria-label="Note"]')!;
  const addNote = (): void => {
    [...el(pane).querySelectorAll("button")].find((b) => b.textContent === "Add note")!.click();
  };
  note.value = "a long investigation";
  addNote();
  assert.equal(note.value, "", "cleared on submit");
  note.value = "typed meanwhile";
  await settle();
  assert.equal(note.value, "a long investigation\ntyped meanwhile");
  assert.deepEqual(h.notices, ["busy"]);
});

test("an edit is not acknowledged by a refresh that failed to read the project", async () => {
  let readOk = false;
  const h = host({ refresh: async () => readOk });
  const pane = mount(h);
  pane.update(data({ "/a": view(item(1, { body: "old" })) }), "/a", []);
  pane.select(1);
  await settle();
  editors[0].change("saved");
  mock.timers.tick(800);
  await settle();                                   // UpdateBacklogItem succeeded; the read after it failed
  pane.update(data({ "/a": view(item(1, { body: "old" })) }, 2), "/a", []);   // the cached snapshot renders
  assert.equal(editors[0].text, "saved", "the draft guard is kept until a read carries the saved value");
  readOk = true;
  editors[0].change("saved again");
  mock.timers.tick(800);
  await settle();
  pane.update(data({ "/a": view(item(1, { body: "from agent" })) }, 3), "/a", []);
  assert.equal(editors[0].text, "from agent", "a confirmed save releases the guard");
});

test("a dependency list submitted before the panel was closed is the base for the reopened panel's next change", async () => {
  const finish: (() => void)[] = [];
  reply = (method) => method === "UpdateBacklogItem" ? new Promise((r) => { finish.push(() => r({})); }) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { waits_on: [2, 3] }), item(2), item(3)) }), "/a", []);
  pane.select(1);
  el(pane).querySelector('button[aria-label="Remove B-2"]')!.click();
  pane.select(2);                                      // close it and look at another item
  pane.select(1);                                      // reopen: a new panel, the write still in flight
  assert.equal(el(pane).querySelector('button[aria-label="Remove B-2"]'), null, "the removal still shows");
  el(pane).querySelector('button[aria-label="Remove B-3"]')!.click();
  finish[0]();
  await settle();
  assert.deepEqual(updates().map((c) => c.body.waits_on), [[3], []], "the second removal builds on the first, not on the stored list");
  finish[1]?.();
  await settle();
});

test("a body save held across a panel reopening shows the saved text, not the stored one", async () => {
  reply = (method) => method === "UpdateBacklogItem" ? new Promise(() => {}) : Promise.resolve({});
  const pane = mount(host());
  pane.update(data({ "/a": view(item(1, { body: "old" }), item(2)) }), "/a", []);
  pane.select(1);
  await settle();
  editors[0].change("typed");
  mock.timers.tick(800);                               // the save is in flight and never lands
  pane.select(2);
  pane.select(1);
  await settle();
  assert.equal(editors.at(-1)!.text, "typed");
});
