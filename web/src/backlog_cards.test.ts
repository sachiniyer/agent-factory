import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, FakeDataTransfer, type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import { bodyEditorLoader } from "./backlog_body.js";
import type { BacklogData } from "./backlog_data.js";
import { groupCounts, overview, positions, progressText, segments, waitsInGroup } from "./backlog_cards.js";
import { type BacklogView, type Item, listRows } from "./backlog_model.js";
import { BacklogPane, type BacklogHost } from "./backlog_pane.js";

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: { method: string; body: Record<string, unknown> }[];
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;
const NOW = Date.parse("2000-01-03T10:00:00Z");

beforeEach(() => {
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    calls.push({ method: url.replace("/v1/", ""), body: JSON.parse(String(init.body)) as Record<string, unknown> });
    return new Response(JSON.stringify({ data: {}, error: null }), { status: 200 });
  };
  bodyEditorLoader.load = async () => ({
    createDocsEditor: (parent: HTMLElement) => {
      (parent as unknown as FakeElement).append(doc.createElement("div"));
      return { getText: () => "", setText: () => {}, destroy: () => {}, applyRemote: () => {}, goToNextQuestion: () => false, focus: () => {} } as never;
    },
  });
  mock.timers.enable({ apis: ["setTimeout", "Date"], now: NOW });
});

afterEach(async () => {
  await settle();
  mock.timers.reset();
  dom.restore();
  (globalThis as { fetch: unknown }).fetch = realFetch;
  bodyEditorLoader.load = realLoad;
});

const settle = async (): Promise<void> => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

function item(id: number, over: Partial<Item> = {}): Item {
  return { id, title: `item ${id}`, status: "draft", created_at: "", updated_at: "", runs: [], waits_on: [], thread: [], ...over };
}

function view(items: Item[], states: Record<number, string> = {}): BacklogView {
  const st: BacklogView["states"] = {};
  for (const [id, s] of Object.entries(states)) st[id] = { state: s as never };
  return {
    backlog: { schema_version: 2, next_id: 100, settings: { runner: false, max_running: 2, on_done: "keep" }, items },
    states: st, runner: { state: "off", used: 0, max: 2 },
  };
}

function data(v: BacklogView): BacklogData {
  return { views: new Map([["/a", v]]), errors: new Map(), budget: null, usage: null, generation: 1 };
}

function host(): BacklogHost & { notes: string[] } {
  const h = {
    notes: [] as string[],
    token: () => "t",
    refresh: async () => true,
    started: () => 1,
    notify: (m: string) => { h.notes.push(m); },
    openSession: () => {},
    archiveSession: () => {},
    restoreSession: () => {},
    confirm: async () => true,
    loadSessions: async () => {},
  };
  return h;
}

function mount(h: BacklogHost = host()): BacklogPane {
  const pane = new BacklogPane(h);
  doc.body.append(pane.el as unknown as FakeElement);
  return pane;
}

const el = (pane: BacklogPane): FakeElement => pane.el as unknown as FakeElement;
const buttonNamed = (root: FakeElement, name: string): FakeElement | undefined =>
  root.querySelectorAll("button").find((b) => b.textContent === name && !b.hidden);
const rowIDs = (pane: BacklogPane): number[] =>
  el(pane).querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId));

const card = (pane: BacklogPane, id: number): FakeElement =>
  el(pane).querySelector(`.af-backlog-row[data-item-id="${id}"]`)!.closest(".af-backlog-group")!;
const lit = (pane: BacklogPane, cls: string): number[] =>
  el(pane).querySelectorAll(`.af-backlog-row.${cls}`).map((r) => Number(r.dataset.itemId));

