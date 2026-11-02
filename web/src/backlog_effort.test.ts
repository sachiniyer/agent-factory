import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, type FakeElement, installFakeDom } from "./backlog_fakedom.js";
import { bodyEditorLoader } from "./backlog_body.js";
import type { BacklogData } from "./backlog_data.js";
import { inbox, inboxNotes } from "./backlog_groups.js";
import { type BacklogView, type Item, type Message, listRows, needsYouCount } from "./backlog_model.js";
import { BacklogPane, type BacklogHost } from "./backlog_pane.js";
import { WAIT_LISTED_MS } from "./backlog_effort.js";
import type { SessionData } from "./types.js";

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: { method: string; body: Record<string, unknown> }[];
let replies: Record<string, unknown>;
let refusals: Record<string, string>;
let hold: Promise<void> | null;
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;

beforeEach(() => {
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  replies = {};
  refusals = {};
  hold = null;
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    const method = url.replace("/v1/", "");
    calls.push({ method, body: JSON.parse(String(init.body)) as Record<string, unknown> });
    if (hold) await hold;
    if (refusals[method]) return new Response(JSON.stringify({ data: null, error: { message: refusals[method] } }), { status: 400 });
    return new Response(JSON.stringify({ data: replies[method] ?? {}, error: null }), { status: 200 });
  };
  bodyEditorLoader.load = async () => ({
    createDocsEditor: (parent: HTMLElement) => {
      (parent as unknown as FakeElement).append(doc.createElement("div"));
      return { getText: () => "", setText: () => {}, destroy: () => {}, applyRemote: () => {}, goToNextQuestion: () => false, focus: () => {} } as never;
    },
  });
  mock.timers.enable({ apis: ["setTimeout", "Date"], now: Date.parse("2000-01-03T10:00:00Z") });
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

const question = (id: number, route?: string): Message => ({ id, kind: "question", author: "agent:s", text: `q${id}`, at: "2000-01-01T00:00:00Z", route });
const orch = { session_id: "o1", title: "effort B-1", bound_at: "" };

function view(items: Item[], orchestrators: BacklogView["orchestrators"] = {}): BacklogView {
  return {
    backlog: { schema_version: 3, next_id: 100, settings: { runner: false, max_running: 2, on_done: "keep" }, items },
    states: {}, runner: { state: "off", used: 0, max: 2 }, orchestrators,
  };
}

function data(v: BacklogView): BacklogData {
  return { views: new Map([["/a", v]]), errors: new Map(), budget: null, usage: null, generation: 1 };
}

function host(): BacklogHost & { opened: string[]; notes: string[] } {
  const h = {
    opened: [] as string[],
    notes: [] as string[],
    token: () => "t",
    refresh: async () => true,
    started: () => 1,
    notify: (m: string) => { h.notes.push(m); },
    openSession: (id: string) => { h.opened.push(id); },
    archiveSession: () => {},
    restoreSession: () => {},
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
const effortButton = (pane: BacklogPane): FakeElement => el(pane).querySelector(".af-backlog-effort")!;
const session = (id: string): SessionData => ({ id, title: id }) as SessionData;

test("a question routed to a bound group's orchestrator leaves the Inbox and the needs-you counts", () => {
  const routed = (o: typeof orch | null) => view([
    item(1, { orchestrator: o }), item(2, { parent: 1, thread: [question(1, "orchestrator")] }),
    item(3, { parent: 1, thread: [question(2, "user")] }), item(4, { parent: 1, thread: [question(3)] }),
  ]);
  const bound = routed(orch);
  assert.deepEqual(inbox(bound).map((e) => e.question.id), [2, 3]);
  assert.equal(needsYouCount([bound]), 2);
  assert.equal(listRows(bound, { filter: "all", showDone: true, folded: new Set() })[0].reason, "2 needs you");

  const unbound = routed(null);
  assert.deepEqual(inbox(unbound).map((e) => e.question.id), [1, 2, 3], "a routed question in a group with no orchestrator is the user's");
  assert.equal(needsYouCount([unbound]), 3);
});

test("inbox notes list every item's Inbox notes, oldest first, leaving out archived groups", () => {
  const note = (id: number, at: string, inboxNote = true): Message => ({ id, kind: "note", author: "agent:o1", text: `n${id}`, at, inbox: inboxNote });
  const v = view([
    item(1), item(2, { parent: 1, thread: [note(1, "2000-01-02T00:00:00Z"), note(2, "2000-01-01T00:00:00Z", false)] }),
    item(3, { parent: 1, thread: [note(1, "2000-01-01T00:00:00Z")] }),
    item(4, { archived: true }), item(5, { parent: 4, thread: [note(1, "2000-01-01T00:00:00Z")] }),
  ]);
  assert.deepEqual(inboxNotes(v).map((n) => [n.item.id, n.note.id]), [[3, 1], [2, 1]]);
});

test("Start effort session opens the session af starts once it is listed", async () => {
  const h = host();
  const pane = mount(h);
  const d = data(view([item(1), item(2, { parent: 1 })]));
  pane.update(d, "/a", []);
  replies.OpenBacklogOrchestrator = { session_id: "n1", title: "effort B-1", change: "started" };
  let release = (): void => {};
  hold = new Promise((r) => { release = r; });

  assert.equal(effortButton(pane).textContent, "Start effort session");
  effortButton(pane).click();
  await settle();
  assert.equal(effortButton(pane).textContent, "Starting effort session…");
  assert.equal(effortButton(pane).disabled, true);
  assert.equal(effortButton(pane).getAttribute("aria-label"), "Starting effort session for B-1");
  assert.equal(effortButton(pane).getAttribute("aria-busy"), "true");
  release();
  await settle();

  assert.deepEqual(calls.find((c) => c.method === "OpenBacklogOrchestrator")?.body, { repo_path: "/a", id: 1 });
  assert.equal(effortButton(pane).textContent, "Start effort session");
  assert.equal(effortButton(pane).getAttribute("aria-busy"), null);
  assert.equal(effortButton(pane).getAttribute("aria-label"), "Start effort session for B-1");
  assert.deepEqual(h.opened, [], "not listed yet");
  pane.update(d, "/a", [session("n1")]);
  assert.deepEqual(h.opened, ["n1"]);
  pane.update(d, "/a", [session("n1"), session("x")]);
  assert.deepEqual(h.opened, ["n1"], "opens once");
});

test("Effort session restores an archived orchestrator and opens it", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1, { orchestrator: orch }), item(2, { parent: 1 })],
    { 1: { ...orch, state: "archived", pending: 0 } })), "/a", [session("o1")]);
  replies.OpenBacklogOrchestrator = { session_id: "o1", title: "effort B-1", change: "restored" };
  let release = (): void => {};
  hold = new Promise((r) => { release = r; });

  assert.equal(effortButton(pane).textContent, "Effort session");
  effortButton(pane).click();
  await settle();
  assert.equal(effortButton(pane).textContent, "Restoring effort session…");
  release();
  await settle();
  assert.deepEqual(h.opened, ["o1"]);
});

