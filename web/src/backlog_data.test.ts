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

test("a spend event refetches only the budget", async () => {
  const l = loader();
  void l.refresh();
  await settle();
  answer("GetBacklog", backlogOf("a"));
  answer("GetBudget", budgetOf(1));
  await settle();
  const views = state.views;
  l.requestBudget();
  l.requestBudget();
  mock.timers.tick(150);
  await settle();
  assert.deepEqual(pending.map((p) => p.method), ["GetBudget"]);
  answer("GetBudget", budgetOf(2.5));
  await settle();
  assert.equal(state.budget?.usd_spent, 2.5);
  assert.equal(state.views, views);
  assert.equal(state.generation, 1);
});

import { backlogEventAction, runSessionIds } from "./backlog_data.js";

test("every backlog.changed frame is a full re-read, whatever its ids say", () => {
  // The frame's ids are not an input: a parent's derived state moves unnamed, and an
  // empty list means "refresh the project". The decision is on the type alone.
  assert.equal(backlogEventAction("backlog.changed"), "refresh");
  assert.equal(backlogEventAction("backlog.runner"), "refresh");
  assert.equal(backlogEventAction("backlog.spend"), "budget");
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
    errors: new Map(), budget: null, generation: 1,
  });
  assert.deepEqual([...ids].sort(), ["s1", "s2"]);
});
