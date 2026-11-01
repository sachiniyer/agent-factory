import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type BacklogData, BacklogLoader, EMPTY_BACKLOG_DATA } from "./backlog_data.js";

interface Pending {
  method: string;
  root: string;
  resolve(data: unknown): void;
  reject(message: string): void;
}

let pending: Pending[];
let state: BacklogData;
let commits: number;
const realFetch = globalThis.fetch;

beforeEach(() => {
  pending = [];
  state = EMPTY_BACKLOG_DATA;
  commits = 0;
  (globalThis as { fetch: unknown }).fetch = (url: string, init: RequestInit) => new Promise<Response>((done) => {
    const body = JSON.parse(String(init.body)) as { repo_path?: string };
    pending.push({
      method: url.replace("/v1/", ""),
      root: body.repo_path ?? "",
      resolve: (data) => done(new Response(JSON.stringify({ data, error: null }))),
      reject: (message) => done(new Response(JSON.stringify({ data: null, error: { message } }), { status: 500 })),
    });
  });
  mock.timers.enable({ apis: ["setTimeout"] });
});

afterEach(() => {
  mock.timers.reset();
  (globalThis as { fetch: unknown }).fetch = realFetch;
});

const settle = async (): Promise<void> => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

function loader(roots = ["/a"]): BacklogLoader {
  return new BacklogLoader({
    token: () => "t",
    roots: () => roots,
    current: () => state,
    commit: (d) => {
      state = d;
      commits++;
    },
  });
}

const backlogOf = (title: string): unknown => ({ backlog: { items: [{ id: 1, title, status: "draft" }] } });
const budgetOf = (spent: number): unknown => ({ budget: { share: 30, stop_at: 80, max_per_run: 10, usd_limit: 5, usd_spent: spent } });

function answer(method: string, data: unknown, root = ""): void {
  const i = pending.findIndex((p) => p.method === method && (root === "" || p.root === root));
  assert.ok(i >= 0, `no pending ${method}`);
  pending.splice(i, 1)[0].resolve(data);
}

test("a burst of events makes one refetch after the debounce", async () => {
  const l = loader();
  l.request();
  l.request();
  l.request();
  assert.equal(pending.length, 0);
  mock.timers.tick(149);
  assert.equal(pending.length, 0);
  mock.timers.tick(1);
  await settle();
  assert.deepEqual(pending.map((p) => p.method).sort(), ["GetBacklog", "GetBudget"]);
});

test("a later refresh wins over one still in flight, and stamps its generation", async () => {
  const l = loader();
  void l.refresh();
  void l.refresh();
  await settle();
  assert.equal(l.started(), 2);
  const [first, second] = pending.filter((p) => p.method === "GetBacklog");
  second.resolve(backlogOf("new"));
  answer("GetBudget", budgetOf(1));
  answer("GetBudget", budgetOf(2));
  await settle();
  first.resolve(backlogOf("old"));
  await settle();
  assert.equal(commits, 1);
  assert.equal(state.generation, 2);
  assert.equal(state.views.get("/a")?.backlog.items[0].title, "new");
});

test("a failed refetch keeps the last view and records the error", async () => {
  const l = loader();
  void l.refresh();
  await settle();
  answer("GetBacklog", backlogOf("kept"));
  answer("GetBudget", budgetOf(1));
  await settle();
  void l.refresh();
  await settle();
  pending.find((p) => p.method === "GetBacklog")!.reject("project gone");
  answer("GetBudget", budgetOf(1));
  await settle();
  assert.equal(state.views.get("/a")?.backlog.items[0].title, "kept");
  assert.match(state.errors.get("/a") ?? "", /project gone/);
  assert.equal(state.budget?.usd_spent, 1);
});

test("a spend event applies in place and wins over a budget read before it", async () => {
  const l = loader();
  void l.refresh();
  await settle();
  answer("GetBacklog", backlogOf("a"));
  answer("GetBudget", budgetOf(1));
  await settle();
  const views = state.views;
  void l.refresh();
  await settle();
  l.applySpend({
    five_hour: { plan: 40, backlog: 12, resets_at: "2000-01-01T05:00:00Z", all_cost: 20 },
    seven_day: { plan: 30, backlog: 18, resets_at: null },
    reading_at: "2000-01-01T00:00:00Z",
    runs: { s1: 1.25 },
  });
  assert.deepEqual(pending.map((p) => p.method).sort(), ["GetBacklog", "GetBudget"]);
  assert.equal(state.usage?.five_hour.backlog, 12);
  assert.equal(state.usage?.runs.s1, 1.25);
  assert.equal(state.views, views);
  answer("GetBacklog", backlogOf("b"));
  answer("GetBudget", { ...(budgetOf(2) as object), windows: { five_hour: { plan: 1, backlog: 1, resets_at: null } }, runs: {},
    unmeterable: { s7: "the runner can't meter aider" } });
  await settle();
  assert.equal(state.budget?.usd_spent, 2);
  assert.equal(state.usage?.five_hour.backlog, 12);
  assert.equal(state.usage?.runs.s1, 1.25);
  assert.deepEqual(state.usage?.unmeterable, { s7: "the runner can't meter aider" }, "an older spend payload names no unmeterable runs");
});