test("a failed open names the error", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1), item(2, { parent: 1 })])), "/a", []);
  (globalThis as { fetch: unknown }).fetch = async () =>
    new Response(JSON.stringify({ data: null, error: { code: "invalid", message: "B-1 is archived; unarchive it to start an effort session" } }), { status: 400 });
  effortButton(pane).click();
  await settle();
  assert.match(h.notes.join("\n"), /unarchive it/);
  assert.deepEqual(h.opened, []);
});

test("the group panel shows the Orchestrator row, and Unbind clears it", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { orchestrator: orch }), item(2, { parent: 1 })],
    { 1: { ...orch, state: "live", pending: 2 } })), "/a", []);
  el(pane).querySelectorAll("button").find((b) => b.textContent === "Details")!.click();
  await settle();
  const row = el(pane).querySelector(".af-backlog-orchestrator")!;
  assert.equal(row.textContent, "effort B-1Live · 2 pending eventsUnbind");
  row.querySelectorAll("button").find((b) => b.textContent === "Unbind")!.click();
  await settle();
  assert.deepEqual(calls.find((c) => c.method === "SetBacklogOrchestrator")?.body, { repo_path: "/a", id: 1, clear: true });
});

test("an item whose question waits for the effort session says so", () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { orchestrator: orch }), item(2, { parent: 1, status: "running", thread: [question(1, "orchestrator")] })],
    { 1: { ...orch, state: "live", pending: 1 } })), "/a", []);
  const row = el(pane).querySelector('.af-backlog-row[data-item-id="2"]')!;
  assert.equal(row.querySelector(".af-backlog-effort-tag")?.textContent, "With effort session");
});

