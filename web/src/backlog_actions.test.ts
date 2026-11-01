import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeDocument, type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import { backlogRoutes } from "./backlog_api.js";
import { bodyEditorLoader } from "./backlog_body.js";
import type { BacklogData } from "./backlog_data.js";
import type { BacklogView, Budget, Item, RunnerView } from "./backlog_model.js";
import { BacklogPane, type BacklogHost } from "./backlog_pane.js";
import type { Usage } from "./backlog_usage.js";
import type { SessionData } from "./types.js";

interface Call {
  method: string;
  body: Record<string, unknown>;
}

let dom: ReturnType<typeof installFakeDom>;
let doc: FakeDocument;
let calls: Call[];
let reply: (method: string, body: Record<string, unknown>) => Promise<unknown>;
const realFetch = globalThis.fetch;
const realLoad = bodyEditorLoader.load;
const NOW = Date.parse("2000-01-03T10:00:00");
const iso = (s: string): string => new Date(s).toISOString();

beforeEach(() => {
  backlogRoutes.runBacklog = true; // the off case has its own test
  dom = installFakeDom();
  doc = dom.doc;
  calls = [];
  reply = async () => ({});
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit) => {
    const method = url.replace("/v1/", "");
    const body = JSON.parse(String(init.body)) as Record<string, unknown>;
    calls.push({ method, body });
    try {
      return new Response(JSON.stringify({ data: await reply(method, body), error: null }), { status: 200 });
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

afterEach(() => {
  backlogRoutes.runBacklog = true;
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

function view(items: Item[], states: Record<number, string> = {}, runner: Partial<RunnerView> = {}): BacklogView {
  const st: BacklogView["states"] = {};
  for (const [id, s] of Object.entries(states)) st[id] = { state: s as never };
  return {
    backlog: { schema_version: 1, next_id: 100, settings: { runner: false, max_running: 2, on_done: "keep" }, items },
    states: st, runner: { state: "off", reason: "Items run only when you start them.", used: 0, max: 2, ...runner },
  };
}

const BUDGET: Budget = { limit: 50, max_per_run: 10, usd_limit: 0, usd_spent: 0 };

function usage(over: Partial<Usage> = {}): Usage {
  return {
    metering: false,
    account: "default",
    accounts: {},
    five_hour: { plan: 41, resets_at: iso("2000-01-03T14:00:00"), all_cost: 20 },
    seven_day: { plan: 40, resets_at: iso("2000-01-10T09:00:00"), all_cost: 200 },
    reading_at: iso("2000-01-03T09:58:00"),
    runs: {},
    run_window: {},
    unmeterable: {},
    ...over,
  };
}

function data(v: BacklogView, generation = 1, u: Usage | null = usage(), budget: Budget | null = BUDGET): BacklogData {
  return { views: new Map([["/a", v]]), errors: new Map(), budget, usage: u, generation };
}

interface TestHost extends BacklogHost {
  notices: string[];
  asked: string[][];
  answerConfirm: boolean;
  gen: number;
}

function host(): TestHost {
  const h: TestHost = {
    notices: [],
    asked: [],
    answerConfirm: true,
    gen: 1,
    token: () => "t",
    refresh: async () => { h.gen++; return true; },
    started: () => h.gen,
    notify: (m: string) => { h.notices.push(m); },
    openSession: () => {},
    archiveSession: () => {},
    restoreSession: () => {},
    loadSessions: async () => {},
    confirm: async (title: string, body: string, label: string) => {
      h.asked.push([title, body, label]);
      return h.answerConfirm;
    },
  };
  return h;
}

function mount(h: BacklogHost): BacklogPane {
  const pane = new BacklogPane(h);
  doc.body.append(pane.el as unknown as FakeElement);
  return pane;
}

const el = (pane: BacklogPane): FakeElement => pane.el as unknown as FakeElement;
const chip = (pane: BacklogPane, id: number): string =>
  el(pane).querySelector(`.af-backlog-row[data-item-id="${id}"] .af-backlog-chip`)?.textContent ?? "";
const buttonNamed = (root: FakeElement, name: string): FakeElement | undefined =>
  root.querySelectorAll("button").find((b) => b.textContent === name && !b.hidden);
const session = (id: string): SessionData => ({ id, title: id, status: "Ready" } as unknown as SessionData);
const sent = (method: string): Call[] => calls.filter((c) => c.method === method);

test("Start now shows Starting at once and until the session exists", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1)], { 1: "draft" })), "/a", []);
  pane.select(1);
  reply = async (m) => (m === "StartBacklogItem" ? { run: { session_id: "", state: "starting" } } : {});
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.deepEqual(sent("StartBacklogItem")[0].body, { repo_path: "/a", id: 1 });
  assert.match(chip(pane, 1), /Starting/);
  assert.equal(buttonNamed(el(pane), "Start now"), undefined);
  const run = { session_id: "s1", title: "B-1 item 1", state: "started", prev_status: "draft" as const, started_by: "user", started_at: "" };
  const started = view([item(1, { status: "running", runs: [run] })], { 1: "working" });
  pane.update(data(started, 1), "/a", []);
  assert.match(chip(pane, 1), /Starting/, "a refresh started before the reply still shows Starting");
  pane.update(data(started, 3), "/a", []);
  assert.match(chip(pane, 1), /Starting/, "the session is not in the list yet");
  pane.update(data(started, 3), "/a", [session("s1")]);
  assert.match(chip(pane, 1), /Working/);
});

