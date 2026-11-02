import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import { bodyEditorLoader } from "./backlog_body.js";
import type { BacklogData } from "./backlog_data.js";
import { type BacklogView, type Item, type RunRef, blocking, latestRun, relatedIDs } from "./backlog_model.js";
import { BacklogPane, type BacklogHost } from "./backlog_pane.js";
import { PREVIEW_DELAY_MS } from "./backlog_preview.js";
import { Liveness, type SessionData } from "./types.js";

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: { method: string; body: Record<string, unknown> }[];
let reply: (method: string) => Promise<unknown>;
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;
const NOW = Date.parse("2000-01-03T10:00:00Z");

beforeEach(() => {
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  reply = async () => ({});
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    const method = url.replace("/v1/", "");
    calls.push({ method, body: JSON.parse(String(init.body)) as Record<string, unknown> });
    try {
      return new Response(JSON.stringify({ data: await reply(method), error: null }), { status: 200 });
    } catch (e) {
      return new Response(JSON.stringify({ data: null, error: { message: (e as Error).message } }), { status: 400 });
    }
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

function run(sid: string, over: Partial<RunRef> = {}): RunRef {
  return { session_id: sid, title: `run ${sid}`, state: "started", prev_status: "queued", started_by: "runner",
    started_at: new Date(NOW - 12 * 60_000).toISOString(), ...over };
}

function view(items: Item[], states: Record<number, string> = {}): BacklogView {
  const st: BacklogView["states"] = {};
  for (const [id, s] of Object.entries(states)) st[id] = { state: s as never };
  return {
    backlog: { schema_version: 1, next_id: 100, settings: { runner: false, max_running: 2, on_done: "keep" }, items },
    states: st, runner: { state: "off", used: 0, max: 2 },
  };
}

function data(v: BacklogView): BacklogData {
  return { views: new Map([["/a", v]]), errors: new Map(), budget: null, usage: null, generation: 1 };
}

function host(): BacklogHost & { opened: string[]; archived: string[]; restored: string[] } {
  const h = {
    opened: [] as string[],
    archived: [] as string[],
    restored: [] as string[],
    token: () => "t",
    refresh: async () => true,
    started: () => 1,
    notify: () => {},
    openSession: (id: string) => { h.opened.push(id); },
    archiveSession: (id: string) => { h.archived.push(id); },
    restoreSession: (id: string) => { h.restored.push(id); },
    loadSessions: async () => {},
    confirm: async () => true,
  };
  return h;
}

function mount(h: BacklogHost): BacklogPane {
  const pane = new BacklogPane(h);
  doc.body.append(pane.el as unknown as FakeElement);
  return pane;
}

const el = (pane: BacklogPane): FakeElement => pane.el as unknown as FakeElement;
const row = (pane: BacklogPane, id: number): FakeElement => el(pane).querySelector(`.af-backlog-row[data-item-id="${id}"]`)!;
const session = (id: string, over: Partial<SessionData> = {}): SessionData =>
  ({ id, title: id, status: "Ready", branch: `af/${id}`, ...over } as unknown as SessionData);
const buttonNamed = (root: FakeElement, name: string): FakeElement | undefined =>
  root.querySelectorAll("button").find((b) => b.textContent === name && !b.hidden);
const lit = (pane: BacklogPane, cls: string): number[] =>
  el(pane).querySelectorAll(`.af-backlog-row.${cls}`).map((r) => Number(r.dataset.itemId));

test("model: blocks counts open waiters; related joins waits-on and needed-by", () => {
  const v = view([item(1), item(2, { waits_on: [1] }), item(3, { waits_on: [1], status: "done" }), item(4, { waits_on: [2, 99] }),
    // A parent whose children all finished derives done while its stored status still reads running (Codex P2 on #6544).
    item(5, { waits_on: [1], status: "running" }), item(6, { parent: 5, status: "done" })],
  { 2: "draft", 3: "done", 4: "draft", 5: "done", 6: "done" });
  assert.deepEqual(blocking(v, 1).map((i) => i.id), [2], "closed by derived state, not stored status: the list hides them too");
  assert.deepEqual([...relatedIDs(v.backlog, 2)].sort(), [1, 4]);
  assert.deepEqual([...relatedIDs(v.backlog, 4)], [2]);
  assert.equal(latestRun(item(5, { runs: [run("a"), run("", { state: "starting" })] }))?.state, "starting");
});

test("the state chip opens the latest run's session and does not select the row", () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1, { status: "running", runs: [run("old"), run("new")] })], { 1: "working" })), "/a",
    [session("old"), session("new")]);
  const chip = row(pane, 1).querySelector(".af-backlog-chip-link")!;
  assert.equal(chip.textContent, "●Working · 12m↗");
  chip.click();
  assert.deepEqual(h.opened, ["new"]);
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
});

