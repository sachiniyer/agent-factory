import assert from "node:assert/strict";
import { test } from "node:test";
import { restoreSession } from "./api.js";
import { PendingRestores } from "./pending_restores.js";

test("two Restore clicks send one request without a failure modal; settlement releases the fence", async t => {
  let release!: () => void;
  const response = new Promise<void>(resolve => { release = resolve; });
  let requests = 0;
  let failureModals = 0;
  let progressModals = 0;
  let visiblePending: ReadonlySet<string> = new Set();
  t.mock.method(globalThis, "fetch", async (url: string) => {
    assert.match(url, /\/RestoreSession$/);
    requests++;
    await response;
    return new Response(JSON.stringify({ data: {} }));
  });
  const pending = new PendingRestores(ids => { visiblePending = ids; });
  const click = () => pending.run("session", () => {
    progressModals++;
    return restoreSession("session", "title", "").catch(() => { failureModals++; });
  });
  const first = click();
  assert.equal(click(), null);
  assert.equal(requests, 1);
  assert.equal(progressModals, 1);
  assert.equal(failureModals, 0);
  assert.equal(visiblePending.has("session"), true);
  release();
  await first;
  assert.equal(pending.has("session"), false);
  assert.equal(visiblePending.size, 0);
  await click();
  assert.equal(requests, 2);
  assert.equal(failureModals, 0);
});

test("restore rejection releases only its session fence", async () => {
  const pending = new PendingRestores(() => {});
  let release!: () => void;
  const other = pending.run("other", () => new Promise<void>(resolve => { release = resolve; }));
  await assert.rejects(pending.run("failed", async () => { throw new Error("refused"); })!, /refused/);
  assert.equal(pending.has("failed"), false);
  assert.equal(pending.has("other"), true);
  release();
  await other;
});

test("reset prevents an old response from clearing a newer restore", async () => {
  const pending = new PendingRestores(() => {});
  let releaseOld!: () => void;
  let releaseNew!: () => void;
  const old = pending.run("session", () => new Promise<void>(resolve => { releaseOld = resolve; }));
  pending.reset();
  const current = pending.run("session", () => new Promise<void>(resolve => { releaseNew = resolve; }));
  releaseOld();
  await old;
  assert.equal(pending.has("session"), true);
  releaseNew();
  await current;
  assert.equal(pending.has("session"), false);
});
