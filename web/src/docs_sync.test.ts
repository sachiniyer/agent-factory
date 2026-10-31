import assert from "node:assert/strict";
import { test } from "node:test";
import { AUTOSAVE_DELAY_MS, DocSync, SAVE_RETRY_MS, SAVE_TIMEOUT_MS, type SyncStatus } from "./docs_sync.js";
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

test("typing during a merged save is rebased onto the merge and the base moves to its commit", async () => {
  const h = await started();
  h.type("l1\nl2\nl3");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "saved", text: "l1\nl2\nl3", commit: "c2", merged: false });
  await tick();
  h.type("l1x\nl2\nl3");
  await h.fire();
  h.type("l1xx\nl2\nl3");
  // The daemon merged the agent's edit of line 3 into what was sent.
  h.saves[1]!.reply.resolve({ kind: "saved", text: "l1x\nl2\nl3y", commit: "c3", merged: true });
  await tick();
  assert.equal(h.buffer, "l1xx\nl2\nl3y", "the newer keystrokes survive and the agent's line arrives");
  assert.equal(h.sync.baseCommit, "c3", "the base is the merged commit, not the old one");
  assert.equal(h.status, "pending");
  await h.fire();
  assert.deepEqual([h.saves[2]!.text, h.saves[2]!.base], ["l1xx\nl2\nl3y", "c3"]);
});

test("typing on the line the agent changed during a merged save opens a conflict instead of a doomed save", async () => {
  const h = await started();
  h.type("a\nb\nc");
  await h.fire();
  h.type("a\nb\nc2");
  h.saves[0]!.reply.resolve({ kind: "saved", text: "a\nb\nC", commit: "c3", merged: true });
  await tick();
  assert.equal(h.status, "conflict", "the user's typing and the agent's edit hit the same line");
  assert.equal(h.sync.text, "a\nb\nc2", "the draft stays in the buffer");
  assert.equal(h.saves.length, 1, "no save that would conflict with our own commit");
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

test("a failed save reads the daemon's head before it is sent again", async () => {
  const h = await started();
  h.type("x");
  await h.fire();
  const loadsBefore = h.loads;
  h.saves[0]!.reply.reject(new Error("cannot reach the daemon"));
  await tick();
  assert.equal(h.loads, loadsBefore + 1, "the outcome is unknown, so the head is read first");
  assert.equal(h.saves.length, 1, "nothing is resent blind");
  await h.fire();
  assert.equal(h.saves.length, 2, "the head is where it was, so the save goes out again");
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base], ["x", "c1"]);
});