test("a chip is plain with no run, or while the run's session is not listed", () => {
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { runs: [run("s2")] })], { 2: "working" })), "/a", []);
  assert.equal(el(pane).querySelector(".af-backlog-chip-link"), null);
});

test("an archived session's chip says so and asks for no preview", () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { status: "running", runs: [run("s1")] })], { 1: "needs_you" })), "/a",
    [session("s1", { liveness: Liveness.Archived })]);
  const chip = row(pane, 1).querySelector(".af-backlog-chip-link")!;
  assert.equal(chip.textContent, "?Needs you · archived↗");
  chip.dispatchEvent(new FakeEvent("mouseenter"));
  mock.timers.tick(PREVIEW_DELAY_MS);
  assert.equal(calls.filter((c) => c.method === "PreviewSession").length, 0);
});

test("hovering the chip previews its session after 300 ms", async () => {
  reply = async (m) => (m === "PreviewSession" ? { text: "$ go test\nok" } : {});
  const pane = mount(host());
  pane.update(data(view([item(1, { status: "running", runs: [run("s1")] })], { 1: "working" })), "/a", [session("s1")]);
  const chip = row(pane, 1).querySelector(".af-backlog-chip-link")!;
  chip.dispatchEvent(new FakeEvent("mouseenter"));
  assert.equal(calls.length, 0);
  mock.timers.tick(PREVIEW_DELAY_MS);
  await settle();
  assert.deepEqual(calls, [{ method: "PreviewSession", body: { id: "s1", lines: 12 } }]);
  const pop = el(pane).querySelector(".af-backlog-preview")!;
  assert.equal(pop.hidden, false);
  assert.match(pop.textContent, /run s1 · 12m ago\$ go test\nok/);
});

test("blocks n counts open items that wait on this one, and a click picks them", () => {
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { waits_on: [1] }), item(3, { waits_on: [1] }), item(4)])), "/a", []);
  const blocks = row(pane, 1).querySelector(".af-backlog-blocks")!;
  assert.equal(blocks.textContent, "Blocks 2");
  assert.equal(row(pane, 2).querySelector(".af-backlog-blocks"), null);
  blocks.click();
  assert.deepEqual(lit(pane, "af-backlog-row-picked"), [1, 2, 3]);
  assert.ok(el(pane).querySelector(".af-backlog-dim"));
  assert.equal(doc.activeElement, row(pane, 2));
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
  row(pane, 2).dispatchEvent(new FakeEvent("keydown", { key: "Escape" }));
  assert.deepEqual(lit(pane, "af-backlog-row-picked"), []);
});

test("hover or focus lights up waits-on and needed-by rows and dims the rest", () => {
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { waits_on: [1] }), item(3, { waits_on: [2] }), item(4)])), "/a", []);
  row(pane, 2).dispatchEvent(new FakeEvent("mouseenter"));
  assert.deepEqual(lit(pane, "af-backlog-row-related"), [1, 3]);
  assert.deepEqual(lit(pane, "af-backlog-row-hot"), [2]);
  assert.ok(el(pane).querySelector(".af-backlog-dim"));
  row(pane, 2).dispatchEvent(new FakeEvent("mouseleave"));
  assert.deepEqual(lit(pane, "af-backlog-row-related"), []);
  assert.equal(el(pane).querySelector(".af-backlog-dim"), null);

  row(pane, 1).focus();
  assert.deepEqual(lit(pane, "af-backlog-row-related"), [2]);
  row(pane, 4).dispatchEvent(new FakeEvent("mouseenter"));
  assert.equal(el(pane).querySelector(".af-backlog-dim"), null);
});