test("a refused start says why and drops Starting", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1)], { 1: "draft" })), "/a", []);
  pane.select(1);
  reply = async (m) => {
    if (m === "StartBacklogItem") throw new Error("B-1 is already starting");
    return {};
  };
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.deepEqual(h.notices, ["B-1 is already starting"]);
  assert.match(chip(pane, 1), /Draft/);
});

test("a closed budget gate asks once before a start by hand", async () => {
  const h = host();
  const pane = mount(h);
  const v = view([item(12), item(13)], { 12: "queued", 13: "queued" });
  const used = usage({ seven_day: { plan: 60, resets_at: iso("2000-01-10T09:00:00"), all_cost: 1 } });
  pane.update(data(v, 1, used), "/a", []);
  pane.select(12);
  h.answerConfirm = false;
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.deepEqual(h.asked, [["Budget gate closed",
    "The plan is at 60% of the week, over the 50% limit. Start B-12 anyway? This run will use more.", "Start anyway"]]);
  assert.equal(sent("StartBacklogItem").length, 0);
  h.answerConfirm = true;
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.equal(sent("StartBacklogItem").length, 1);
  const more = usage({ seven_day: { plan: 64, resets_at: iso("2000-01-10T09:00:00"), all_cost: 1 } });
  pane.update(data(v, 2, more), "/a", []);
  pane.select(13);
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.equal(h.asked.length, 2);
  assert.equal(sent("StartBacklogItem").length, 2);
});

test("without a RunBacklog route on the daemon the runner bar draws no Run next button", () => {
  backlogRoutes.runBacklog = false;
  const pane = mount(host());
  assert.equal(buttonNamed(el(pane), "Run next")?.hidden ?? true, true);
});

test("Run next starts one item, shows it Starting, and says why when none started", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1, { status: "queued" }), item(2, { status: "queued" })], { 1: "queued", 2: "queued" })), "/a", []);
  reply = async (m) => (m === "RunBacklog" ? { started: [1] } : {});
  buttonNamed(el(pane), "Run next")!.click();
  await settle();
  assert.deepEqual(sent("RunBacklog")[0].body, { repo_path: "/a", count: 1 });
  assert.match(chip(pane, 1), /Starting/);
  assert.match(chip(pane, 2), /Queued/);
  reply = async () => ({ started: [], reason: "all 2 slots are in use; wait for a run to end" });
  buttonNamed(el(pane), "Run next")!.click();
  await settle();
  assert.deepEqual(h.notices, ["All 2 slots are in use; wait for a run to end"]);
});

