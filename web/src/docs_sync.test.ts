import assert from "node:assert/strict";
import { test } from "node:test";
import { AUTOSAVE_DELAY_MS, DocSync, SAVE_RETRY_MS, type SyncStatus } from "./docs_sync.js";
import type { DocText, SaveResult } from "./proposal_api.js";

interface Deferred<T> {
  promise: Promise<T>;
  resolve(v: T): void;
  reject(e: unknown): void;
}

function deferred<T>(): Deferred<T> {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

const tick = () => new Promise<void>((r) => setImmediate(r));

class Harness {
  head: DocText = { text: "v1", commit: "c1" };
  buffer = "";
  bufferSets: string[] = [];
  statuses: SyncStatus[] = [];
  saves: { text: string; base: string; final: boolean; reply: Deferred<SaveResult> }[] = [];
  loads = 0;
  timers: { fn: () => void; ms: number; live: boolean }[] = [];
  holdLoad: Deferred<DocText> | null = null;
  sync = new DocSync({
    load: () => {
      this.loads++;
      return this.holdLoad ? this.holdLoad.promise : Promise.resolve({ ...this.head });
    },
    save: (text, base, final) => {
      const reply = deferred<SaveResult>();
      this.saves.push({ text, base, final: final === true, reply });
      return reply.promise;
    },
    setBuffer: (text) => {
      this.buffer = text;
      this.bufferSets.push(text);
    },
    onStatus: (s) => this.statuses.push(s),
    setTimer: (fn, ms) => {
      const t = { fn, ms, live: true };
      this.timers.push(t);
      return t;
    },
    clearTimer: (h) => {
      (h as { live: boolean }).live = false;
    },
  });

  get status(): SyncStatus {
    return this.statuses[this.statuses.length - 1]!;
  }

  liveTimers(): { fn: () => void; ms: number; live: boolean }[] {
    return this.timers.filter((t) => t.live);
  }

  async fire(): Promise<void> {
    const live = this.liveTimers();
    assert.equal(live.length, 1, "exactly one timer should be pending");
    live[0]!.live = false;
    live[0]!.fn();
    await tick();
  }

  type(text: string): void {
    this.buffer = text;
    this.sync.edit(text);
  }
}

async function started(): Promise<Harness> {
  const h = new Harness();
  await h.sync.start();
  return h;
}

test("start loads the doc into the buffer", async () => {
  const h = await started();
  assert.equal(h.buffer, "v1");
  assert.equal(h.sync.baseCommit, "c1");
  assert.equal(h.status, "saved");
});

test("autosave waits 500 ms after the last keystroke and sends the base commit", async () => {
  const h = await started();
  h.type("v1 a");
  h.type("v1 ab");
  assert.equal(h.liveTimers().length, 1, "each keystroke restarts the one timer");
  assert.equal(h.liveTimers()[0]!.ms, AUTOSAVE_DELAY_MS);
  assert.equal(h.status, "pending");
  await h.fire();
  assert.deepEqual(h.saves.map((s) => [s.text, s.base]), [["v1 ab", "c1"]]);
  assert.equal(h.status, "saving");
  h.saves[0]!.reply.resolve({ kind: "saved", text: "v1 ab", commit: "c2", merged: false });
  await tick();
  assert.equal(h.status, "saved");
  assert.equal(h.sync.baseCommit, "c2");
  assert.equal(h.sync.dirty, false);
});

test("a merged save replaces the buffer and says so", async () => {
  const h = await started();
  h.type("mine\nv1");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "saved", text: "mine\nv1\nagent", commit: "c3", merged: true });
  await tick();
  assert.equal(h.buffer, "mine\nv1\nagent");
  assert.equal(h.status, "merged");
  assert.equal(h.sync.baseCommit, "c3");
  assert.equal(h.sync.dirty, false);
});

test("typing during a merged save keeps the edits and the old base for the next merge", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.type("mine more");
  h.saves[0]!.reply.resolve({ kind: "saved", text: "mine\nagent", commit: "c3", merged: true });
  await tick();
  assert.equal(h.buffer, "mine more", "the user's newer keystrokes are not replaced");
  assert.equal(h.sync.baseCommit, "c1", "the buffer lacks the agent's edits, so the base stays");
  assert.equal(h.status, "pending");
  await h.fire();
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base], ["mine more", "c1"]);
});

test("typing during a plain save moves the base to the new commit", async () => {
  const h = await started();
  h.type("a");
  await h.fire();
  h.type("ab");
  h.saves[0]!.reply.resolve({ kind: "saved", text: "a", commit: "c2", merged: false });
  await tick();
  assert.equal(h.sync.baseCommit, "c2");
  await h.fire();
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base], ["ab", "c2"]);
});

test("a conflict stops autosave until the user picks", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "mine", theirs: "theirs", base: "v1", head: "c5" });
  await tick();
  assert.equal(h.status, "conflict");
  h.type("mine 2");
  assert.equal(h.liveTimers().length, 0);
  assert.equal(h.status, "conflict");
});

test("Keep mine saves the buffer against the agent's head", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "mine", theirs: "theirs", base: "v1", head: "c5" });
  await tick();
  const loads = h.loads;
  await h.sync.keepMine();
  await tick();
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base], ["mine", "c5"], "the 409's head is the new base");
  assert.equal(h.loads, loads, "the 409 already holds the head, so nothing is read again");
});

test("without a head in the 409, a resolution reads the doc again", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "mine", theirs: "theirs", base: "v1", head: "" });
  await tick();
  h.head = { text: "theirs now", commit: "c6" };
  await h.sync.takeTheirs();
  assert.equal(h.buffer, "theirs now");
  assert.equal(h.sync.baseCommit, "c6");
});

