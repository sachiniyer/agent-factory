import assert from "node:assert/strict";
import { test } from "node:test";
import { DRAFT_STALE_MS, DraftStorage, type KV } from "./docs_draft_store.js";
import type { Draft } from "./docs_sync_model.js";

function memory(): KV & { map: Map<string, string> } {
  const map = new Map<string, string>();
  return { map, get: (k) => map.get(k) ?? null, set: (k, v) => void map.set(k, v), remove: (k) => void map.delete(k), keys: () => [...map.keys()] };
}

const draft = (buffer: string): Draft => ({ buffer, base: "v1", theirs: "", head: "", kind: "unsent" });

test("two windows keep independent drafts of the same doc; neither clears the other's", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  a.store("goals", draft("from A"));
  b.store("goals", draft("from B"));
  a.clear("goals");
  assert.equal(b.load("goals")?.buffer, "from B", "A clearing its draft leaves B's");
  assert.equal(a.load("goals"), null, "and A does not read B's while B is alive");
});

test("a closed window's draft is adopted by the window that opens the doc next", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("unsaved work"));
  a.release(); // pagehide
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  assert.equal(b.load("goals")?.buffer, "unsaved work");
  assert.equal(kv.keys().filter((k) => k.endsWith("\nwA.0")).length, 0, "the orphan moved to B");
  assert.equal(b.load("goals")?.buffer, "unsaved work", "B now owns it");
});

test("a crashed window's draft is adopted once its heartbeat is stale, not before", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  a.store("goals", draft("work"));
  now += DRAFT_STALE_MS - 1;
  assert.equal(b.load("goals"), null, "A is still alive");
  a.touch();
  now += DRAFT_STALE_MS - 1;
  assert.equal(b.load("goals"), null, "a heartbeat keeps it alive");
  now += 2;
  assert.equal(b.load("goals")?.buffer, "work", "it went stale");
});

test("a draft stored before entries had owners is adopted", () => {
  const kv = memory();
  kv.set("af.docs.draft\ns\nplan\ngoals", JSON.stringify(draft("old format")));
  const a = new DraftStorage(kv, "wA", () => 0, "s\nplan");
  assert.equal(a.load("goals")?.buffer, "old format");
});

test("drafts of other docs and proposals are not touched", () => {
  const kv = memory();
  const a = new DraftStorage(kv, "wA", () => 0, "s\nplan");
  const other = new DraftStorage(kv, "wB", () => 0, "s\nother");
  other.store("goals", draft("other proposal"));
  other.release();
  a.store("api", draft("api work"));
  assert.equal(a.load("goals"), null, "another proposal's orphan is not adopted");
  assert.equal(a.load("api")?.buffer, "api work");
});

test("two panes of one window keep independent drafts; a save in one leaves the other's", () => {
  const kv = memory();
  const now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  const b = new DraftStorage(kv, "wA", () => now, "s\nplan", "2");
  a.store("goals", draft("from pane 1"));
  b.store("goals", draft("unresolved conflict in pane 2"));
  a.clear("goals"); // pane 1's save landed
  assert.equal(b.load("goals")?.buffer, "unresolved conflict in pane 2", "pane 2's draft is still stored");
  assert.equal(a.load("goals"), null, "and pane 1 does not adopt it while pane 2 is alive");
  b.release(); // pane 2 closes
  assert.equal(new DraftStorage(kv, "wA", () => now, "s\nplan", "3").load("goals")?.buffer, "unresolved conflict in pane 2", "a later pane adopts it");
});

test("a pane's heartbeat does not revive a closed sibling's released entries", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  const b = new DraftStorage(kv, "wA", () => now, "s\nplan", "2");
  b.store("goals", draft("kept"));
  b.release();
  now += 5000;
  a.touch();
  assert.equal(new DraftStorage(kv, "wA", () => now, "s\nplan", "3").load("goals")?.buffer, "kept");
});

test("a full storage keeps the orphan: it is removed only once the copy is stored", () => {
  const kv = memory();
  const now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("only copy"));
  a.release();
  let full = true;
  const quota: KV = { ...kv, set: (k, v) => { if (full) throw new Error("QuotaExceededError"); kv.set(k, v); } };
  const b = new DraftStorage(quota, "wB", () => now, "s\nplan");
  assert.equal(b.load("goals")?.buffer, "only copy", "the draft is still restored");
  assert.equal(kv.keys().length, 1, "and the original is still stored");
  full = false;
  b.store("goals", draft("only copy, edited")); // room again: the copy lands
  assert.deepEqual(kv.keys().map((k) => k.split("\n").pop()), ["wB.0"], "the original goes once B's copy holds the work");
});

test("a full storage keeps a legacy draft too", () => {
  const kv = memory();
  const now = 1000;
  kv.set("af.docs.draft\ns\nplan\ngoals", JSON.stringify(draft("legacy")));
  const quota: KV = { ...kv, set: () => { throw new Error("QuotaExceededError"); } };
  assert.equal(new DraftStorage(quota, "wB", () => now, "s\nplan").load("goals")?.buffer, "legacy");
  assert.equal(kv.keys().length, 1);
});
