import { test } from "node:test";
import assert from "node:assert/strict";

import {
  type Backlog, type BacklogView, type Item, type ListOptions, type State,
  applyMove, canStartNow, dropMove, itemActions, keyboardMove, listRows, needsYouByRoot, needsYouCount, needsYouLabel, neededBy, openQuestion, parseBacklogView, safeHttpUrl,
  sessionItems, sessionItemsSig, sources, topQueued,
} from "./backlog_model.js";
import { waitsOnBlock } from "./backlog_cycle.js";

function item(id: number, over: Partial<Item> = {}): Item {
  return { id, title: `item ${id}`, status: "draft", created_at: "", updated_at: "", ...over };
}

function backlog(...items: Item[]): Backlog {
  return { schema_version: 1, next_id: 100, settings: { runner: false, max_running: 1, on_done: "keep" }, items };
}

function view(b: Backlog, states: Record<number, State | [State, string]> = {}): BacklogView {
  const out: BacklogView["states"] = {};
  for (const [id, s] of Object.entries(states)) {
    out[id] = Array.isArray(s) ? { state: s[0], reason: s[1] } : { state: s };
  }
  return { backlog: b, states: out, runner: { state: "off", used: 0, max: 1 } };
}

const ALL: ListOptions = { filter: "all", showDone: true, folded: new Set() };

function ids(b: Backlog): number[] {
  return b.items.map((it) => it.id);
}

// 1 { 2, 3 { 4 } }, 5, 6
function tree(): Backlog {
  return backlog(item(1), item(2, { parent: 1 }), item(3, { parent: 1 }), item(4, { parent: 3 }), item(5), item(6));
}

test("parseBacklogView fills the arrays Go leaves out", () => {
  const v = parseBacklogView({ backlog: { items: [{ id: 1, title: "a", status: "draft" }] } });
  assert.deepEqual(v.backlog.items[0].runs, []);
  assert.deepEqual(v.backlog.items[0].waits_on, []);
  assert.deepEqual(v.backlog.items[0].thread, []);
  assert.deepEqual(v.states, {});
  assert.equal(v.runner.state, "off");
});

test("listRows keeps tree order and depth", () => {
  const rows = listRows(view(tree()), ALL);
  assert.deepEqual(rows.map((r) => [r.item.id, r.depth, r.parent]), [
    [1, 0, true], [2, 1, false], [3, 1, true], [4, 2, false], [5, 0, false], [6, 0, false],
  ]);
});

test("listRows hides a folded parent's subtree", () => {
  const rows = listRows(view(tree()), { ...ALL, folded: new Set([3]) });
  assert.deepEqual(rows.map((r) => r.item.id), [1, 2, 3, 5, 6]);
  assert.equal(rows[2].folded, true);
});

test("listRows hides done and dropped rows and their subtrees unless asked", () => {
  const v = view(tree(), { 3: "done", 4: "done", 6: "dropped" });
  assert.deepEqual(listRows(v, { ...ALL, showDone: false }).map((r) => r.item.id), [1, 2, 5]);
  assert.deepEqual(listRows(v, ALL).map((r) => r.item.id), [1, 2, 3, 4, 5, 6]);
});

test("listRows filter keeps the ancestors of a match", () => {
  const v = view(tree(), { 4: "waiting", 5: "starting", 6: "working" });
  assert.deepEqual(listRows(v, { ...ALL, filter: "waiting" }).map((r) => r.item.id), [1, 3, 4]);
  assert.deepEqual(listRows(v, { ...ALL, filter: "working" }).map((r) => r.item.id), [5, 6]);
});

test("listRows adds the needs-you count to a parent's reason", () => {
  const b = tree();
  b.items[3].thread = [{ id: 1, kind: "question", author: "agent:x", text: "?", at: "" }];
  const rows = listRows(view(b, { 1: ["needs_you", "0 of 2 done"] }), ALL);
  assert.equal(rows[0].reason, "0 of 2 done · 1 needs you");
  assert.equal(rows[2].reason, "1 needs you");
});