test("a row shows its PR link at the end", () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { pr: "https://github.com/o/r/pull/7" }), item(2, { pr: "javascript:alert(1)" })])), "/a", []);
  assert.equal(row(pane, 1).querySelector(".af-backlog-row-pr")?.href, "https://github.com/o/r/pull/7");
  assert.equal(row(pane, 2).querySelector(".af-backlog-row-pr"), null);
});

test("the panel starts with a session card: preview, branch, PR, actions, earlier runs folded", async () => {
  reply = async (m) => (m === "PreviewSession" ? { text: "working on it" } : {});
  const h = host();
  const pane = mount(h);
  const it = item(1, { status: "review", pr: "https://github.com/o/r/pull/7", runs: [run("s0"), run("s1")] });
  pane.update(data(view([it], { 1: "in_review" })), "/a", [session("s0"), session("s1")]);
  pane.select(1);
  const card = (): FakeElement => el(pane).querySelector(".af-backlog-card")!;
  assert.equal(card().hidden, false);
  assert.match(card().textContent, /Loading preview…/);
  await settle();
  assert.equal(card().querySelector(".af-backlog-card-preview")?.textContent, "working on it");
  assert.equal(card().querySelector(".af-backlog-card-branch")?.textContent, "af/s1");
  assert.equal(card().querySelector(".af-backlog-pr")?.href, "https://github.com/o/r/pull/7");
  assert.equal(calls.filter((c) => c.method === "PreviewSession").length, 1);

  const head = card().querySelector(".af-backlog-card-head")!;
  assert.match(head.textContent, /^run s1\w+ · Started by runner · 12m ago$/);
  buttonNamed(card(), "Open")!.click();
  buttonNamed(card(), "Archive")!.click();
  assert.deepEqual([h.opened, h.archived], [["s1"], ["s1"]]);

  assert.ok(buttonNamed(card(), "Start again"));
  assert.equal(buttonNamed(el(pane).querySelector(".af-backlog-actions")!, "Start now"), undefined);

  const earlier = card().querySelector(".af-backlog-earlier")!;
  assert.equal(earlier.open, false);
  assert.equal(earlier.querySelector("summary")?.textContent, "Earlier runs (1)");
  assert.match(earlier.textContent, /run s0/);
});

test("the card says when the session is archived or gone, and is hidden with no run", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { runs: [run("s1")] }), item(2, { runs: [run("gone")] }), item(3)])), "/a",
    [session("s1", { liveness: Liveness.Archived })]);
  pane.select(1);
  const card = (): FakeElement => el(pane).querySelector(".af-backlog-card")!;
  assert.match(card().textContent, /Archived · Restore to see the session/);
  assert.ok(buttonNamed(card(), "Restore"));
  pane.select(2);
  assert.match(card().textContent, /Checking…/, "a session the rail does not hold is looked up before it is called gone");
  await settle();
  assert.match(card().textContent, /Session gone/);
  pane.select(3);
  assert.equal(card().hidden, true);
  assert.ok(buttonNamed(el(pane), "Start now"));
  assert.equal(calls.filter((c) => c.method === "PreviewSession").length, 0);
});

test("the chip and the card follow the same run: a new run that is starting has no link yet", () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { status: "running", runs: [run("a"), run("", { state: "starting" })] })], { 1: "starting" })), "/a",
    [session("a")]);
  assert.equal(row(pane, 1).querySelector(".af-backlog-chip-link"), null);
  pane.select(1);
  const card = el(pane).querySelector(".af-backlog-card")!;
  assert.match(card.textContent, /Starting…/);
  assert.equal(card.querySelector(".af-backlog-earlier summary")?.textContent, "Earlier runs (1)");
});

