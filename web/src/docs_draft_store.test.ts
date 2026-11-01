import assert from "node:assert/strict";
import { test } from "node:test";
import { DRAFT_STALE_MS, DraftStorage, newWindowId, type KV } from "./docs_draft_store.js";
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

test("a closed pane's late write is released at once, and the heartbeat does not revive it", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  a.store("goals", draft("first"));
  a.close(); // the pane is gone; a save of its is still in flight
  a.store("goals", draft("conflict draft written by the closing sync")); // the reply came back
  a.touch();
  now += 1000; // far inside the 30 s a live owner would be waited for
  assert.equal(new DraftStorage(kv, "wA", () => now, "s\nplan", "2").load("goals")?.buffer, "conflict draft written by the closing sync");
});

test("docs() lists every doc with a draft this pane may take over, deleted or not; peek adopts nothing", () => {
  const kv = memory();
  const now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  a.store("goals", draft("goals work"));
  a.store("old", draft("work on a doc the daemon no longer has"));
  const live = new DraftStorage(kv, "wB", () => now, "s\nplan", "1");
  live.store("theirs", draft("another live pane's"));
  a.release();
  const next = new DraftStorage(kv, "wA", () => now, "s\nplan", "2");
  assert.deepEqual(next.docs().sort(), ["goals", "old"], "a live pane's draft is not offered");
  assert.equal(next.peek("old")?.buffer, "work on a doc the daemon no longer has");
  assert.equal(kv.keys().length, 3, "peek and docs adopted nothing");
});

test("a closed pane's check of its own entry adopts nothing", () => {
  const kv = memory();
  const now = 1000;
  const orphan = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  orphan.store("goals", draft("someone else's released draft"));
  orphan.release();
  const closed = new DraftStorage(kv, "wA", () => now, "s\nplan", "2");
  closed.close();
  assert.equal(closed.load("goals"), null);
  assert.equal(kv.keys().length, 1, "the orphan stays where it is for the pane that opens next");
});

test("pagehide releases first and the final saves that follow stay released, until the page is shown again", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan", "1");
  a.store("goals", draft("typed"));
  a.release(); // pagehide, before the panes' own listeners start their final saves
  a.store("goals", { ...draft("typed"), maybe: ["typed, sent"] }); // the final save's draft
  now += 1000;
  assert.equal(new DraftStorage(kv, "wA", () => now, "s\nplan", "2").peek("goals")?.maybe?.[0], "typed, sent", "adoptable at once, not after 30 s");
  a.touch(); // bfcache restore
  a.store("goals", draft("typed more"));
  assert.equal(new DraftStorage(kv, "wA", () => now, "s\nplan", "3").peek("goals"), null, "alive again");
});

test("a duplicated browser tab gets its own window id even with a copy of the first tab's sessionStorage", () => {
  // The id never comes from storage, so there is nothing for a copy to carry over.
  const first = newWindowId(() => 0.123456789, () => 1000);
  const dup = newWindowId(() => 0.987654321, () => 1000);
  assert.notEqual(first, dup);
  const a = new DraftStorage(memory(), first, () => 0, "s\nplan", "1");
  const b = new DraftStorage(memory(), dup, () => 0, "s\nplan", "1");
  assert.notEqual(a.owner, b.owner, "the same pane number in both pages is still two owners");
});

test("a bfcache restore writes back a draft another window adopted and removed", () => {
  const kv = memory();
  const now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("unresolved conflict"));
  a.release(); // pagehide into the back-forward cache
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  assert.equal(b.load("goals")?.buffer, "unresolved conflict", "B adopts it and the original entry is removed");
  b.clear("goals"); // B resolves or discards its copy
  assert.equal(a.load("goals"), null, "A's entry is gone: touch() alone could not bring it back");
  a.revive(); // pageshow
  assert.equal(a.load("goals")?.buffer, "unresolved conflict", "A still holds the work it never resolved");
  a.clear("goals");
  a.revive();
  assert.equal(a.load("goals"), null, "a draft the sync cleared is not written back");
});