test("Take theirs replaces the buffer with the agent's head", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "mine", theirs: "theirs", base: "v1", head: "c5" });
  await tick();
  await h.sync.takeTheirs();
  assert.equal(h.buffer, "theirs");
  assert.equal(h.sync.baseCommit, "c5");
  assert.equal(h.status, "saved");
  assert.equal(h.sync.dirty, false);
});

test("Show both writes conflict markers and holds the save until they are gone", async () => {
  const h2 = new Harness();
  h2.head = { text: "a\nb\nc", commit: "c1" };
  await h2.sync.start();
  h2.type("a\nmine\nc");
  await h2.fire();
  h2.saves[0]!.reply.resolve({ kind: "conflict", ours: "a\nmine\nc", theirs: "a\ntheirs\nc", base: "a\nb\nc", head: "c7" });
  await tick();
  await h2.sync.showBoth();
  assert.equal(h2.buffer, "a\n<<<<<<< mine\nmine\n=======\ntheirs\n>>>>>>> agent\nc");
  assert.equal(h2.status, "markers");
  assert.equal(h2.sync.baseCommit, "c7");
  h2.type("a\nmine and theirs\nc");
  assert.equal(h2.status, "pending");
  await h2.fire();
  assert.deepEqual([h2.saves[1]!.text, h2.saves[1]!.base], ["a\nmine and theirs\nc", "c7"]);
});

test("a failed save retries after a pause", async () => {
  const h = await started();
  h.type("x");
  await h.fire();
  h.saves[0]!.reply.reject(new Error("cannot reach the daemon"));
  await tick();
  assert.equal(h.status, "error");
  assert.equal(h.liveTimers()[0]!.ms, SAVE_RETRY_MS);
  await h.fire();
  assert.equal(h.saves.length, 2);
});

test("an agent change reloads a clean buffer", async () => {
  const h = await started();
  h.head = { text: "v1\nagent line", commit: "c9" };
  h.sync.remoteChanged("c9");
  await tick();
  assert.equal(h.buffer, "v1\nagent line");
  assert.equal(h.sync.baseCommit, "c9");
  assert.equal(h.sync.dirty, false);
});

test("an event for the commit already loaded does nothing", async () => {
  const h = await started();
  const loads = h.loads;
  h.sync.remoteChanged("c1");
  await tick();
  assert.equal(h.loads, loads);
});

test("an agent change waits while the buffer has edits; the next save merges", async () => {
  const h = await started();
  h.type("mine");
  h.head = { text: "agent", commit: "c9" };
  const loads = h.loads;
  h.sync.remoteChanged("c9");
  await tick();
  assert.equal(h.loads, loads, "a dirty buffer is not reloaded");
  assert.equal(h.buffer, "mine");
  await h.fire();
  assert.equal(h.saves[0]!.base, "c1", "the save carries the old base so the daemon merges");
});

test("an agent change during a save reloads once the save is clean", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.sync.remoteChanged("c9");
  h.head = { text: "mine\nagent", commit: "c9" };
  h.saves[0]!.reply.resolve({ kind: "saved", text: "mine", commit: "c8", merged: false });
  await tick();
  await tick();
  assert.equal(h.buffer, "mine\nagent");
  assert.equal(h.sync.baseCommit, "c9");
});

test("typing while a reload is in flight keeps the edits", async () => {
  const h = await started();
  h.holdLoad = deferred<DocText>();
  h.sync.remoteChanged("c9");
  h.type("typed");
  h.holdLoad.resolve({ text: "agent", commit: "c9" });
  await tick();
  assert.equal(h.buffer, "typed");
  assert.equal(h.sync.baseCommit, "c1");
});

test("dispose saves unsaved edits", async () => {
  const h = await started();
  h.type("unsaved");
  h.sync.dispose();
  await tick();
  assert.deepEqual(h.saves.map((s) => s.text), ["unsaved"]);
});

test("dispose saves a buffer that still has conflict markers", async () => {
  const h = new Harness();
  h.head = { text: "a\nb\nc", commit: "c1" };
  await h.sync.start();
  h.type("a\nmine\nc");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "a\nmine\nc", theirs: "a\ntheirs\nc", base: "a\nb\nc", head: "c7" });
  await tick();
  await h.sync.showBoth();
  assert.equal(h.status, "markers");
  assert.equal(h.liveTimers().length, 0, "autosave holds while markers are present");
  h.sync.dispose();
  assert.equal(h.saves.length, 2);
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base, h.saves[1]!.final],
    ["a\n<<<<<<< mine\nmine\n=======\ntheirs\n>>>>>>> agent\nc", "c7", true]);
});

test("leaving the page saves edits but keeps the doc open", async () => {
  const h = await started();
  h.type("unsaved");
  h.sync.saveBeforeLeaving();
  assert.deepEqual(h.saves.map((s) => [s.text, s.final]), [["unsaved", true]]);
  h.type("unsaved more");
  assert.equal(h.status, "pending", "editing continues after a pagehide that did not unload");
});

test("dispose does not send an open conflict the user has not resolved", async () => {
  const h = await started();
  h.type("mine");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "conflict", ours: "mine", theirs: "theirs", base: "v1", head: "c5" });
  await tick();
  h.sync.dispose();
  assert.equal(h.saves.length, 1);
});

test("dispose does not repeat a save that is already in flight", async () => {
  const h = await started();
  h.type("x");
  await h.fire();
  h.sync.dispose();
  assert.equal(h.saves.length, 1);
});
