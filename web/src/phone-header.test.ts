import { test } from "node:test";
import assert from "node:assert/strict";
import * as components from "./components.js";

// Small eventful DOM for component units; real geometry is asserted by the recorder.
class Element extends EventTarget {
  children: Element[] = [];
  className = "";
  hidden = false;
  title = "";
  textContent = "";
  attrs = new Map<string, string>();
  constructor(readonly tagName = "div") { super(); }
  append(...children: (Element | string)[]) {
    for (const child of children) {
      if (typeof child === "string") this.textContent += child;
      else this.children.push(child);
    }
  }
  replaceChildren(...children: (Element | string)[]) { this.children = []; this.textContent = ""; this.append(...children); }
  setAttribute(key: string, value: string) { this.attrs.set(key, value); }
  getAttribute(key: string) { return this.attrs.get(key) ?? null; }
  contains(node: Element): boolean { return node === this || this.children.some(child => child.contains(node)); }
  focus() { doc.activeElement = this; }
  click() { this.dispatchEvent(new Event("click")); }
}
const doc = Object.assign(new EventTarget(), {
  activeElement: null as Element | null,
  createElement: (tag: string) => new Element(tag),
  createElementNS: (_ns: string, tag: string) => new Element(tag),
});
Object.assign(globalThis, { document: doc });
function media(width: number) {
  return Object.assign(new EventTarget(), { matches: width <= 768 }) as unknown as MediaQueryList;
}
function controls(width: number) {
  const theme = new Element();
  theme.append("Light · Dark · System");
  const disconnect = new Element("button");
  disconnect.append("Disconnect");
  const query = media(width);
  const create = components.appbarControls;
  assert.equal(typeof create, "function", "app controls must reuse a responsive disclosure component");
  return { menu: create([theme, disconnect] as unknown as HTMLElement[], query), query, theme, disconnect };
}

test("phone header at 360 folds secondary controls; desktop renders the same controls inline", () => {
  for (const width of [360, 1440]) {
    const { menu, theme, disconnect } = controls(width);
    assert.equal(menu.trigger.hidden, width > 768);
    assert.equal(menu.panel.hidden, width <= 768);
    assert.ok((menu.panel as unknown as Element).contains(theme));
    assert.ok((menu.panel as unknown as Element).contains(disconnect));
    menu.close();
    assert.equal(menu.panel.hidden, width <= 768, "desktop controls stay exposed when another menu opens");
    menu.dispose();
  }
});

test("phone overflow has native keyboard activation and Escape returns focus to its trigger", () => {
  const { menu, query } = controls(360);
  assert.equal((menu.trigger as unknown as Element).tagName, "button");
  menu.trigger.click(); // Native Enter/Space dispatch click on a button.
  assert.equal(menu.panel.hidden, false);
  assert.equal(menu.trigger.getAttribute("aria-expanded"), "true");
  const escape = Object.assign(new Event("keydown", { cancelable: true }), { key: "Escape" });
  menu.el.dispatchEvent(escape);
  assert.equal(menu.panel.hidden, true);
  assert.equal(doc.activeElement, menu.trigger);
  assert.equal(escape.defaultPrevented, true);
  menu.open();
  Object.assign(query, { matches: false });
  query.dispatchEvent(new Event("change"));
  assert.equal(menu.trigger.hidden, true);
  assert.equal(menu.panel.hidden, false);
  const desktopControl = (menu.panel as unknown as Element).children[1];
  desktopControl.focus();
  menu.el.dispatchEvent(Object.assign(new Event("keydown"), { key: "Escape" }));
  assert.equal(doc.activeElement, desktopControl, "desktop Escape must not focus a hidden trigger");
  Object.assign(query, { matches: true });
  query.dispatchEvent(new Event("change"));
  assert.equal(menu.panel.hidden, true, "returning to phone closes the overflow");
  menu.dispose();
});

test("truncated session title retains the complete name for pointer and accessible text", () => {
  const title = "Review the focused-session header across all phone widths";
  const chrome = components.terminalChrome({ title, copyLink() {}, handoff() {}, retry() {} });
  assert.equal(chrome.title.title, title, "ellipsis must have a full-name route");
  assert.equal(chrome.title.textContent, title);
  assert.equal((chrome.keyboard as unknown as Element).tagName, "span", "Keyboard is static");
  chrome.dispose();
});
