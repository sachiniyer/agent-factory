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

test("a later snapshot that carries a saved body whose confirming read failed releases the draft", async () => {
  const ed = new BodyEditor(() => Promise.resolve(false), "Body", "k");
  ed.setText("old");
  await settle();
  change("edited");
  mock.timers.tick(SAVE_DELAY_MS);
  await settle();
  ed.setText("old");
  assert.equal(text, "edited", "the unconfirmed edit is still shown over the stale body");
  ed.setText("edited");
  ed.setText("edited elsewhere");
  assert.equal(text, "edited elsewhere", "the draft was released, so a newer remote body shows");
  change("typing");
  ed.setText("edited elsewhere again");
  assert.equal(text, "typing", "new typing is still protected");
});

test("a stale snapshot that matches the draft does not release it while a write is in flight or unsaved typing is waiting", async () => {
  const finish: ((ok: boolean) => void)[] = [];
  const ed = new BodyEditor(() => new Promise<boolean>((r) => { finish.push(r); }), "Body", "k2");
  ed.setText("stored");
  await settle();
  change("first");
  mock.timers.tick(SAVE_DELAY_MS);
  await settle();
  change("stored");                       // typed back to the old text; the debounce is running
  ed.setText("stored");                   // a stale refresh carrying the stored text
  ed.setText("first");                    // then the first save's confirming refresh
  assert.equal(text, "stored", "the newer typing was not reverted");
  mock.timers.tick(SAVE_DELAY_MS);
  finish[0](true);
  await settle();
  assert.equal(finish.length, 2, "the latest text is saved");
  ed.setText("first");
  assert.equal(text, "stored", "still protected while that save is in flight");
  finish[1](true);
  await settle();
  ed.setText("stored");
  ed.setText("remote");
  assert.equal(text, "remote", "released once everything settled");
});

test("an acknowledgement does not clear a draft that was typed back to the acknowledged text", async () => {
  const sent: string[] = [];
  const finish: ((ok: boolean) => void)[] = [];
  const ed = new BodyEditor((t) => { sent.push(t); return new Promise<boolean>((r) => { finish.push(r); }); }, "Body", "k3");
  ed.setText("stored");
  await settle();
  change("A");
  mock.timers.tick(SAVE_DELAY_MS);          // A is in flight
  change("B");
  mock.timers.tick(SAVE_DELAY_MS);          // B waits behind it
  change("A");                              // the user's final edit is A again, unsaved
  finish[0](true);                          // A's acknowledgement
  await settle();
  ed.setText("A");
  assert.equal(text, "A");
  ed.setText("B");                          // B's confirming snapshot must not replace the typing
  assert.equal(text, "A", "the draft survived A's acknowledgement");
  mock.timers.tick(SAVE_DELAY_MS);
  finish[1]?.(true);
  await settle();
  finish[2]?.(true);
  await settle();
  assert.equal(sent.at(-1), "A", "the last write is the user's final edit");
});