test("the Inbox shows an effort session's done note with its pull request", async () => {
  const pane = mount(host());
  const note: Message = { id: 1, kind: "note", author: "agent:o1", text: "The effort session marked this done · https://example.test/pr/1",
    at: "2000-01-02T00:00:00Z", inbox: true };
  pane.update(data(view([item(1, { orchestrator: orch }), item(2, { parent: 1, status: "done", pr: "https://example.test/pr/1", thread: [note] })])), "/a", []);
  el(pane).querySelectorAll("button").find((b) => b.textContent?.startsWith("Inbox"))!.click();
  const card = el(pane).querySelector(".af-backlog-inbox-note")!;
  assert.match(card.textContent ?? "", /B-2 item 2/);
  assert.match(card.textContent ?? "", /marked this done/);
  assert.equal(card.querySelector("a")?.href, "https://example.test/pr/1");
  assert.equal(el(pane).querySelector(".af-backlog-empty")?.hidden, true);
});

const doneNote = (id: number, at: string): Message => ({ id, kind: "note", author: "agent:o1", text: `done ${id}`, at, inbox: true });

function inboxWithNotes(h: ReturnType<typeof host>, notes: Message[], d?: BacklogData): BacklogPane {
  const pane = mount(h);
  pane.update(d ?? data(view([item(1, { orchestrator: orch }), item(2, { parent: 1, status: "done", thread: notes })])), "/a", []);
  el(pane).querySelectorAll("button").find((b) => b.textContent?.startsWith("Inbox"))!.click();
  return pane;
}

const dismissButtons = (pane: BacklogPane) => el(pane).querySelectorAll("button").filter((b) => b.textContent === "Dismiss");

test("Dismiss sends DismissBacklogNote and moves focus to the next note card", async () => {
  const pane = inboxWithNotes(host(), [doneNote(1, "2000-01-02T00:00:00Z"), doneNote(2, "2000-01-02T01:00:00Z")]);
  const [first, second] = dismissButtons(pane);
  assert.notEqual(first.getAttribute("aria-label"), second.getAttribute("aria-label"));
  assert.match(first.getAttribute("aria-label") ?? "", /^Dismiss note on B-2 from /);
  first.focus();
  first.click();
  assert.equal(first.disabled, true);
  await settle();
  assert.deepEqual(calls.find((c) => c.method === "DismissBacklogNote")?.body, { repo_path: "/a", id: 2, note_id: 1 });
  const cards = el(pane).querySelectorAll(".af-backlog-inbox-note");
  assert.equal(doc.activeElement, cards[1].querySelector("button"));
});

test("Dismiss on the last note card moves focus to the previous card", async () => {
  const pane = inboxWithNotes(host(), [doneNote(1, "2000-01-02T00:00:00Z"), doneNote(2, "2000-01-02T01:00:00Z")]);
  const second = dismissButtons(pane)[1];
  second.focus();
  second.click();
  await settle();
  assert.equal(doc.activeElement, el(pane).querySelectorAll(".af-backlog-inbox-note")[0].querySelector("button"));
});