// 1 { 2, 3 { 4 }, 5 }, 6, 7 { 8 }
const tree = (): Item[] => [
  item(1), item(2, { parent: 1, status: "done" }), item(3, { parent: 1 }), item(4, { parent: 3, waits_on: [2, 9] }),
  item(5, { parent: 1, waits_on: [4, 2] }), item(6), item(7, { paused: true }), item(8, { parent: 7 }), item(9),
];
const states = { 2: "done", 4: "working", 5: "needs_you", 8: "queued" };

test("segments split groups from top-level items and keep tree order", () => {
  const v = view(tree(), states);
  const segs = segments(listRows(v, { filter: "all", showDone: true, folded: new Set() }));
  assert.deepEqual(segs.map((s) => s.kind === "group" ? [s.head.item.id, s.rows.map((r) => r.item.id)] : s.rows.map((r) => r.item.id)),
    [[1, [2, 3, 4, 5]], [6], [7, [8]], [9]]);
  const group = segs[0];
  assert.ok(group.kind === "group");
  assert.deepEqual([...positions(group.rows)], [[2, 1], [3, 2], [4, 3], [5, 4]]);
});

test("group counts take leaves only, and the overview leaves out archived groups", () => {
  const v = view([...tree(), item(10, { archived: true, paused: true }), item(11, { parent: 10 })], states);
  assert.deepEqual(groupCounts(v, 1), { done: 1, total: 3, working: 1, needsYou: 1 });
  assert.equal(progressText(groupCounts(v, 1)), "1 of 3 done · 1 working · 1 needs you");
  assert.equal(progressText(groupCounts(v, 7)), "0 of 1 done");
  assert.equal(overview(v), "2 groups · 6 items · 1 working · 1 needs you");
});

test("a group is a card: header, progress, rows with place and waits, and an add row", () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  const c = card(pane, 1);
  assert.deepEqual(c.querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId)), [1, 3, 4, 5]);
  const head = c.querySelector(".af-backlog-group-head")!;
  assert.equal(Number(head.dataset.itemId), 1);
  assert.match(head.textContent, /item 1B-1○Draft1 needs youPauseDetails/);
  assert.equal(head.getAttribute("aria-describedby"), "af-backlog-progress-1");
  assert.equal(c.querySelector(".af-backlog-progress")!.id, "af-backlog-progress-1");
  assert.equal(c.querySelector(".af-backlog-progress")!.getAttribute("aria-hidden"), "true");
  assert.equal(c.querySelector(".af-backlog-group-add")!.closest('[role="tree"]'), null);
  assert.equal(c.querySelector(".af-backlog-progress")!.textContent, "1 of 3 done · 1 working · 1 needs you");
  const row5 = c.querySelector('.af-backlog-row[data-item-id="5"]')!;
  assert.equal(row5.querySelector(".af-backlog-pos")!.textContent, "#3");
  assert.equal(row5.querySelector(".af-backlog-meta")!.textContent, "B-5·after#2·waiting on 1 item");
  assert.equal(c.querySelector('.af-backlog-row[data-item-id="4"] .af-backlog-meta')!.textContent, "B-4·waiting on 1 item");
  assert.ok(c.querySelector(".af-backlog-group-add input"));
  assert.equal(c.querySelector(".af-backlog-notice"), null);
  assert.equal(el(pane).querySelector('.af-backlog-row[data-item-id="6"]')!.closest(".af-backlog-group"), null);
  assert.equal(el(pane).querySelector(".af-backlog-overview")!.textContent, "2 groups · 6 items · 1 working · 1 needs you");
});

test("a paused group shows a notice and Unpause; a folded group hides its rows and add row", async () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  const c = card(pane, 7);
  assert.match(c.querySelector(".af-backlog-notice")!.textContent, /^‖ Paused — the runner starts nothing/);
  assert.equal(c.querySelector(".af-backlog-group-head")!.getAttribute("aria-describedby"), "af-backlog-progress-7 af-backlog-paused-7");
  buttonNamed(c, "Unpause")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "UpdateBacklogItem", body: { repo_path: "/a", id: 7, paused: false } });
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
  c.querySelector(".af-backlog-fold")!.click();
  const folded = card(pane, 7);
  assert.deepEqual(folded.querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId)), [7]);
  assert.equal(folded.querySelector(".af-backlog-group-add")!.hidden, true);
  buttonNamed(folded, "Details")!.click();
  assert.ok(el(pane).querySelector(".af-backlog-panel"));
});