// Mirrors backlog.Message.closesQuestion and Item.AnswerStarting in Go: an
// answer kept for a new run (the asking session is gone) or one a later answer
// replaced leaves its question open, but not while the run carrying it starts.
test("openQuestion stays open with an answer kept for a new run, but not while that run starts", () => {
  const q = { id: 1, kind: "question", author: "agent:x", text: "q", at: "" };
  const kept = { id: 2, kind: "answer", author: "user", text: "a", answers: 1, delivery: "new_run", at: "" };
  const run = { session_id: "", title: "", state: "failed", prev_status: "review" as const, started_by: "user", started_at: "", answers: [2] };
  assert.equal(openQuestion(item(1, { thread: [q, kept], runs: [run] }))?.id, 1);
  assert.equal(openQuestion(item(1, { thread: [q, { ...kept, delivery: "superseded" }] }))?.id, 1);
  assert.equal(openQuestion(item(1, { thread: [q, kept], runs: [{ ...run, state: "starting" }] })), undefined);
  assert.equal(openQuestion(item(1, { thread: [q, { ...kept, delivery: "delivered" }] })), undefined);
  assert.equal(needsYouCount([{ backlog: backlog(item(1, { thread: [q, kept], runs: [{ ...run, state: "starting" }] })), states: {} } as BacklogView]), 0);
});

test("needsYouCount counts open questions once across projects", () => {
  const a = tree();
  a.items[1].thread = [
    { id: 1, kind: "question", author: "agent:x", text: "q1", at: "" },
    { id: 2, kind: "answer", author: "user", text: "a1", answers: 1, at: "" },
  ];
  a.items[3].thread = [{ id: 1, kind: "question", author: "agent:x", text: "q", at: "" }];
  const b = backlog(item(1, { thread: [{ id: 1, kind: "question", author: "af", text: "q", closed: "done", at: "" }] }),
    item(2, { thread: [{ id: 1, kind: "question", author: "af", text: "q", at: "" }] }));
  assert.equal(needsYouCount([view(a), view(b)]), 2);
});

test("needsYouByRoot keys each project's count and drops projects with none", () => {
  const q = (id: number) => item(id, { thread: [{ id: 1, kind: "question", author: "agent:x", text: "q", at: "" }] });
  const views = new Map([["/detail", view(backlog(q(1)))], ["/sahara", view(backlog(q(1), q(2)))], ["/empty", view(backlog(item(1)))]]);
  assert.deepEqual([...needsYouByRoot(views)], [["/detail", 1], ["/sahara", 2]]);
});

test("sessionItems maps run session ids to items", () => {
  const b = backlog(item(7, { runs: [
    { session_id: "s1", title: "B-7 a", state: "started", prev_status: "queued", started_by: "user", started_at: "" },
    { session_id: "", title: "B-7 a", state: "starting", prev_status: "queued", started_by: "user", started_at: "" },
  ] }));
  const m = sessionItems(new Map([["/repo", view(b)]]));
  assert.deepEqual([...m.entries()], [["s1", { root: "/repo", id: 7, title: "item 7", state: "draft" }]]);
});

test("keyboardMove steps among siblings and nests", () => {
  const b = tree();
  assert.deepEqual(keyboardMove(b, 3, "up"), { before: 2 });
  assert.equal(keyboardMove(b, 2, "up"), null);
  assert.deepEqual(keyboardMove(b, 5, "down"), { after: 6 });
  assert.equal(keyboardMove(b, 6, "down"), null);
  assert.deepEqual(keyboardMove(b, 6, "in"), { into: 5 });
  assert.equal(keyboardMove(b, 1, "in"), null);
  assert.deepEqual(keyboardMove(b, 4, "out"), { after: 3 });
  assert.equal(keyboardMove(b, 5, "out"), null);
});

test("keyboardMove will not nest under an item with runs", () => {
  const b = tree();
  b.items[4].runs = [{ session_id: "s", title: "", state: "started", prev_status: "draft", started_by: "user", started_at: "" }];
  assert.equal(keyboardMove(b, 6, "in"), null);
});

test("dropMove refuses a drop into the dragged subtree", () => {
  const b = tree();
  assert.equal(dropMove(b, 1, 4, "before"), null);
  assert.equal(dropMove(b, 3, 3, "after"), null);
  assert.deepEqual(dropMove(b, 6, 2, "into"), { into: 2 });
  assert.deepEqual(dropMove(b, 6, 1, "before"), { before: 1 });
});