test("a bfcache restore writes back a draft the pane adopted, not only one it stored", () => {
  const kv = memory();
  const now = 1000;
  const first = new DraftStorage(kv, "wA", () => now, "s\nplan");
  first.store("goals", draft("conflict from before"));
  first.release();
  const a = new DraftStorage(kv, "wB", () => now, "s\nplan");
  assert.equal(a.load("goals")?.buffer, "conflict from before", "the pane adopts it and never stores it again");
  a.release(); // pagehide into the back-forward cache
  const other = new DraftStorage(kv, "wC", () => now, "s\nplan");
  assert.ok(other.load("goals"), "another window adopts it");
  other.clear("goals");
  assert.equal(a.load("goals"), null);
  a.revive();
  assert.equal(a.load("goals")?.buffer, "conflict from before");
});

test("a bfcache restore writes back a legacy-adopted draft", () => {
  const kv = memory();
  kv.set("af.docs.draft\ns\nplan\ngoals", JSON.stringify(draft("legacy work")));
  const a = new DraftStorage(kv, "wA", () => 1000, "s\nplan");
  assert.equal(a.load("goals")?.buffer, "legacy work");
  a.release();
  for (const k of kv.keys()) kv.remove(k);
  a.revive();
  assert.equal(a.load("goals")?.buffer, "legacy work");
});

test("a heartbeat that resumes after a stall writes back a draft another window adopted as dead", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("unresolved conflict"));
  now += DRAFT_STALE_MS + 1; // the page was throttled: no heartbeat for over 30 s
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  assert.equal(b.load("goals")?.buffer, "unresolved conflict", "B adopts what looks like a dead window's draft");
  b.clear("goals"); // B resolves or discards its copy
  assert.equal(a.load("goals"), null);
  a.touch(); // the heartbeat resumes: no pageshow, so revive() is never reached
  assert.equal(a.load("goals")?.buffer, "unresolved conflict", "A's unresolved work is stored again");
  a.clear("goals");
  a.touch();
  assert.equal(a.load("goals"), null, "a draft the sync cleared is not written back");
});

const ownersLeft = (kv: ReturnType<typeof memory>, owners: string[]): string[] =>
  kv.keys().filter((k) => owners.some((o) => k.endsWith(`\n${o}`)));

test("a page that opens a doc takes over every draft the closed pages left, and leaves none under their ids", () => {
  const kv = memory();
  let now = 1000;
  const closed = new DraftStorage(kv, "wClosed", () => now, "s\nplan");
  closed.store("goals", draft("the closed page's unsent work"));
  closed.release(); // pagehide
  const crashed = new DraftStorage(kv, "wCrashed", () => now, "s\nplan");
  now += 5;
  crashed.store("goals", draft("a second window's conflict"));
  now += DRAFT_STALE_MS + 1; // its heartbeat stopped: it looks dead
  const next = new DraftStorage(kv, "wNext", () => now, "s\nplan");
  assert.equal(next.load("goals")?.buffer, "a second window's conflict", "the newest orphan is the draft");
  assert.deepEqual(ownersLeft(kv, ["wClosed.0", "wCrashed.0"]), [], "nothing is left under the ids of the closed pages");
  // The other draft was not dropped: it comes forward when this one is dealt with.
  next.clear("goals");
  assert.equal(next.load("goals")?.buffer, "the closed page's unsent work");
  next.clear("goals");
  assert.equal(next.load("goals"), null, "and then nothing is left");
});

test("a draft the new page already holds does not keep a closed page's draft behind", () => {
  const kv = memory();
  let now = 1000;
  const closed = new DraftStorage(kv, "wClosed", () => now, "s\nplan");
  closed.store("goals", draft("old unsent work"));
  closed.release();
  now += 5;
  const next = new DraftStorage(kv, "wNext", () => now, "s\nplan");
  next.store("goals", draft("work typed in the new page")); // it has its own entry before it ever loads
  assert.equal(next.load("goals")?.buffer, "work typed in the new page", "its own draft stays the draft");
  assert.deepEqual(ownersLeft(kv, ["wClosed.0"]), [], "the closed page's entry is taken over, not left behind");
  next.clear("goals");
  assert.equal(next.load("goals")?.buffer, "old unsent work", "and its draft is still recoverable");
});