test("the open question takes an answer, kept across a rerender until it is sent", async () => {
  const h = host();
  const pane = mount(h);
  const q = { id: 4, kind: "question", author: "agent:s1", text: "Which share?", at: "" };
  const v = view([item(6, { status: "running", thread: [q] })], { 6: "needs_you" });
  pane.update(data(v), "/a", []);
  pane.select(6);
  const box = el(pane).querySelector(".af-backlog-question")!;
  assert.equal(box.hidden, false);
  assert.match(box.textContent, /Which share\?/);
  const answer = box.querySelector("textarea")!;
  assert.equal(answer.placeholder, "Answer B-6…");
  answer.value = "30%";
  answer.focus();
  pane.update(data(view([item(6, { status: "running", title: "renamed", thread: [q] })], { 6: "needs_you" }), 2), "/a", []);
  assert.equal(doc.activeElement, answer);
  assert.equal(answer.value, "30%");
  reply = async (m) => {
    if (m === "PostBacklogMessage") throw new Error("B-6 is done; reopen it with \"af backlog reopen 6\" to start a new session with your answer");
    return {};
  };
  buttonNamed(el(pane), "Send answer")!.click();
  await settle();
  assert.deepEqual(sent("PostBacklogMessage")[0].body, { repo_path: "/a", id: 6, kind: "answer", text: "30%", question_id: 4 });
  assert.equal(answer.value, "30%");
  assert.deepEqual(h.notices, ["B-6 is done; reopen it with \"af backlog reopen 6\" to start a new session with your answer"]);
  reply = async () => ({ message: {} });
  buttonNamed(el(pane), "Send answer")!.click();
  await settle();
  assert.equal(answer.value, "");
  pane.update(data(view([item(6, { status: "running", thread: [{ ...q, closed: "answered" }] })], { 6: "working" }), 3), "/a", []);
  assert.equal(box.hidden, true);
});

test("Send to review shows only while af's own question is open", () => {
  const pane = mount(host());
  const cap = { id: 1, kind: "question", author: "af", text: "This run used 11% of your 5-hour window, over the 10% cap per run.", at: "" };
  pane.update(data(view([item(1, { status: "running", thread: [cap] }), item(2, { status: "running", thread: [{ ...cap, author: "agent:s" }] })],
    { 1: "needs_you", 2: "needs_you" })), "/a", []);
  pane.select(1);
  assert.match(el(pane).querySelector(".af-backlog-question")!.textContent, /^af asks: This run used 11%/);
  buttonNamed(el(pane), "Send to review")!.click();
  assert.deepEqual(sent("SetBacklogItemStatus")[0].body, { repo_path: "/a", id: 1, status: "review" });
  pane.select(2);
  assert.equal(buttonNamed(el(pane), "Send to review"), undefined);
});

test("the runner bar shows the window closest to the limit, and both windows on hover", () => {
  const pane = mount(host());
  const v = view([item(1)], {}, { state: "running", used: 1, max: 2, reason: "" });
  v.backlog.settings.runner = true;
  pane.update(data(v), "/a", []);
  const bar = el(pane).querySelector(".af-backlog-head")!;
  const toggle = bar.querySelector('[role="switch"]')!;
  assert.equal(toggle.textContent, "Runner on");
  assert.equal(toggle.getAttribute("aria-checked"), "true");
  const plan = bar.querySelector(".af-backlog-plan")!;
  assert.equal(plan.textContent, "Plan 41% of 5-hour window  limit 50%");
  assert.equal(plan.querySelector(".af-backlog-meter-fill")!.style.width, "82%");
  mock.timers.tick(5 * 60_000);
  plan.dispatchEvent(new FakeEvent("pointerenter"));
  assert.equal(plan.title, "5-hour · plan 41% of 50% limit · resets 14:00\n" +
    "Weekly · plan 40% of 50% limit · resets Mon 09:00\nread 7 min ago · all projects");
  assert.equal(bar.querySelector('select[aria-label="Max running"]')!.value, "2");
  assert.deepEqual(bar.querySelector('select[aria-label="Max running"]')!.children.map((o) => o.textContent).slice(0, 3),
    ["1 of 1 working", "1 of 2 working", "1 of 3 working"]);
});

