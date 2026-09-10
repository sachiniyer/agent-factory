import { test } from "node:test";
import assert from "node:assert/strict";
import { replaceProjectMenuChildren } from "./project-menu-focus.js";

// Model the browser's focus loss on removal, including a changed row label/order.
class Control {
  dataset: Record<string, string> = {};
  disabled = false;
  constructor(key: string) { this.dataset.projectFocus = key; }
  focus() { doc.activeElement = this; }
}
const doc = { activeElement: null as Control | null };
class Menu {
  ownerDocument = doc;
  hidden = false;
  constructor(public children: Control[]) {}
  contains(node: unknown) { return this.children.includes(node as Control); }
  replaceChildren(...children: Control[]) {
    if (this.contains(doc.activeElement)) doc.activeElement = null;
    this.children = children;
  }
  querySelectorAll() { return this.children; }
}
function replace(menu: Menu, rows: Control[], fallback: Control) {
  replaceProjectMenuChildren(menu as unknown as HTMLElement, rows as unknown as HTMLElement[], fallback as unknown as HTMLElement);
}
for (const key of ["project:/work/todo-cli", "add", "delete"]) {
  test(`project refresh preserves focused ${key} by identity after a reorder`, () => {
    const old = new Control(key);
    const menu = new Menu([old]);
    old.focus();
    const replacement = new Control(key);
    replace(menu, [new Control("project:/work/inserted"), replacement], new Control("trigger"));
    assert.equal(doc.activeElement, replacement);
  });
}
test("a vanished or disabled project action returns focus to the switcher", () => {
  for (const removed of [true, false]) {
    const old = new Control("delete");
    const menu = new Menu([old]);
    old.focus();
    const fallback = new Control("trigger");
    const replacement = new Control("delete");
    replacement.disabled = true;
    replace(menu, removed ? [] : [replacement], fallback);
    assert.equal(doc.activeElement, fallback);
  }
});
test("a project refresh does not steal focus from another control", () => {
  const external = new Control("terminal");
  external.focus();
  replace(new Menu([new Control("add")]), [new Control("add")], new Control("trigger"));
  assert.equal(doc.activeElement, external);
});

test("a refresh returns focus to the switcher after the project menu closes", () => {
  const old = new Control("project:/work/todo-cli");
  const menu = new Menu([old]);
  old.focus();
  menu.hidden = true;
  const replacement = new Control("project:/work/todo-cli");
  const fallback = new Control("trigger");
  replace(menu, [replacement], fallback);
  assert.equal(doc.activeElement, fallback);
});
