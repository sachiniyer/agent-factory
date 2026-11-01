import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { type FakeElement, FakeEvent, installFakeDom } from "./backlog_fakedom.js";
import { PREVIEW_DELAY_MS, PREVIEW_TTL_MS, PreviewCache, PreviewPopover } from "./backlog_preview.js";

let dom: ReturnType<typeof installFakeDom>;
let now: number;

beforeEach(() => {
  dom = installFakeDom();
  now = 1000;
  mock.timers.enable({ apis: ["setTimeout"] });
});

afterEach(() => {
  mock.timers.reset();
  dom.restore();
});

const settle = async (): Promise<void> => {
  for (let i = 0; i < 5; i++) await new Promise((r) => setImmediate(r));
};

interface Pending {
  id: string;
  resolve(text: string): void;
  reject(e: Error): void;
}

function loader(): { pending: Pending[]; load(id: string): Promise<string> } {
  const pending: Pending[] = [];
  return {
    pending,
    load: (id) => new Promise((resolve, reject) => { pending.push({ id, resolve, reject }); }),
  };
}

test("a preview is cached for 5 s", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  const first = cache.get("s1");
  l.pending[0].resolve("hello");
  assert.deepEqual(await first, { text: "hello" });
  now += PREVIEW_TTL_MS - 1;
  assert.deepEqual(await cache.get("s1"), { text: "hello" });
  assert.equal(l.pending.length, 1);
  now += 1;
  assert.equal(cache.cached("s1"), undefined);
  void cache.get("s1");
  assert.equal(l.pending.length, 2);
});

test("one request at a time: a newer ask replaces a waiting one", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  const a = cache.get("a");
  const b = cache.get("b");
  const c = cache.get("c");
  assert.deepEqual(l.pending.map((p) => p.id), ["a"]);
  assert.equal(await b, null);
  l.pending[0].resolve("A");
  assert.deepEqual(await a, { text: "A" });
  await settle();
  assert.deepEqual(l.pending.map((p) => p.id), ["a", "c"]);
  l.pending[1].resolve("C");
  assert.deepEqual(await c, { text: "C" });
});

test("two asks for the same waiting id share its result", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  void cache.get("a");
  const b1 = cache.get("b");
  const b2 = cache.get("b");
  l.pending[0].resolve("A");
  await settle();
  l.pending[1].resolve("B");
  assert.deepEqual(await b1, { text: "B" });
  assert.deepEqual(await b2, { text: "B" });
});

test("a failed preview is kept as an error, not as empty text", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  const p = cache.get("s1");
  l.pending[0].reject(new Error("session is archived"));
  assert.deepEqual(await p, { text: "", error: "session is archived" });
});

test("the popover asks only after 300 ms of hover, and not if the pointer left", async () => {
  const l = loader();
  const pop = new PreviewPopover(new PreviewCache(l.load, () => now));
  const doc = dom.doc;
  const anchor = doc.createElement("button");
  doc.body.append(anchor, pop.el as unknown as FakeElement);
  pop.attach(anchor as unknown as HTMLElement, "s1", "B-7 run · 5m ago");

  anchor.dispatchEvent(new FakeEvent("mouseenter"));
  mock.timers.tick(PREVIEW_DELAY_MS - 1);
  anchor.dispatchEvent(new FakeEvent("mouseleave"));
  mock.timers.tick(PREVIEW_DELAY_MS);
  assert.equal(l.pending.length, 0);

  anchor.dispatchEvent(new FakeEvent("mouseenter"));
  mock.timers.tick(PREVIEW_DELAY_MS);
  assert.equal(l.pending.length, 1);
  l.pending[0].resolve("line 1\nline 2");
  await settle();
  const el = pop.el as unknown as FakeElement;
  assert.equal(el.hidden, false);
  assert.equal(el.textContent, "B-7 run · 5m agoline 1\nline 2");
  assert.equal(anchor.getAttribute("aria-describedby"), "af-backlog-preview");

  anchor.dispatchEvent(new FakeEvent("mouseleave"));
  assert.equal(el.hidden, true);
  assert.equal(anchor.getAttribute("aria-describedby"), null);
});

test("after a rebuild the open popover follows its session's new chip", async () => {
  const l = loader();
  const pop = new PreviewPopover(new PreviewCache(l.load, () => now));
  const doc = dom.doc;
  const list = doc.createElement("ul");
  const old = doc.createElement("button");
  list.append(old);
  doc.body.append(list, pop.el as unknown as FakeElement);
  pop.attach(old as unknown as HTMLElement, "s1", "run");
  old.dispatchEvent(new FakeEvent("mouseenter"));
  mock.timers.tick(PREVIEW_DELAY_MS);
  l.pending[0].resolve("x");
  await settle();

  const next = doc.createElement("button");
  pop.attach(next as unknown as HTMLElement, "s1", "run");
  list.replaceChildren(next);
  pop.rebind(list as unknown as HTMLElement);
  const el = pop.el as unknown as FakeElement;
  assert.equal(el.hidden, false);
  next.dispatchEvent(new FakeEvent("mouseleave"));
  assert.equal(el.hidden, true);

  list.replaceChildren();
  next.dispatchEvent(new FakeEvent("mouseenter"));
  pop.rebind(list as unknown as HTMLElement);
  mock.timers.tick(PREVIEW_DELAY_MS);
  assert.equal(l.pending.length, 1);
});

test("a kept ask is never superseded, and does not starve a hover ask", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  void cache.get("x");
  const panel = cache.get("p", true);
  const hover1 = cache.get("h1");
  const hover2 = cache.get("h2");
  assert.equal(await hover1, null);
  l.pending[0].resolve("X");
  await settle();
  assert.deepEqual(l.pending.map((p) => p.id), ["x", "p"]);
  l.pending[1].resolve("P");
  assert.deepEqual(await panel, { text: "P" });
  await settle();
  assert.deepEqual(l.pending.map((p) => p.id), ["x", "p", "h2"]);
  l.pending[2].resolve("H");
  assert.deepEqual(await hover2, { text: "H" });
});

test("a waiting ask that is cached by its turn resolves and the queue goes on", async () => {
  const l = loader();
  const cache = new PreviewCache(l.load, () => now);
  void cache.get("a");
  const a2 = cache.get("a", true);
  const b = cache.get("b", true);
  l.pending[0].resolve("A");
  assert.deepEqual(await a2, { text: "A" });
  await settle();
  assert.deepEqual(l.pending.map((p) => p.id), ["a", "b"]);
  l.pending[1].resolve("B");
  assert.deepEqual(await b, { text: "B" });
});