test("identical drafts are not kept twice, and storing keeps the drafts taken over", () => {
  const kv = memory();
  const now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("same"));
  a.release();
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  b.store("goals", draft("same"));
  b.load("goals");
  b.clear("goals");
  assert.equal(b.load("goals"), null, "an identical draft is not kept as a second one");
  const c = new DraftStorage(kv, "wC", () => now, "s\nplan");
  c.store("goals", draft("one"));
  c.release();
  const d = new DraftStorage(kv, "wD", () => now, "s\nplan");
  d.store("goals", draft("two"));
  d.load("goals"); // takes wC's draft over with its own
  d.store("goals", draft("two, edited")); // the pane keeps typing
  d.clear("goals");
  assert.equal(d.load("goals")?.buffer, "one", "storing did not drop the draft it took over");
});

test("a draft taken over with the newest stays protected until a load restores it, however the pane writes meanwhile", () => {
  // The newest draft matches the server and is cleared; the pane then types: the older draft it came with
  // must not be overwritten by that write, because the editor has not been shown it.
  const kv = memory();
  let now = 1000;
  const older = new DraftStorage(kv, "wOlder", () => now, "s\nplan");
  older.store("goals", draft("older unsent work"));
  older.release();
  now += 5;
  const newest = new DraftStorage(kv, "wNewest", () => now, "s\nplan");
  newest.store("goals", draft("newest work"));
  newest.release();
  now += 5;
  const next = new DraftStorage(kv, "wNext", () => now, "s\nplan");
  assert.equal(next.load("goals")?.buffer, "newest work");
  next.clear("goals"); // it matched the server
  next.store("goals", draft("typed after")); // the next edit
  next.clear("goals"); // saved
  assert.equal(next.load("goals")?.buffer, "older unsent work", "the older draft survived the writes in between");
});

test("a page that takes over many drafts keeps every one", () => {
  const kv = memory();
  let now = 1000;
  for (let i = 0; i < 12; i++) {
    const w = new DraftStorage(kv, `wOld${i}`, () => now, "s\nplan");
    w.store("goals", draft(`work ${i}`));
    w.release();
    now += 1;
  }
  const next = new DraftStorage(kv, "wNext", () => now, "s\nplan");
  const seen: string[] = [];
  for (let i = 0; i < 12; i++) {
    const d = next.load("goals");
    if (d) seen.push(d.buffer);
    next.clear("goals");
  }
  assert.deepEqual([...seen].sort(), Array.from({ length: 12 }, (_, i) => `work ${i}`).sort(), "all twelve come back, one at a time");
  assert.equal(next.load("goals"), null);
});

test("drafts bundled with an orphan survive a failed adoption", () => {
  const kv = memory();
  let now = 1000;
  const a = new DraftStorage(kv, "wA", () => now, "s\nplan");
  a.store("goals", draft("a"));
  a.release();
  now += 1;
  const b = new DraftStorage(kv, "wB", () => now, "s\nplan");
  b.store("goals", draft("b"));
  b.release();
  now += 1;
  // wC takes both over (b is the draft, a rides along) and then closes without restoring them.
  const c = new DraftStorage(kv, "wC", () => now, "s\nplan");
  assert.equal(c.load("goals")?.buffer, "b");
  c.release();
  now += 1;
  // wD finds the bundle while storage is full: the copy does not fit, and the bundle must stay whole.
  let full = true;
  const limited: typeof kv = { ...kv, set: (k, v) => { if (full && !kv.map.has(k)) throw new Error("QuotaExceededError"); kv.set(k, v); } };
  const d = new DraftStorage(limited, "wD", () => now, "s\nplan");
  assert.equal(d.load("goals")?.buffer, "b", "its own copy does not fit, the draft is still returned");
  full = false;
  d.store("goals", draft("b, edited")); // space is back and the pane writes
  d.clear("goals");
  assert.equal(d.load("goals")?.buffer, "a", "the draft that rode along with the orphan was not lost with its source");
});