test("applyMove moves the whole subtree", () => {
  const b = tree();
  assert.deepEqual(ids(applyMove(b, 3, { after: 5 }) as Backlog), [1, 2, 5, 3, 4, 6]);
  assert.deepEqual(ids(applyMove(b, 1, { bottom: true }) as Backlog), [5, 6, 1, 2, 3, 4]);
  assert.deepEqual(ids(applyMove(b, 6, { top: true }) as Backlog), [6, 1, 2, 3, 4, 5]);
  const into = applyMove(b, 5, { into: 3 }) as Backlog;
  assert.deepEqual(ids(into), [1, 2, 3, 4, 5, 6]);
  assert.equal(into.items.find((it) => it.id === 5)?.parent, 3);
  const out = applyMove(b, 4, { after: 3 }) as Backlog;
  assert.equal(out.items.find((it) => it.id === 4)?.parent, 1);
  assert.equal(applyMove(b, 1, { into: 4 }), null);
  assert.deepEqual(ids(b), [1, 2, 3, 4, 5, 6], "the input is unchanged");
});

test("neededBy lists the items that wait on an item", () => {
  const b = backlog(item(1), item(2, { waits_on: [1] }), item(3, { waits_on: [2, 1] }));
  assert.deepEqual(neededBy(b, 1).map((it) => it.id), [2, 3]);
});

// The same cases as backlog/cycle_test.go TestCheckWaitsOn.
test("waitsOnBlock matches the daemon's cycle rule", () => {
  const build = (seq: boolean): Backlog => backlog(
    item(1, { sequential: seq }), item(2, { parent: 1 }), item(3, { parent: 1 }), item(4, { parent: 3 }),
    item(5, { waits_on: [6] }), item(6), item(7, { waits_on: [5] }),
  );
  const cases: Array<[number, number, boolean, string | null]> = [
    [2, 3, false, null],
    [5, 2, false, null],
    [4, 6, false, null],
    [1, 7, false, null],
    [5, 5, false, "B-5 cannot wait on itself"],
    [6, 5, false, "B-6 cannot wait on B-5: B-5 already waits on B-6 (B-6 → B-5 → B-6)"],
    [6, 7, false, "B-6 cannot wait on B-7: B-7 already waits on B-6 (B-6 → B-7 → B-5 → B-6)"],
    [4, 1, false, "B-4 cannot wait on B-1: B-1 contains it"],
    [1, 4, false, "B-1 cannot wait on B-4: B-4 is inside it"],
    [1, 9, false, "B-9 not found"],
    [6, 1, false, null],
    [2, 3, true, "B-2 cannot wait on B-3: that would make a wait cycle (B-2 → B-3"],
    [3, 2, true, null],
  ];
  for (const [id, on, seq, want] of cases) {
    const got = waitsOnBlock(build(seq), id, on);
    if (want === null) assert.equal(got, null, `${id} on ${on}`);
    else assert.ok(got?.startsWith(want), `${id} on ${on}: ${got}`);
  }
});

test("waitsOnBlock follows a wait through a parent", () => {
  const b = backlog(item(1, { waits_on: [3] }), item(2, { parent: 1 }), item(3));
  assert.equal(waitsOnBlock(b, 3, 2), "B-3 cannot wait on B-2: that would make a wait cycle (B-3 → B-2 → B-1 → B-3)");
});