test("row keys: o opens the session, b picks what it blocks, p opens the PR", () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1, { status: "running", runs: [run("s1")], pr: "https://github.com/o/r/pull/1" }),
    item(2, { waits_on: [1] })], { 1: "working" })), "/a", [session("s1")]);
  assert.equal(row(pane, 1).getAttribute("aria-keyshortcuts"), "o b p");
  let prClicks = 0;
  row(pane, 1).querySelector(".af-backlog-row-pr")!.addEventListener("click", () => { prClicks++; });
  row(pane, 1).focus();
  row(pane, 1).dispatchEvent(new FakeEvent("keydown", { key: "o" }));
  assert.deepEqual(h.opened, ["s1"]);
  row(pane, 1).dispatchEvent(new FakeEvent("keydown", { key: "p" }));
  assert.equal(prClicks, 1);
  row(pane, 1).dispatchEvent(new FakeEvent("keydown", { key: "b" }));
  assert.deepEqual(lit(pane, "af-backlog-row-picked"), [1, 2]);
  assert.equal(doc.activeElement, row(pane, 2));
  const k = new FakeEvent("keydown", { key: "o" });
  row(pane, 2).dispatchEvent(k);
  assert.equal(k.defaultPrevented, false);
  assert.equal(el(pane).querySelector(".af-backlog-panel"), null);
});

test("focusing a row from the keyboard previews its session after 300 ms", async () => {
  reply = async (m) => (m === "PreviewSession" ? { text: "hi" } : {});
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { status: "running", runs: [run("s2")] })], { 2: "working" })), "/a", [session("s2")]);
  row(pane, 1).focus();
  row(pane, 1).dispatchEvent(new FakeEvent("keydown", { key: "ArrowDown" }));
  assert.equal(doc.activeElement, row(pane, 2));
  mock.timers.tick(PREVIEW_DELAY_MS);
  await settle();
  const pop = el(pane).querySelector(".af-backlog-preview")!;
  assert.equal(pop.hidden, false);
  assert.equal(row(pane, 2).querySelector(".af-backlog-chip-link")?.getAttribute("aria-describedby"), "af-backlog-preview");
  row(pane, 2).dispatchEvent(new FakeEvent("keydown", { key: "ArrowUp" }));
  assert.equal(pop.hidden, true);
});

test("a row's focus preview does not follow its rebuilt chip once Enter takes focus into the panel (Codex P2 on #6544)", async () => {
  reply = async (m) => (m === "PreviewSession" ? { text: "hi" } : {});
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { status: "running", runs: [run("s2")] })], { 2: "working" })), "/a", [session("s2")]);
  row(pane, 1).focus();
  row(pane, 1).dispatchEvent(new FakeEvent("keydown", { key: "ArrowDown" }));
  mock.timers.tick(PREVIEW_DELAY_MS);
  await settle();
  const pop = el(pane).querySelector(".af-backlog-preview")!;
  assert.equal(pop.hidden, false, "the focused row's preview shows");
  row(pane, 2).dispatchEvent(new FakeEvent("keydown", { key: "Enter" }));
  assert.ok(el(pane).querySelector(".af-backlog-panel")?.contains(doc.activeElement), "Enter moved focus into the panel");
  assert.equal(pop.hidden, true, "the preview no longer floats over the panel");
  assert.equal(row(pane, 2).querySelector(".af-backlog-chip-link")?.getAttribute("aria-describedby"), null);
});

test("Blocks n unfolds and clears a filter so the picked rows show", () => {
  const pane = mount(host());
  pane.update(data(view([item(1), item(3), item(2, { parent: 3, waits_on: [1] })], { 1: "queued", 2: "draft", 3: "draft" })), "/a", []);
  row(pane, 3).querySelector(".af-backlog-fold")!.click();
  buttonNamed(el(pane), "Queued")!.click();
  assert.deepEqual(el(pane).querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId)), [1]);
  row(pane, 1).querySelector(".af-backlog-blocks")!.click();
  assert.deepEqual(el(pane).querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId)), [1, 3, 2]);
  assert.equal(doc.activeElement, row(pane, 2));
  assert.equal(buttonNamed(el(pane), "All")?.getAttribute("aria-pressed"), "true");
});
