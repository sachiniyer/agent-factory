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
  assert.equal(kv.keys().filter((k) => k.endsWith("wA")).length, 0, "the orphan moved to B");
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