test("Dismiss on the only card moves focus to the Inbox chip", async () => {
  const pane = inboxWithNotes(host(), [doneNote(1, "2000-01-02T00:00:00Z")]);
  const only = dismissButtons(pane)[0];
  only.focus();
  only.click();
  await settle();
  assert.match(doc.activeElement.textContent ?? "", /^Inbox/);
  pane.update(data(view([item(1, { orchestrator: orch }), item(2, { parent: 1, status: "done" })])), "/a", []);
  assert.match(doc.activeElement.textContent ?? "", /^Inbox/, "a re-render keeps the focused chip");
});

test("a failed Dismiss says why and enables the button again", async () => {
  const h = host();
  const pane = inboxWithNotes(h, [doneNote(1, "2000-01-02T00:00:00Z")]);
  refusals.DismissBacklogNote = "message 1 on B-2 is not a note in the Inbox";
  const only = dismissButtons(pane)[0];
  only.click();
  await settle();
  assert.equal(only.disabled, false);
  assert.deepEqual(h.notes, ["message 1 on B-2 is not a note in the Inbox"]);
});

test("a Dismiss that lands after a move to another project dismisses in the card's project and moves no focus", async () => {
  const notes = [doneNote(1, "2000-01-02T00:00:00Z")];
  const d = data(view([item(1, { orchestrator: orch }), item(2, { parent: 1, status: "done", thread: notes })]));
  const both: BacklogData = { ...d, views: new Map([...d.views, ["/b", view([])]]) };
  const pane = inboxWithNotes(host(), notes, both);
  let release = (): void => {};
  hold = new Promise((r) => { release = r; });
  const only = dismissButtons(pane)[0];
  only.focus();
  only.click();
  await settle();
  pane.update(both, "/b", []);
  release();
  await settle();
  assert.deepEqual(calls.find((c) => c.method === "DismissBacklogNote")?.body, { repo_path: "/a", id: 2, note_id: 1 });
  assert.equal(doc.activeElement === doc.body, true, `focus moved to ${doc.activeElement.textContent}`);
});

async function startUnlisted(h: ReturnType<typeof host>): Promise<{ pane: BacklogPane; d: BacklogData }> {
  const pane = mount(h);
  const d = data(view([item(1), item(2, { parent: 1 })]));
  pane.update(d, "/a", []);
  replies.OpenBacklogOrchestrator = { session_id: "n1", title: "effort B-1", change: "started" };
  effortButton(pane).click();
  await settle();
  return { pane, d };
}

test("a session listed after the wait window does not steal focus", async () => {
  const h = host();
  const { pane, d } = await startUnlisted(h);
  mock.timers.tick(WAIT_LISTED_MS);
  pane.update(d, "/a", [session("n1")]);
  assert.deepEqual(h.opened, []);
});

test("a session listed within the wait window opens", async () => {
  const h = host();
  const { pane, d } = await startUnlisted(h);
  mock.timers.tick(WAIT_LISTED_MS - 1);
  pane.update(d, "/a", [session("n1")]);
  assert.deepEqual(h.opened, ["n1"]);
});

test("moving to another project stops waiting for the session", async () => {
  const h = host();
  const { pane, d } = await startUnlisted(h);
  const other: BacklogData = { ...d, views: new Map([...d.views, ["/b", view([])]]) };
  pane.update(other, "/b", []);
  pane.update(other, "/b", [session("n1")]);
  assert.deepEqual(h.opened, []);
});