test("the switch, max running and when-done change this project's runner", () => {
  const pane = mount(host());
  pane.update(data(view([item(1)])), "/a", []);
  const bar = el(pane).querySelector(".af-backlog-head")!;
  bar.querySelector('[role="switch"]')!.click();
  const slots = bar.querySelector('select[aria-label="Max running"]')!;
  slots.value = "4";
  slots.dispatchEvent(new FakeEvent("change"));
  const onDone = bar.querySelector('select[aria-label="When an item is done"]')!;
  assert.equal(onDone.value, "keep");
  onDone.value = "archive";
  onDone.dispatchEvent(new FakeEvent("change"));
  assert.deepEqual(sent("SetBacklogSettings").map((c) => c.body), [
    { repo_path: "/a", runner: true }, { repo_path: "/a", max_running: 4 }, { repo_path: "/a", on_done: "archive" },
  ]);
});

test("budget fields send one change each and refuse values out of range", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1)])), "/a", []);
  const input = (label: string): FakeElement => el(pane).querySelector(`input[aria-label="${label}"]`)!;
  assert.equal(input("Plan limit").value, "50");
  input("Plan limit").value = "120";
  input("Plan limit").dispatchEvent(new FakeEvent("change"));
  assert.equal(input("Plan limit").value, "50");
  input("Plan limit").value = "30";
  input("Plan limit").dispatchEvent(new FakeEvent("change"));
  input("Cap per run").value = "10";
  input("Cap per run").dispatchEvent(new FakeEvent("change"));
  input("Dollar limit").value = "12.5";
  input("Dollar limit").dispatchEvent(new FakeEvent("change"));
  await settle();
  assert.deepEqual(sent("SetBudget").map((c) => c.body), [{ limit: 30 }, { usd_limit: 12.5 }]);
});

test("a budget field changed and changed back before the first write is read sends both, in order", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1)])), "/a", []);
  const input = (label: string): FakeElement => el(pane).querySelector(`input[aria-label="${label}"]`)!;
  const edit = (label: string, v: string): void => {
    input(label).value = v;
    input(label).dispatchEvent(new FakeEvent("change"));
  };
  edit("Plan limit", "30");
  edit("Plan limit", "50"); // the budget shown is still the daemon's 50: this must not read as no change
  edit("Plan limit", "50");
  await settle();
  assert.deepEqual(sent("SetBudget").map((c) => c.body), [{ limit: 30 }, { limit: 50 }], "the last edit is the one saved");
  assert.equal(input("Plan limit").value, "50");
});

test("an earlier write of a value does not retire the draft of a later write of the same value", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1)])), "/a", []);
  const input = (label: string): FakeElement => el(pane).querySelector(`input[aria-label="${label}"]`)!;
  const waiting: (() => void)[] = [];
  reply = (m) => (m === "SetBudget" ? new Promise((r) => waiting.push(() => r({}))) : Promise.resolve({}));
  for (const v of ["30", "40", "30"]) {
    input("Plan limit").value = v;
    input("Plan limit").dispatchEvent(new FakeEvent("change"));
  }
  await settle();
  assert.equal(waiting.length, 1, "writes go out one at a time");
  waiting.shift()!();
  await settle();
  assert.equal(input("Plan limit").value, "30", "the first reply retires the first edit only; the last 30 is still pending");
  while (waiting.length > 0) {
    waiting.shift()!();
    await settle();
  }
  assert.deepEqual(sent("SetBudget").map((c) => c.body), [{ limit: 30 }, { limit: 40 }, { limit: 30 }]);
});

test("a saved budget is applied from the SetBudget reply, not only from the read after it", async () => {
  const h = host();
  const applied: unknown[] = [];
  h.applyBudget = (r) => { applied.push(r); };
  const pane = mount(h);
  pane.update(data(view([item(1)])), "/a", []);
  const f = el(pane).querySelector('input[aria-label="Dollar limit"]')!;
  reply = async (m) => (m === "SetBudget" ? { budget: { ...BUDGET, usd_limit: 5 } } : {});
  f.value = "5";
  f.dispatchEvent(new FakeEvent("change"));
  await settle();
  assert.equal(applied.length, 1);
  assert.equal((applied[0] as { budget: Budget }).budget.usd_limit, 5);
});

