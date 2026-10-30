import { afterEach, beforeEach, mock, test } from "node:test";
import assert from "node:assert/strict";

import { installFakeDom } from "./backlog_fakedom.js";
import { BodyEditor, SAVE_DELAY_MS, bodyEditorLoader } from "./backlog_body.js";

let dom: ReturnType<typeof installFakeDom>;
const realLoad = bodyEditorLoader.load;
let text: string;
let change: (t: string) => void;
let destroyed: boolean;

beforeEach(() => {
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
  const ed = new BodyEditor((t) => saves.push(t), "Body");
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
  const ed = new BodyEditor((t) => saves.push(t), "Body");
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