test("Add item makes a child of the group and keeps focus across a rerender; New group makes a top-level item", async () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  const input = card(pane, 1).querySelector(".af-backlog-group-add input")!;
  input.focus();
  input.value = "child";
  input.dispatchEvent(new FakeEvent("keydown", { key: "Enter" }));
  await settle();
  assert.deepEqual(calls.at(-1), { method: "AddBacklogItems", body: { repo_path: "/a", items: [{ key: "new", title: "child", parent: 1 }] } });
  input.value = "half typed";
  let blurs = 0;
  input.addEventListener("blur", () => { blurs++; });
  pane.update(data(view([...tree(), item(12, { parent: 1 })], states)), "/a", []);
  assert.equal(card(pane, 1).querySelector(".af-backlog-group-add input"), input);
  assert.equal(doc.activeElement, input);
  assert.equal(blurs, 0);
  assert.equal(input.value, "half typed");
  const group = el(pane).querySelector('input[aria-label="New group title"]')!;
  group.value = "Docs refresh";
  group.dispatchEvent(new FakeEvent("keydown", { key: "Enter" }));
  await settle();
  assert.deepEqual(calls.at(-1), { method: "AddBacklogItems", body: { repo_path: "/a", items: [{ key: "new", title: "Docs refresh" }] } });
});

test("a selected row lights up what it waits on and what waits on it, without dimming the rest", () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  pane.select(4);
  assert.deepEqual(lit(pane, "af-backlog-row-related"), [5, 9]);
  assert.equal(el(pane).querySelector(".af-backlog-dim"), null);
});

test("a header row shows its state chip and Blocks n, and b picks what waits on the group", () => {
  const pane = mount();
  pane.update(data(view([...tree(), item(10, { waits_on: [1], pr: "https://github.com/o/r/pull/3" })], states)), "/a", []);
  const head = card(pane, 1).querySelector(".af-backlog-group-head")!;
  assert.equal(head.querySelector(".af-backlog-blocks")!.textContent, "Blocks 1");
  assert.equal(head.getAttribute("aria-keyshortcuts"), "b");
  head.focus();
  head.dispatchEvent(new FakeEvent("keydown", { key: "b" }));
  assert.deepEqual(lit(pane, "af-backlog-row-picked"), [1, 10]);
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
});

test("waits count derived states: a done parent clears, a dropped item still blocks and is marked", () => {
  // 20 { 21 dropped, 22 { 23 }, 24 }
  const items = [item(20), item(21, { parent: 20, status: "dropped" }), item(22, { parent: 20 }), item(23, { parent: 22, status: "done" }),
    item(24, { parent: 20, waits_on: [21, 22, 99] })];
  const v = view(items, { 21: "dropped", 22: "done", 23: "done" });
  assert.deepEqual(waitsInGroup(v, items[4], new Map([[21, 1], [22, 2]])),
    { open: 1, dropped: 1, after: [{ pos: 1, dropped: true }, { pos: 2, dropped: false }] });
  const pane = mount();
  pane.update(data(v), "/a", []);
  assert.equal(card(pane, 20).querySelector('.af-backlog-row[data-item-id="24"] .af-backlog-meta')!.textContent,
    "B-24·waiting on 1 item (1 dropped)");
  buttonNamed(el(pane), "Show done")!.click();
  assert.equal(card(pane, 20).querySelector('.af-backlog-row[data-item-id="24"] .af-backlog-meta')!.textContent,
    "B-24·after#1 dropped#2·waiting on 1 item (1 dropped)");
});