test("an optimistic Starting is dropped when the daemon, read after the reply, says the item stopped", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1)], { 1: "draft" })), "/a", []);
  pane.select(1);
  reply = async (m) => (m === "StartBacklogItem" ? { run: { session_id: "", state: "starting" } } : {});
  buttonNamed(el(pane), "Start now")!.click();
  await settle();
  assert.match(chip(pane, 1), /Starting/);
  // The session was killed before it reached the session list: the run stays "started", the item derives Stopped.
  const run = { session_id: "s1", title: "B-1 item 1", state: "started", prev_status: "draft" as const, started_by: "user", started_at: "" };
  const killed = view([item(1, { status: "running", runs: [run] })], { 1: "stopped" });
  pane.update(data(killed, 3), "/a", []);
  assert.doesNotMatch(chip(pane, 1), /Starting/);
  assert.notEqual(buttonNamed(el(pane), "Start now"), undefined, "Start now is offered again without a reload");
});

test("a paused runner says why and offers the main action ui.md names", () => {
  const pane = mount(host());
  const paused = (gate: string, reason: string, items: Item[] = [item(1)]): void =>
    pane.update(data(view(items, {}, { state: gate === "no_reading" ? "waiting" : "stopped", gate, reason })), "/a", []);
  const bar = (): FakeElement => el(pane).querySelector(".af-backlog-head")!;
  paused("limit", "Plan at 52% of the 5-hour window (limit 50%) · resumes 14:00");
  assert.equal(bar().querySelector('[role="switch"]')!.textContent, "Runner paused");
  assert.ok(bar().querySelector(".af-backlog-runner-state")!.classList.contains("af-backlog-warn"));
  assert.match(bar().textContent, /· Plan at 52% of the 5-hour window \(limit 50%\) · resumes 14:00/);
  buttonNamed(bar(), "Change limit")!.click();
  assert.equal(bar().querySelector("details.af-backlog-settings")!.open, true);
  assert.equal(doc.activeElement, bar().querySelector('input[aria-label="Plan limit"]'));
  paused("usage_limit", "Usage limit until 14:00");
  assert.equal(buttonNamed(bar(), "Change limit"), undefined);
  assert.equal(buttonNamed(bar(), "Run next")!.className, "af-ghost");
  paused("no_reading", "No plan usage seen yet · start an item by hand once to take a reading");
  assert.equal(bar().querySelector('[role="switch"]')!.textContent, "Runner waiting");
  assert.equal(buttonNamed(bar(), "Run next")!.className, "af-primary");
  const run = { session_id: "s7", title: "B-7 runner", state: "started", prev_status: "queued" as const, started_by: "runner", started_at: "" };
  pane.update(data(view([item(7, { status: "running", runs: [run] })], {}, { state: "stopped", gate: "unmeterable", reason: "The runner can't meter aider" }),
    1, usage({ unmeterable: { s7: "the runner can't meter aider" } })), "/a", []);
  buttonNamed(bar(), "Open B-7")!.click();
  assert.equal(el(pane).querySelector(".af-backlog-panel")?.getAttribute("aria-label"), "Item B-7");
});

test("a run shows its use of the 5-hour window and its API cost", () => {
  const pane = mount(host());
  const run = { session_id: "s1", title: "B-1 item 1", state: "started", prev_status: "draft" as const, started_by: "user", started_at: "" };
  pane.update(data(view([item(1, { status: "running", runs: [run] })], { 1: "working" }), 1,
    usage({ runs: { s1: 9 }, run_window: { s1: 2 } })), "/a", [session("s1")]);
  pane.select(1);
  const use = el(pane).querySelector(".af-backlog-run-use")!;
  assert.equal(use.textContent, "4.1% of the 5-hour window · $9.00 at API prices");
  assert.equal(use.title, "$2.00 in this 5-hour window");
  pane.update(data(view([item(1, { status: "running", runs: [run] })], { 1: "working" }), 2,
    usage({ runs: { s1: 9 }, run_window: { s1: 2 }, five_hour: { plan: 0, resets_at: null } })), "/a", [session("s1")]);
  assert.equal(el(pane).querySelector(".af-backlog-run-use")!.textContent, "$9.00 at API prices");
});

