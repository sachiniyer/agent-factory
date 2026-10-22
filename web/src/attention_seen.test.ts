import assert from "node:assert/strict";
import test from "node:test";

import { AttentionClearer, attentionSeen, type AttentionView } from "./attention_seen.js";
import { Liveness, type SessionData } from "./types.js";

function session(id: string, setAt: string | null): SessionData {
  return {
    id,
    title: id,
    liveness: Liveness.Ready,
    attention: setAt ? { set_at: setAt } : undefined,
  } as SessionData;
}

function view(over: Partial<AttentionView> = {}): AttentionView {
  return {
    phase: "app",
    view: "sessions",
    sessions: [session("a", "t1"), session("b", null)],
    selectedId: "a",
    shownTabs: [0],
    ...over,
  };
}

test("attention_seen: the selected agent tab in front clears its request", () => {
  assert.equal(attentionSeen(view(), true), "a");
});

test("attention_seen: nothing clears while the page is not in front", () => {
  assert.equal(attentionSeen(view(), false), null);
});

test("attention_seen: another tab alone does not count", () => {
  assert.equal(attentionSeen(view({ shownTabs: [1] }), true), null);
  assert.equal(attentionSeen(view({ shownTabs: [1, 0] }), true), "a");
});

test("attention_seen: the tasks view and other sessions do not count", () => {
  assert.equal(attentionSeen(view({ view: "tasks" }), true), null);
  assert.equal(attentionSeen(view({ selectedId: "b" }), true), null);
  assert.equal(attentionSeen(view({ selectedId: null }), true), null);
});

test("attention_seen: one clear per request, a new request clears again", async () => {
  const sent: string[] = [];
  const c = new AttentionClearer(async (id) => {
    sent.push(id);
  });
  c.note(view(), true);
  c.note(view(), true);
  assert.deepEqual(sent, ["a"]);
  c.note(view({ sessions: [session("a", "t2")] }), true);
  assert.deepEqual(sent, ["a", "a"]);
});

test("attention_seen: a failed clear is retried on the next note", async () => {
  let calls = 0;
  const c = new AttentionClearer(() => {
    calls++;
    return Promise.reject(new Error("down"));
  });
  c.note(view(), true);
  await Promise.resolve();
  await Promise.resolve();
  c.note(view(), true);
  assert.equal(calls, 2);
});