test("a save with no reply is treated as unknown after the timeout, and its late reply is ignored", async () => {
  const h = await started();
  h.type("x");
  await h.fire();
  assert.equal(h.status, "saving");
  const loadsBefore = h.loads;
  const timeout = h.liveTimers().find((t) => t.ms === SAVE_TIMEOUT_MS);
  assert.ok(timeout, "a reply timeout is armed with the save");
  timeout!.fn();
  await tick();
  assert.equal(h.loads, loadsBefore + 1, "the daemon's head is read");
  // The reply to the first request finally arrives; it must not be mistaken for anything.
  h.saves[0]!.reply.resolve({ kind: "saved", text: "x", commit: "c2", merged: false });
  await tick();
  assert.equal(h.sync.baseCommit, "c1");
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
  h.saves[0]!.reply.resolve({ kind: "saved", text: "unsaved", commit: "c2", merged: false });
  await tick();
  assert.equal(h.status, "pending", "editing continues after a pagehide that did not unload");
  await h.fire();
  assert.deepEqual([h.saves[1]!.text, h.saves[1]!.base], ["unsaved more", "c2"]);
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

test("whenSaved stays pending while a lost-reply save is unreconciled, even when the buffer is back to the acknowledged text", async () => {
  const h = await started();
  h.type("v2");
  await h.fire(); // the save of v2 against c1 goes out
  // It lands on the daemon, but its reply is lost.
  h.head = { text: "v2", commit: "c2" };
  h.holdLoad = deferred<DocText>(); // the read of the head is not answered yet
  h.saves[0]!.reply.reject(new Error("503 service unavailable"));
  await tick();
  // The user undoes back to the text they last saw acknowledged.
  h.type("v1");
  let verdict: string | null = null;
  void h.sync.whenSaved().then((v) => (verdict = v));
  await tick();
  assert.equal(verdict, null, "the daemon may still hold v2: nothing is saved yet");
  // The read answers: v2 did land, so the undo is a real edit and is saved on top of it.
  h.holdLoad.resolve({ ...h.head });
  h.holdLoad = null;
  await tick();
  assert.equal(verdict, null, "the corrective save of v1 is still to land");
  await h.fire(); // after a failure the retry waits for its timer
  assert.equal(h.saves.at(-1)!.text, "v1");
  assert.equal(h.saves.at(-1)!.base, "c2");
  h.saves.at(-1)!.reply.resolve({ kind: "saved", text: "v1", commit: "c3", merged: false });
  await tick();
  assert.equal(verdict, "saved");
});

test("a tab closed while its first read is out is not quiet until that read has answered", async () => {
  const h = new Harness();
  h.holdLoad = deferred<DocText>();
  const started = h.sync.start();
  await tick();
  h.sync.dispose(); // closed before the doc has loaded: it will restore its draft and send it
  let quiet = false;
  void h.sync.whenQuiet().then(() => (quiet = true));
  await tick();
  assert.equal(quiet, false, "a reopened tab must not read yet");
  h.holdLoad.resolve({ ...h.head });
  await started;
  await tick();
  assert.equal(quiet, true);
});

test("a 404 to a save sent before the doc was restored does not mark the restored doc deleted", async () => {
  const h = await started();
  h.type("v2");
  await h.fire(); // the save goes out
  h.sync.remoteDeleted(); // the doc list says the doc is gone: unsaved work keeps the tab
  assert.equal(h.sync.currentStatus, "gone");
  h.head = { text: "v1", commit: "c5" }; // the doc is added again
  await h.sync.revive();
  assert.notEqual(h.sync.currentStatus, "gone");
  h.saves[0]!.reply.reject(Object.assign(new Error("404 doc not found"), { status: 404 })); // the old save's answer, late
  await tick();
  assert.notEqual(h.sync.currentStatus, "gone", "the restore stands");
});

test("a save that timed out and lands later does not overwrite an undo", async () => {
  const h = await started();
  h.type("v2");
  await h.fire(); // the save of v2 against c1 goes out and takes its time
  h.type("v1"); // the user undoes back to the acknowledged text
  // The client gives up after 30 s and reads the head, which has not changed.
  const timeout = h.timers.find((t) => t.live && t.ms === 30_000)!;
  timeout.live = false;
  timeout.fn();
  await tick();
  let verdict: string | null = null;
  void h.sync.whenSaved().then((v) => (verdict = v));
  await tick();
  assert.equal(verdict, null, "the request is still out: the daemon may yet hold v2");
  // The request lands after all, and the daemon's change event reaches the tab.
  h.head = { text: "v2", commit: "c2" };
  h.sync.remoteChanged("c2");
  h.saves[0]!.reply.resolve({ kind: "saved", text: "v2", commit: "c2", merged: false }); // ignored as a reply, but it ends the request
  await tick();
  await tick();
  assert.equal(h.buffer, "v1", "the undo stands over the landed save");
  await h.fire();
  assert.equal(h.saves.at(-1)!.text, "v1", "and is saved on top of it");
});

test("a pumped whenSaved does not turn a failing save into a loop of save and read", async () => {
  const h = await started();
  h.type("v2");
  void h.sync.whenSaved();
  await tick();
  for (let i = 0; i < 20; i++) {
    const n = h.saves.length;
    h.saves.at(-1)!.reply.reject(new Error("503 service unavailable"));
    await tick();
    await tick();
    assert.equal(h.saves.length, n, `no save is sent until the retry timer fires (round ${i})`);
    await h.fire();
  }
});

test("every save names its writer and counts its saves upward", async () => {
  const fences: { writer: string; seq: number }[] = [];
  const h = new Harness();
  const save = h.sync["deps"].save;
  h.sync["deps"].save = (text, base, final, fence) => {
    if (fence) fences.push(fence);
    return save(text, base, final, fence);
  };
  await h.sync.start();
  h.type("v2");
  await h.fire();
  h.saves[0]!.reply.resolve({ kind: "saved", text: "v2", commit: "c2", merged: false });
  await tick();
  h.type("v3");
  await h.fire();
  assert.equal(fences.length, 2);
  assert.equal(fences[0]!.writer, fences[1]!.writer, "one sync, one writer");
  assert.ok(fences[1]!.seq > fences[0]!.seq, "and its saves count upward");
  assert.ok(fences[0]!.writer.length >= 8);
});