test("a second click while a start or a run is out does nothing", async () => {
  const h = host();
  const pane = mount(h);
  pane.update(data(view([item(1, { status: "queued" })], { 1: "queued" })), "/a", []);
  pane.select(1);
  let release: () => void = () => {};
  h.confirm = () => new Promise((r) => { release = () => r(true); });
  pane.update(data(view([item(1, { status: "queued" })], { 1: "queued" }), 2,
    usage({ seven_day: { plan: 60, resets_at: iso("2000-01-10T09:00:00") } })), "/a", []);
  const start = buttonNamed(el(pane), "Start now")!;
  start.click();
  start.click();
  release();
  await settle();
  assert.equal(sent("StartBacklogItem").length, 1);
  let done: (v: unknown) => void = () => {};
  reply = (m) => (m === "RunBacklog" ? new Promise((r) => { done = r; }) : Promise.resolve({}));
  const run = buttonNamed(el(pane), "Run next")!;
  run.click();
  await settle();
  assert.equal(run.disabled, true);
  run.click();
  done({ started: [] });
  await settle();
  assert.equal(sent("RunBacklog").length, 1);
  assert.equal(buttonNamed(el(pane), "Run next")!.disabled, false);
});

test("focus stays on the max running select and on an item action through a spend render", () => {
  const pane = mount(host());
  const v = view([item(1, { status: "queued" })], { 1: "queued" });
  pane.update(data(v), "/a", []);
  pane.select(1);
  const slots = el(pane).querySelector('select[aria-label="Max running"]')!;
  const options = slots.children;
  slots.focus();
  pane.update(data(v, 1, usage({ runs: { s1: 1 } })), "/a", []);
  assert.equal(doc.activeElement, slots);
  assert.deepEqual(slots.children, options, "the options are not rebuilt");
  buttonNamed(el(pane), "Mark done")!.focus();
  pane.update(data(v, 1, usage({ runs: { s1: 2 } })), "/a", []);
  assert.equal(doc.activeElement?.textContent, "Mark done");
  assert.ok(el(pane).contains(doc.activeElement));
});

test("when Start now goes away its focus moves to the next action", async () => {
  const pane = mount(host());
  pane.update(data(view([item(1)], { 1: "draft" })), "/a", []);
  pane.select(1);
  reply = async () => ({ run: { session_id: "", state: "starting" } });
  const start = buttonNamed(el(pane), "Start now")!;
  start.focus();
  start.click();
  await settle();
  assert.equal(buttonNamed(el(pane), "Start now"), undefined);
  assert.equal(doc.activeElement?.dataset.action, "queue");
});

test("the plan use opens its detail on tap, and daemon reasons lose their CLI hints", () => {
  const pane = mount(host());
  pane.update(data(view([item(1)], {}, { state: "stopped", gate: "limit", reason: 'Plan limit is 0% · raise it with "af backlog budget --limit 50"' })), "/a", []);
  const bar = el(pane).querySelector(".af-backlog-head")!;
  assert.match(bar.textContent, /· Plan limit is 0%/);
  assert.doesNotMatch(bar.textContent, /af backlog/);
  const plan = bar.querySelector("button.af-backlog-plan")!;
  const detail = bar.querySelector(".af-backlog-plan-detail")!;
  assert.equal(detail.hidden, true);
  plan.click();
  assert.equal(plan.getAttribute("aria-expanded"), "true");
  assert.deepEqual(detail.children.map((c) => c.textContent), [
    "5-hour · plan 41% of 50% limit · resets 14:00",
    "Weekly · plan 40% of 50% limit · resets Mon 09:00",
    "read 2 min ago · all projects",
  ]);
  plan.click();
  assert.equal(detail.hidden, true);
});
