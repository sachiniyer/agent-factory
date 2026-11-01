import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, type FakeElement, installFakeDom } from "./backlog_fakedom.js";
import { bodyEditorLoader } from "./backlog_body.js";
import type { BacklogData } from "./backlog_data.js";
import { answerEffect, groupDependencies, groupHistory, inbox } from "./backlog_groups.js";
import { type BacklogView, type Item, type Message, type RunRef, needsYouCount } from "./backlog_model.js";
import { BacklogPane, type BacklogHost } from "./backlog_pane.js";
import { InFlightOp, Liveness, type SessionData } from "./types.js";

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: { method: string; body: Record<string, unknown> }[];
let refuse: string | null;
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;
const NOW = Date.parse("2000-01-03T10:00:00Z");

beforeEach(() => {
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  refuse = null;
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    calls.push({ method: url.replace("/v1/", ""), body: JSON.parse(String(init.body)) as Record<string, unknown> });
    if (refuse !== null) return new Response(JSON.stringify({ data: null, error: { message: refuse } }), { status: 400 });
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

const ago = (min: number): string => new Date(NOW - min * 60_000).toISOString();

function item(id: number, over: Partial<Item> = {}): Item {
  return { id, title: `item ${id}`, status: "draft", created_at: "", updated_at: "", runs: [], waits_on: [], thread: [], ...over };
}

function run(sid: string, over: Partial<RunRef> = {}): RunRef {
  return { session_id: sid, title: `run ${sid}`, state: "started", prev_status: "queued", started_by: "runner", started_at: ago(60), ...over };
}

function question(id: number, min: number, over: Partial<Message> = {}): Message {
  return { id, kind: "question", author: "agent:s", text: `question ${id}`, at: ago(min), ...over };
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
    loadSessions: async () => {},
    confirm: async () => true,
  };
  return h;
}

function mount(h: BacklogHost = host()): BacklogPane {
  const pane = new BacklogPane(h);
  doc.body.append(pane.el as unknown as FakeElement);
  return pane;
}

const el = (pane: BacklogPane): FakeElement => pane.el as unknown as FakeElement;
const session = (id: string, over: Partial<SessionData> = {}): SessionData =>
  ({ id, title: id, status: "Ready", branch: `af/${id}`, ...over } as unknown as SessionData);
const buttonNamed = (root: FakeElement, name: string): FakeElement | undefined =>
  root.querySelectorAll("button").find((b) => b.textContent === name && !b.hidden);
const rowIDs = (pane: BacklogPane): number[] =>
  el(pane).querySelectorAll(".af-backlog-row").map((r) => Number(r.dataset.itemId));

test("inbox: every open question oldest first, archived groups left out", () => {
  const v = view([
    item(1, { status: "running", thread: [question(1, 5), question(2, 30)] }),
    item(2, { status: "running", thread: [question(1, 10)] }),
    item(3, { archived: true, paused: true }),
    item(4, { parent: 3, status: "running", thread: [question(1, 60)] }),
  ]);
  assert.deepEqual(inbox(v).map((e) => [e.item.id, e.question.id]), [[1, 2], [2, 1], [1, 1]]);
  assert.equal(needsYouCount([v]), 3);
});

test("an answer restores an archived session, starts a new run when it is gone", () => {
  const it = item(1, { runs: [run("s1")] });
  assert.equal(answerEffect(it, [session("s1")]), "live");
  assert.equal(answerEffect(it, [session("s1", { liveness: Liveness.Archived })]), "restore");
  assert.equal(answerEffect(it, []), "new_run");
});

test("an answer starts a new run unless a started run's session is listed and not being killed", () => {
  const live = [session("s1")];
  assert.equal(answerEffect(item(1), live), "new_run");
  assert.equal(answerEffect(item(1, { runs: [run("s1", { state: "failed" })] }), live), "new_run");
  assert.equal(answerEffect(item(1, { runs: [run("s1"), run("", { state: "starting" })] }), live), "new_run");
  const it = item(1, { runs: [run("s1")] });
  assert.equal(answerEffect(it, [session("s1", { user_killed: true })]), "new_run");
  assert.equal(answerEffect(it, [session("s1", { in_flight_op: InFlightOp.Killing })]), "new_run");
  assert.equal(answerEffect(it, [session("s1", { in_flight_op: InFlightOp.Archiving })]), "live");
});

test("group dependencies name links inside and out; history is closed items, newest first", () => {
  const v = view([
    item(1), item(2, { parent: 1, waits_on: [3] }), item(3, { parent: 1, waits_on: [9] }), item(9),
    item(4, { parent: 1, status: "done", updated_at: ago(30) }), item(5, { parent: 1, status: "dropped", updated_at: ago(5) }),
  ], { 4: "done", 5: "dropped" });
  assert.deepEqual(groupDependencies(v.backlog, 1).map((d) => [d.from.id, d.toID, d.inside]), [[2, 3, true], [3, 9, false]]);
  assert.deepEqual(groupHistory(v, 1).map((it) => it.id), [5, 4]);
});

test("the Inbox filter shows each question with its handoff, age and what an answer does", async () => {
  const pane = mount();
  const v = view([
    item(1, { status: "running", runs: [run("s1")], thread: [question(1, 90, { handoff: "Tried A.\nB is next." })] }),
    item(2, { status: "running", runs: [run("s2")], thread: [question(1, 5)] }),
  ], { 1: "needs_you", 2: "needs_you" });
  pane.update(data(v), "/a", [session("s2", { liveness: Liveness.Archived })]);
  const chip = buttonNamed(el(pane), "Inbox (2)")!;
  chip.click();
  assert.equal(buttonNamed(el(pane), "Inbox (2)")!.getAttribute("aria-pressed"), "true");
  assert.deepEqual(rowIDs(pane), []);
  const cards = el(pane).querySelectorAll(".af-backlog-inbox-card");
  assert.equal(cards.length, 2);
  assert.match(cards[0].textContent, /B-1 item 1Asked 1h ago/);
  assert.match(cards[0].textContent, /question 1Notes from the agentTried A\.\nB is next\./);
  assert.match(cards[0].textContent, /Starts a new run with the previous agent's notes/);
  assert.match(cards[1].textContent, /Restores the session/);

  const answer = cards[1].querySelector("textarea")!;
  answer.value = "yes";
  pane.update(data(view(v.backlog.items, { 1: "needs_you", 2: "needs_you" })), "/a", [session("s2", { liveness: Liveness.Archived })]);
  assert.equal(el(pane).querySelectorAll(".af-backlog-inbox-card")[1].querySelector("textarea"), answer);
  assert.equal(answer.value, "yes");
  buttonNamed(cards[1], "Send answer")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "PostBacklogMessage",
    body: { repo_path: "/a", id: 2, kind: "answer", text: "yes", question_id: 1 } });
  assert.equal(answer.value, "");
});