test("itemActions offers only the changes the daemon allows", () => {
  const b = tree();
  const at = (id: number, over: Partial<Item>, state: State = "draft"): string[] => {
    Object.assign(b.items.find((it) => it.id === id) as Item, over);
    return itemActions(b, b.items.find((it) => it.id === id) as Item, { state });
  };
  assert.deepEqual(at(5, { status: "draft" }), ["queue", "start", "done", "drop", "delete"]);
  assert.deepEqual(at(5, { status: "queued" }, "queued"), ["draft", "start", "done", "drop"]);
  assert.deepEqual(at(5, { status: "queued" }, "waiting"), ["draft", "start", "done", "drop"]);
  assert.deepEqual(at(5, { status: "review" }, "in_review"), ["queue", "draft", "start", "done", "drop"]);
  assert.deepEqual(at(5, { status: "running" }, "working"), ["done", "drop"]);
  assert.deepEqual(at(5, { status: "running" }, "starting"), ["done", "drop"]);
  assert.deepEqual(at(5, { status: "running" }, "stopped"), ["start", "done", "drop"]);
  const capQ = { id: 1, kind: "question", author: "af", text: "This run used 11%…", at: "" };
  assert.deepEqual(at(5, { status: "running", thread: [capQ] }, "needs_you"), ["review", "done", "drop"]);
  const agentQ = { ...capQ, author: "agent:s1" };
  assert.deepEqual(at(5, { status: "running", thread: [agentQ] }, "needs_you"), ["done", "drop"]);
  assert.deepEqual(at(5, { status: "done", thread: [] }, "done"), ["reopen"]);
  assert.deepEqual(at(5, { status: "dropped", thread: [] }, "dropped"), ["reopen"]);
  assert.deepEqual(at(6, { status: "draft", thread: [{ id: 1, kind: "note", author: "user", text: "n", at: "" }] }),
    ["queue", "start", "done", "drop"]);
  assert.deepEqual(at(1, {}), ["queue", "drop"]);
  for (const id of [2, 4]) Object.assign(b.items.find((it) => it.id === id) as Item, { status: "queued" });
  assert.deepEqual(at(1, {}, "queued"), ["drop"]);
  assert.deepEqual(at(1, {}, "done"), []);
});

test("topQueued is the first queued leaf in tree order", () => {
  const b = tree();
  const v = view(b);
  assert.equal(topQueued(v), undefined);
  v.states = { 1: { state: "queued" }, 3: { state: "waiting" }, 4: { state: "queued" }, 5: { state: "queued" } };
  assert.equal(topQueued(v)?.id, 4);
});

test("listRows source filter keeps a proposal's items", () => {
  const b = tree();
  b.items[1].source = { proposal: "docs", key: "a" };
  b.items[4].source = { proposal: "other", key: "b" };
  assert.deepEqual(sources(b), ["docs", "other"]);
  assert.deepEqual(listRows(view(b), { ...ALL, source: "docs" }).map((r) => r.item.id), [1, 2]);
});

test("safeHttpUrl keeps http and https links only", () => {
  assert.equal(safeHttpUrl("https://github.com/o/r/pull/1"), "https://github.com/o/r/pull/1");
  assert.equal(safeHttpUrl("http://x.test/p"), "http://x.test/p");
  for (const bad of ["javascript:alert(1)", "JAVASCRIPT:alert(1)", "data:text/html,x", "/pull/1", "", undefined, "https://"]) {
    assert.equal(safeHttpUrl(bad), null, String(bad));
  }
});

test("needsYouLabel agrees with its count", () => {
  assert.equal(needsYouLabel(1), "1 needs you");
  assert.equal(needsYouLabel(3), "3 need you");
});

test("canStartNow offers Start now only where the daemon's start rule accepts it", () => {
  const b = tree();
  const leaf = b.items.find((it) => it.id === 5) as Item;
  const parent = b.items.find((it) => it.id === 1) as Item;
  const ok = (state: State, status: Item["status"] = "queued"): boolean => canStartNow(b, { ...leaf, status }, { state });
  assert.equal(canStartNow(b, parent, { state: "queued" }), false, "a parent runs through its children");
  for (const status of ["done", "dropped"] as const) assert.equal(ok(status, status), false, `${status} must be reopened first`);
  assert.equal(ok("starting", "running"), false, "already starting");
  assert.equal(ok("working", "running"), false, "an active run is already working");
  assert.equal(ok("needs_you", "running"), false, "the answer is the next step");
  for (const state of ["draft", "queued", "waiting", "in_review", "stopped"] as const) assert.equal(ok(state), true, state);
});

test("a rebound project's chips are redrawn: the signature includes the root", () => {
  const b = backlog(item(1, { runs: [{ session_id: "s1", title: "B-1 a", state: "started", prev_status: "queued", started_by: "user", started_at: "" }] }));
  const at = (root: string): string => sessionItemsSig(sessionItems(new Map([[root, view(b)]])));
  assert.notEqual(at("/old"), at("/new"));
  assert.equal(at("/old"), at("/old"));
});