test("a filter keeps group counts whole and numbers only the rows it shows", () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  buttonNamed(el(pane), "Working")!.click();
  const c = card(pane, 1);
  assert.deepEqual(c.querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId)), [1, 3, 4]);
  assert.equal(c.querySelector(".af-backlog-progress")!.textContent, "1 of 3 done · 1 working · 1 needs you");
  assert.equal(c.querySelector('.af-backlog-row[data-item-id="4"] .af-backlog-pos')!.textContent, "#2");
  assert.equal(el(pane).querySelector(".af-backlog-overview")!.textContent, "2 groups · 6 items · 1 working · 1 needs you");
});

test("an archived group is a card with Unarchive and no notice or add row", async () => {
  const pane = mount();
  pane.update(data(view([...tree(), item(10, { archived: true, paused: true }), item(11, { parent: 10 })], states)), "/a", []);
  assert.equal(el(pane).querySelector('.af-backlog-row[data-item-id="10"]'), null);
  buttonNamed(el(pane), "Show archived")!.click();
  const c = card(pane, 10);
  assert.match(c.querySelector(".af-backlog-group-head")!.textContent, /Archived/);
  assert.equal(c.querySelector(".af-backlog-notice"), null);
  assert.equal(c.querySelector(".af-backlog-group-add")!.hidden, true);
  assert.equal(buttonNamed(c, "Pause"), undefined);
  buttonNamed(c, "Unarchive")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "UpdateBacklogItem", body: { repo_path: "/a", id: 10, archived: false } });
});

test("Alt+arrows and drag reorder inside a card", async () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  const ids = (): number[] => card(pane, 1).querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId));
  card(pane, 1).querySelector('.af-backlog-row[data-item-id="3"]')!.dispatchEvent(new FakeEvent("keydown", { key: "ArrowDown", altKey: true }));
  assert.deepEqual(ids(), [1, 5, 3, 4]);
  await settle();
  assert.deepEqual(calls.at(-1), { method: "MoveBacklogItem", body: { repo_path: "/a", id: 3, after: 5 } });
  const dt = new FakeDataTransfer();
  const row = (id: number): FakeElement => card(pane, 1).querySelector(`.af-backlog-row[data-item-id="${id}"]`)!;
  row(5).dispatchEvent(new FakeEvent("dragstart", { dataTransfer: dt }));
  const over = new FakeEvent("dragover", { dataTransfer: dt, clientY: 100 });
  row(4).dispatchEvent(over);
  assert.equal(over.defaultPrevented, true);
  row(4).dispatchEvent(new FakeEvent("drop", { dataTransfer: dt, clientY: 100 }));
  await settle();
  assert.deepEqual(calls.at(-1), { method: "MoveBacklogItem", body: { repo_path: "/a", id: 5, after: 4 } });
});

test("arrow keys cross card edges in tree order", () => {
  const pane = mount();
  pane.update(data(view(tree(), states)), "/a", []);
  const row = (id: number): FakeElement => el(pane).querySelector(`.af-backlog-row[data-item-id="${id}"]`)!;
  const key = (id: number, k: string): void => { row(id).dispatchEvent(new FakeEvent("keydown", { key: k })); };
  row(5).focus();
  key(5, "ArrowDown");
  assert.equal(doc.activeElement, row(6));
  key(6, "ArrowUp");
  assert.equal(doc.activeElement, row(5));
  key(5, "ArrowLeft");
  assert.equal(doc.activeElement, row(1));
  key(1, "ArrowRight");
  assert.equal(doc.activeElement, row(3));
  row(6).focus();
  key(6, "ArrowDown");
  assert.equal(doc.activeElement, row(7));
  assert.deepEqual(el(pane).querySelectorAll(".af-backlog-row").filter((r) => r.tabIndex === 0).map((r) => Number(r.dataset.itemId)), [7]);
});