test("unmeterable runs follow the newest source that names them", async () => {
  const l = loader();
  void l.refresh();
  await settle();
  answer("GetBacklog", backlogOf("a"));
  answer("GetBudget", { ...(budgetOf(1) as object), unmeterable: { s7: "old" } });
  await settle();
  void l.refresh();
  await settle();
  l.applySpend({ five_hour: {}, seven_day: {}, reading_at: null, runs: {}, run_window: { s1: 0.5 }, unmeterable: {} });
  assert.deepEqual(state.usage?.unmeterable, {});
  assert.deepEqual(state.usage?.run_window, { s1: 0.5 });
  answer("GetBacklog", backlogOf("a"));
  answer("GetBudget", { ...(budgetOf(1) as object), unmeterable: { s7: "old" } });
  await settle();
  assert.deepEqual(state.usage?.unmeterable, {}, "a reply read before the spend event does not bring back a cleared run");
  void l.refresh();
  await settle();
  answer("GetBacklog", backlogOf("a"));
  answer("GetBudget", { ...(budgetOf(1) as object), unmeterable: { s8: "new" } });
  await settle();
  assert.deepEqual(state.usage?.unmeterable, { s8: "new" });
});

import { backlogEventAction, runSessionIds } from "./backlog_data.js";

test("every backlog.changed frame is a full re-read, whatever its ids say", () => {
  // The frame's ids are not an input: a parent's derived state moves unnamed, and an
  // empty list means "refresh the project". The decision is on the type alone.
  assert.equal(backlogEventAction("backlog.changed"), "refresh");
  assert.equal(backlogEventAction("backlog.runner"), "refresh");
  assert.equal(backlogEventAction("backlog.spend"), "spend");
  assert.equal(backlogEventAction("session.updated"), null);
});

test("a lifecycle event for a session an item run points at is a refresh; any other is not", () => {
  const runs = new Set(["s-run"]);
  for (const type of ["session.killed", "session.archived", "session.restored", "session.updated"]) {
    assert.equal(backlogEventAction(type, "s-run", runs), "refresh", type);
    assert.equal(backlogEventAction(type, "s-other", runs), null, type);
    assert.equal(backlogEventAction(type, undefined, runs), null, type);
  }
  assert.equal(backlogEventAction("session.created", "s-run", runs), null);
  assert.equal(backlogEventAction("session.killed", "s-run"), null);
});

test("runSessionIds collects every run's session across projects", () => {
  const run = (session_id: string) => ({ session_id });
  const mk = (...items: unknown[]) => ({ backlog: { items } }) as never;
  const ids = runSessionIds({
    views: new Map([["/a", mk({ runs: [run("s1"), run("")] }, { runs: undefined })], ["/b", mk({ runs: [run("s2")] })]]),
    errors: new Map(), budget: null, usage: null, generation: 1,
  });
  assert.deepEqual([...ids].sort(), ["s1", "s2"]);
});

test("refresh settles true only when the named project's read succeeded", async () => {
  const l = loader(["/a", "/b"]);
  const ok = l.refresh("/a");
  await settle();
  answer("GetBacklog", backlogOf("a"), "/a");
  pending.find((p) => p.method === "GetBacklog" && p.root === "/b")!.reject("busy");
  pending.splice(pending.findIndex((p) => p.root === "/b"), 1);
  answer("GetBudget", budgetOf(1));
  assert.equal(await ok, true, "/b failing does not fail an edit in /a");
  const bad = l.refresh("/a");
  await settle();
  pending.find((p) => p.method === "GetBacklog" && p.root === "/a")!.reject("down");
  pending.splice(pending.findIndex((p) => p.method === "GetBacklog" && p.root === "/a"), 1);
  answer("GetBacklog", backlogOf("b"), "/b");
  answer("GetBudget", budgetOf(1));
  assert.equal(await bad, false, "a failed read of the edited project is not a confirmation");
});

test("a refresh a newer one superseded is acknowledged only by what the newer one commits", async () => {
  const l = loader(["/a"]);
  const confirming = l.refresh("/a");
  await settle();
  const first = pending.splice(0);
  const newer = l.refresh();
  await settle();
  first.find((p) => p.method === "GetBacklog")!.resolve(backlogOf("saved"));
  first.find((p) => p.method === "GetBudget")!.resolve(budgetOf(1));
  await settle();
  pending.find((p) => p.method === "GetBacklog")!.reject("down");
  pending.splice(pending.findIndex((p) => p.method === "GetBacklog"), 1);
  answer("GetBudget", budgetOf(1));
  assert.equal(await newer, false);
  assert.equal(await confirming, false, "its own read was discarded and the newer read failed");
  assert.equal(commits, 1, "the failed newer read still commits (keeping the old snapshot)");

  const again = l.refresh("/a");
  await settle();
  const second = pending.splice(0);
  const newest = l.refresh();
  await settle();
  second.find((p) => p.method === "GetBacklog")!.resolve(backlogOf("stale"));
  second.find((p) => p.method === "GetBudget")!.resolve(budgetOf(1));
  await settle();
  answer("GetBacklog", backlogOf("saved"));
  answer("GetBudget", budgetOf(1));
  assert.equal(await newest, true);
  assert.equal(await again, true, "the newer snapshot that carries the edit committed");
});

test("a spend event is followed by a budget read that renews which account the windows are for", async () => {
  const l = loader();
  state = { ...EMPTY_BACKLOG_DATA, usage: { metering: true, account: "default", accounts: { default: {} as never }, five_hour: {} as never, seven_day: {} as never, reading_at: null, runs: {}, run_window: {}, unmeterable: {} } as never };
  l.applySpend({ five_hour: {}, seven_day: {}, reading_at: null, runs: {}, run_window: {}, unmeterable: {} });
  assert.equal(pending.length, 0, "debounced");
  mock.timers.tick(150);
  await settle();
  assert.deepEqual(pending.map((p) => p.method), ["GetBudget"]);
  answer("GetBudget", { budget: { share: 30, stop_at: 80, max_per_run: 10, usd_limit: 0, usd_spent: 0 }, account: "work", accounts: { work: { windows: { five_hour: { plan: 20 } } } }, windows: {} });
  await settle();
  assert.equal(state.usage?.account, "work", "the account the daemon now meters replaces the one kept");
});