test("a reply that comes after a move to another project opens nothing there", async () => {
  const h = host();
  const pane = mount(h);
  const d = data(view([item(1), item(2, { parent: 1 })]));
  const other: BacklogData = { ...d, views: new Map([...d.views, ["/b", view([])]]) };
  pane.update(d, "/a", []);
  replies.OpenBacklogOrchestrator = { session_id: "n1", title: "effort B-1", change: "started" };
  let release = (): void => {};
  hold = new Promise((r) => { release = r; });
  effortButton(pane).click();
  await settle();
  pane.update(other, "/b", [session("n1")]);
  release();
  await settle();
  pane.update(other, "/b", [session("n1"), session("x")]);
  assert.deepEqual(h.opened, []);
});

/** Two groups, B-1 and B-3, whose OpenBacklogOrchestrator replies wait until
 *  the test releases them one by one: group g's names session "n<g>". */
function twoGroupsHeld(h: ReturnType<typeof host>, listed: SessionData[]): { pane: BacklogPane; d: BacklogData; release(g: number): Promise<void> } {
  const pane = mount(h);
  const d = data(view([item(1), item(2, { parent: 1 }), item(3), item(4, { parent: 3 })]));
  pane.update(d, "/a", listed);
  const gates = new Map<number, () => void>();
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    const body = JSON.parse(String(init.body)) as { id: number };
    calls.push({ method: url.replace("/v1/", ""), body });
    await new Promise<void>((r) => gates.set(body.id, r));
    return new Response(JSON.stringify({ data: { session_id: `n${body.id}`, title: `effort B-${body.id}`, change: "started" }, error: null }), { status: 200 });
  };
  return {
    pane, d,
    release: async (g: number) => {
      gates.get(g)!();
      await settle();
    },
  };
}

const effortFor = (pane: BacklogPane, id: number): FakeElement =>
  el(pane).querySelectorAll(".af-backlog-effort").find((b) => b.getAttribute("aria-label")?.endsWith(`for B-${id}`))!;

test("an older open's late reply does not take focus from the newer one", async () => {
  const h = host();
  const { pane, release } = twoGroupsHeld(h, [session("n1"), session("n3")]);
  effortFor(pane, 1).click();
  await settle();
  effortFor(pane, 3).click();
  await settle();
  await release(3);
  assert.deepEqual(h.opened, ["n3"]);
  await release(1);
  assert.deepEqual(h.opened, ["n3"], "B-1's reply came after the user opened B-3");
});

test("an older open's late reply does not replace the newer one's listing wait", async () => {
  const h = host();
  const { pane, d, release } = twoGroupsHeld(h, []);
  effortFor(pane, 1).click();
  await settle();
  effortFor(pane, 3).click();
  await settle();
  await release(3);
  await release(1);
  pane.update(d, "/a", [session("n1"), session("n3")]);
  assert.deepEqual(h.opened, ["n3"], "only the latest open waits for its session");
});

test("the group panel offers Start effort session to a keyboard user when the group has none", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1), item(2, { parent: 1 })])), "/a", []);
  el(pane).querySelectorAll("button").find((b) => b.textContent === "Details")!.click();
  await settle();
  const start = el(pane).querySelector(".af-backlog-panel .af-backlog-orchestrator .af-backlog-effort")!;
  assert.equal(start.textContent, "Start effort session");
  assert.equal(start.getAttribute("tabindex"), null, "reachable by Tab");
  start.click();
  await settle();
  assert.deepEqual(calls.find((c) => c.method === "OpenBacklogOrchestrator")?.body, { repo_path: "/a", id: 1 });
});

test("an archived group with no effort session offers none in the panel", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1, { archived: true }), item(2, { parent: 1 })])), "/a", []);
  el(pane).querySelectorAll("button").find((b) => b.textContent === "Show archived")!.click();
  el(pane).querySelectorAll("button").find((b) => b.textContent === "Details")!.click();
  await settle();
  assert.ok(el(pane).querySelector(".af-backlog-panel"));
  assert.equal(el(pane).querySelector(".af-backlog-orchestrator"), null);
});
