import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { installFakeDom } from "./backlog_fakedom.js";
import { BodyEditor, SAVE_DELAY_MS, bodyEditorLoader } from "./backlog_body.js";
import { resetSerialState } from "./backlog_serial.js";

let dom: ReturnType<typeof installFakeDom>;
const realLoad = bodyEditorLoader.load;
let text: string;
let change: (t: string) => void;
let destroyed: boolean;

beforeEach(() => {
  resetSerialState();
  dom = installFakeDom();
  destroyed = false;
  bodyEditorLoader.load = async () => ({
    createDocsEditor: (_parent: HTMLElement, opts: { text: string; onChange(t: string): void }) => {
      text = opts.text;
      change = (t) => { text = t; opts.onChange(t); };
      return { getText: () => text, setText: (t: string) => { text = t; }, destroy: () => { destroyed = true; } } as never;
    },
  });
  mock.timers.enable({ apis: ["setTimeout"] });
});

afterEach(() => {
  mock.timers.reset();
  dom.restore();
  bodyEditorLoader.load = realLoad;
});

const settle = (): Promise<void> => new Promise((r) => setImmediate(r));

test("flush saves a pending edit once and is a no-op otherwise", async () => {
  const saves: string[] = [];
  const ed = new BodyEditor((t) => { saves.push(t); }, "Body");
  ed.setText("stored");
  await settle();
  assert.equal(text, "stored");
  ed.flush();
  assert.deepEqual(saves, []);
  change("typed");
  ed.setText("stored again");
  assert.equal(text, "typed");
  ed.flush();
  ed.flush();
  mock.timers.tick(SAVE_DELAY_MS);
  assert.deepEqual(saves, ["typed"]);
});

test("dispose saves a pending edit and destroys the editor", async () => {
  const saves: string[] = [];
  const ed = new BodyEditor((t) => { saves.push(t); }, "Body");
  await settle();
  change("last words");
  ed.dispose();
  assert.deepEqual(saves, ["last words"]);
  assert.equal(destroyed, true);
});

test("the stored body shows as plain text while the editor bundle is missing", async () => {
  bodyEditorLoader.load = () => Promise.reject(new Error("offline"));
  const ed = new BodyEditor(() => {}, "Body");
  ed.setText("plain");
  await settle();
  assert.equal((ed.el as unknown as { textContent: string }).textContent, "plain");
});

test("an edit stays until the save is acknowledged, and a failed save keeps it", async () => {
  let settleSave: (ok: boolean) => void = () => {};
  const ed = new BodyEditor(() => new Promise<boolean>((r) => { settleSave = r; }), "Body");
  await settle();
  ed.setText("stored");
  change("edit");
  mock.timers.tick(SAVE_DELAY_MS);      // debounce fired, save in flight, timer cleared
  ed.setText("stored");
  assert.equal(text, "edit");
  settleSave(false);                    // a failed save keeps the draft
  await settle();
  ed.setText("stored");
  assert.equal(text, "edit");
  change("edit 2");
  mock.timers.tick(SAVE_DELAY_MS);
  settleSave(true);                     // acknowledged
  await settle();
  ed.setText("edit 2 stored");
  assert.equal(text, "edit 2 stored");
});

test("typing during a save keeps the newer draft when the older save is acknowledged", async () => {
  const resolvers: ((ok: boolean) => void)[] = [];
  const ed = new BodyEditor(() => new Promise<boolean>((r) => { resolvers.push(r); }), "Body");
  await settle();
  change("one");
  mock.timers.tick(SAVE_DELAY_MS);
  change("one two");
  resolvers[0](true);                   // acknowledges "one" only
  await settle();
  ed.setText("one");
  assert.equal(text, "one two");
});

test("saves are serialized: an edit made during a save waits and goes out as the latest text", async () => {
  const sent: string[] = [];
  const resolvers: ((ok: boolean) => void)[] = [];
  const ed = new BodyEditor((t) => { sent.push(t); return new Promise<boolean>((r) => { resolvers.push(r); }); }, "Body");
  await settle();
  change("one");
  mock.timers.tick(SAVE_DELAY_MS);
  change("one two");
  mock.timers.tick(SAVE_DELAY_MS);          // the second pause while the first save is still in flight
  change("one two three");
  mock.timers.tick(SAVE_DELAY_MS);
  assert.deepEqual(sent, ["one"], "no second request while the first is in flight");
  resolvers[0](true);
  await settle();
  assert.deepEqual(sent, ["one", "one two three"], "one follow-up with the latest text, not one per pause");
  ed.setText("one");
  assert.equal(text, "one two three", "the older acknowledgement does not release the newer draft");
  resolvers[1](true);
  await settle();
  ed.setText("stored by agent");
  assert.equal(text, "stored by agent");
});

test("a failed save does not retry on its own", async () => {
  const sent: string[] = [];
  const ed = new BodyEditor((t) => { sent.push(t); return Promise.resolve(false); }, "Body");
  await settle();
  change("x");
  mock.timers.tick(SAVE_DELAY_MS);
  await settle();
  assert.deepEqual(sent, ["x"]);
  ed.setText("stored");
  assert.equal(text, "x");
});

test("a save held by a closed editor cannot land after the reopened editor's", async () => {
  const sent: string[] = [];
  const resolvers: ((ok: boolean) => void)[] = [];
  const save = (t: string) => { sent.push(t); return new Promise<boolean>((r) => { resolvers.push(r); }); };
  const first = new BodyEditor(save, "Body", "/a\u00001\u0000body");
  await settle();
  change("first");
  mock.timers.tick(SAVE_DELAY_MS);                  // "first" is in flight
  change("second");
  first.dispose();                                  // the panel closes with "second" queued behind it
  const reopened = new BodyEditor(save, "Body", "/a\u00001\u0000body");
  await settle();
  change("latest");
  mock.timers.tick(SAVE_DELAY_MS);                  // the reopened editor saves newer text
  assert.deepEqual(sent, ["first"], "still one request in flight for this body");
  resolvers[0](true);
  await settle();
  assert.deepEqual(sent, ["first", "latest"], "the stale queued draft was replaced, not sent after it");
  resolvers[1](true);
  await settle();
  assert.equal(sent.at(-1), "latest");
  reopened.dispose();
});
