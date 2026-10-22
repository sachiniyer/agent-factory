import { test } from "node:test";
import assert from "node:assert/strict";

import { createLatestWriter } from "./project_color_writes.js";

function deferred(): { promise: Promise<void>; resolve: () => void; reject: (e: unknown) => void } {
  let resolve!: () => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<void>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

test("a burst of choices writes the first, then only the latest, in order", async () => {
  const calls: string[] = [];
  const gates: Array<ReturnType<typeof deferred>> = [];
  const set = createLatestWriter((key, value) => {
    calls.push(`${key}=${value}`);
    const gate = deferred();
    gates.push(gate);
    return gate.promise;
  }, (e) => assert.fail(String(e)));

  const done = set("/a", "#111111");
  set("/a", "#222222");
  set("/a", "#333333");
  assert.deepEqual(calls, ["/a=#111111"]);

  gates[0].resolve();
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(calls, ["/a=#111111", "/a=#333333"]);

  gates[1].resolve();
  await done;
  assert.deepEqual(calls, ["/a=#111111", "/a=#333333"]);
});

test("keys write independently and a failure does not stop the next write", async () => {
  const calls: string[] = [];
  const errors: unknown[] = [];
  const set = createLatestWriter(async (key, value) => {
    calls.push(`${key}=${value}`);
    if (value === "bad") throw new Error("boom");
  }, (e) => errors.push(e));

  const a = set("/a", "bad");
  set("/a", "#444444");
  const b = set("/b", "#555555");
  await Promise.all([a, b]);
  assert.deepEqual(calls, ["/a=bad", "/b=#555555", "/a=#444444"]);
  assert.equal(errors.length, 1);
});

test("a queued choice keeps the connection it was made under, so a reconnect can fence it", async () => {
  let generation = 1;
  const sent: string[] = [];
  const gate = deferred();
  let first = true;
  const set = createLatestWriter<{ color: string; connection: number }>(async (key, { color, connection }) => {
    if (first) {
      first = false;
      await gate.promise;
    }
    if (connection !== generation) return;
    sent.push(`${key}=${color}`);
  }, (e) => assert.fail(String(e)));

  const done = set("/a", { color: "#111111", connection: 1 });
  set("/a", { color: "#222222", connection: 1 });
  generation = 2;
  gate.resolve();
  await done;
  assert.deepEqual(sent, []);

  await set("/a", { color: "#333333", connection: 2 });
  assert.deepEqual(sent, ["/a=#333333"]);
});