test("an empty inbox says so, and another filter leaves it", () => {
  const pane = mount();
  pane.update(data(view([item(1)])), "/a", []);
  buttonNamed(el(pane), "Inbox (0)")!.click();
  assert.match(el(pane).querySelector(".af-backlog-inbox")!.textContent, /No questions/);
  buttonNamed(el(pane), "All")!.click();
  assert.equal(el(pane).querySelector(".af-backlog-inbox"), null);
  assert.deepEqual(rowIDs(pane), [1]);
});

test("showInbox opens the Inbox filter", () => {
  const pane = mount();
  pane.update(data(view([item(1)])), "/a", []);
  pane.showInbox();
  assert.ok(el(pane).querySelector(".af-backlog-inbox"));
});

test("a paused group says so, and its queued child reads Queued · paused", () => {
  const pane = mount();
  const v = view([item(3, { paused: true }), item(5, { parent: 3, status: "queued" })], { 5: "queued" });
  v.states["5"] = { state: "queued", reason: "Queued · paused (B-3)" };
  pane.update(data(v), "/a", []);
  const rows = el(pane).querySelectorAll(".af-backlog-row");
  assert.match(rows[0].textContent, /‖ Paused/);
  assert.match(rows[1].querySelector(".af-backlog-state")!.textContent, /^▸Queued· paused \(B-3\)$/);
});

test("archived groups hide until Show archived", () => {
  const pane = mount();
  pane.update(data(view([item(1, { archived: true, paused: true }), item(2, { parent: 1 }), item(3)])), "/a", []);
  assert.deepEqual(rowIDs(pane), [3]);
  buttonNamed(el(pane), "Show archived")!.click();
  assert.deepEqual(rowIDs(pane), [1, 2, 3]);
  assert.match(el(pane).querySelector(`.af-backlog-row[data-item-id="1"]`)!.textContent, /Archived/);
});

test("the group panel pauses, archives, and lists dependencies and history", async () => {
  const pane = mount();
  const items = [item(1), item(2, { parent: 1, waits_on: [9] }), item(4, { parent: 1, status: "done", updated_at: ago(30) }), item(9)];
  pane.update(data(view(items, { 2: "waiting", 4: "done", 9: "working" })), "/a", []);
  pane.select(1);
  const panel = el(pane).querySelector(".af-backlog-panel")!;
  assert.match(panel.textContent, /DependenciesB-2 item 2\(Waiting\)waits onB-9 item 9\(Working\)· outside the group/);
  assert.match(panel.textContent, /History.*B-4 item 4✓Done/);
  buttonNamed(panel, "Pause")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "UpdateBacklogItem", body: { repo_path: "/a", id: 1, paused: true } });
  buttonNamed(panel, "Archive")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "UpdateBacklogItem", body: { repo_path: "/a", id: 1, archived: true } });

  pane.update(data(view([{ ...items[0], archived: true, paused: true }, ...items.slice(1)])), "/a", []);
  assert.equal(buttonNamed(panel, "Unpause"), undefined);
  buttonNamed(panel, "Unarchive")!.click();
  await settle();
  assert.deepEqual(calls.at(-1), { method: "UpdateBacklogItem", body: { repo_path: "/a", id: 1, archived: false } });
});

test("a daemon refusal of an archive reaches notify", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(3), item(5, { parent: 3, status: "running" })], { 5: "working" })), "/a", []);
  pane.select(3);
  refuse = "B-3 has a running item (B-5); stop it before you archive B-3";
  buttonNamed(el(pane).querySelector(".af-backlog-panel")!, "Archive")!.click();
  await settle();
  assert.deepEqual(h.notes, ["B-3 has a running item (B-5); stop it before you archive B-3"]);
});

test("a child parent pauses but does not archive", () => {
  const pane = mount();
  pane.update(data(view([item(1), item(2, { parent: 1 }), item(3, { parent: 2 })])), "/a", []);
  pane.select(2);
  const panel = el(pane).querySelector(".af-backlog-panel")!;
  assert.ok(buttonNamed(panel, "Pause"));
  assert.equal(buttonNamed(panel, "Archive"), undefined);
  assert.doesNotMatch(panel.textContent, /Dependencies/);
});

test("the panel's open question shows the handoff in full", () => {
  const pane = mount();
  pane.update(data(view([item(1, { status: "running", runs: [run("s1")], thread: [question(1, 5, { handoff: "notes" })] })],
    { 1: "needs_you" })), "/a", [session("s1")]);
  pane.select(1);
  const q = el(pane).querySelector(".af-backlog-panel .af-backlog-question")!;
  assert.match(q.textContent, /question 1Notes from the agentnotesGoes to the session/);
});
