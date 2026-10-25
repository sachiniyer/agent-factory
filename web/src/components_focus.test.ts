import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Pins the focus-containment invariants of the shared modal chrome. The trap
// used to be attached to the card, so once setBusy(true) disabled a focused
// footer button and the browser moved activeElement to <body>, a Tab keydown
// there never bubbled through the card and the trap's rescue branch was dead
// code — Tab free-walked the page behind the overlay (a WCAG 2.1.2/2.4.3
// failure). These cases lock the document-level trap, the setBusy focus move,
// and the setError refocus so that regression is caught on the host, fast.
//
// Extracts the REAL modalChrome source and runs it in a vm against a minimal
// DOM stub — the same harness used by modal_invoker.test.ts and
// rebind_project_flow.test.ts — so the logic under test is the shipped code.

function extractModalChrome(): string {
  const source = readFileSync(new URL("./components.ts", import.meta.url), "utf8");
  const ast = ts.createSourceFile("components.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find((s) =>
    ts.isFunctionDeclaration(s) && s.name?.text === "modalChrome");
  assert.ok(node, "components.ts must declare modalChrome");
  // Strip `export`: the sandbox runs module=CommonJS-style None, where an
  // `export` token would be a syntax error. The body is unchanged.
  return node.getText(ast).replace(/^export\s+/, "");
}

const COMPILED = ts.transpileModule(extractModalChrome(), {
  compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
}).outputText;

// Tags natively in the tab order with a default tabIndex of 0; anything else
// (div, h2, p, label, …) defaults to -1 unless an explicit tabindex is set — the
// DOM rule the trap's `tabIndex >= 0` filter relies on.
const NATIVELY_TABbable = new Set(["BUTTON", "INPUT", "SELECT", "TEXTAREA", "A"]);

class FakeEvent {
  key: string;
  shiftKey: boolean;
  defaultPrevented = false;
  constructor(key: string, shift = false) {
    this.key = key;
    this.shiftKey = shift;
  }
  preventDefault(): void { this.defaultPrevented = true; }
  stopPropagation(): void { /* the stub does not model propagation */ }
}

class FakeElement {
  tagName: string;
  className = "";
  readonly attributes: Record<string, string> = {};
  readonly children: FakeElement[] = [];
  text = "";
  parent: FakeElement | null = null;
  readonly listeners: Record<string, Array<(e: FakeEvent) => void>> = {};
  hidden = false;
  isConnected = true;
  readonly classList = {
    toggle: (cls: string, force?: boolean): void => {
      const present = (" " + this.className + " ").includes(" " + cls + " ");
      const next = force === undefined ? !present : force;
      if (next && !present) this.className = this.className ? `${this.className} ${cls}` : cls;
      else if (!next && present) this.className = (" " + this.className + " ").replace(` ${cls} `, " ").trim();
    },
  };
  private _disabled = false;
  private tabindexAttr: string | undefined;

  constructor(tag: string) { this.tagName = tag.toUpperCase(); }

  // Chromium moves document.activeElement to <body> when the focused element is
  // disabled — the exact behavior the reported bug depends on. Modeling it here
  // is what makes the setBusy "move focus before disabling" guard and the
  // busy→refused cycle catch the bug rather than passing trivially.
  get disabled(): boolean { return this._disabled; }
  set disabled(v: boolean) {
    this._disabled = v;
    if (v && doc.activeElement === this) doc.activeElement = docBody;
  }

  setAttribute(k: string, v: string): void { this.attributes[k] = v; if (k === "tabindex") this.tabindexAttr = v; }
  getAttribute(k: string): string | null { return this.attributes[k] ?? null; }
  append(...kids: Array<FakeElement | string>): void {
    for (const kid of kids) {
      if (typeof kid === "string") { this.text += kid; continue; }
      kid.parent = this;
      this.children.push(kid);
    }
  }
  replaceChildren(...kids: Array<FakeElement | string>): void {
    for (const c of this.children) c.parent = null;
    this.children.length = 0;
    this.text = "";
    this.append(...kids);
  }
  addEventListener(type: string, fn: (e: FakeEvent) => void): void {
    (this.listeners[type] ??= []).push(fn);
  }
  removeEventListener(type: string, fn: (e: FakeEvent) => void): void {
    this.listeners[type] = (this.listeners[type] ?? []).filter((f) => f !== fn);
  }
  remove(): void {
    this.isConnected = false;
    if (this.parent) {
      const idx = this.parent.children.indexOf(this);
      if (idx !== -1) this.parent.children.splice(idx, 1);
      this.parent = null;
    }
  }
  get tabIndex(): number {
    if (this.tabindexAttr !== undefined) return Number(this.tabindexAttr);
    return NATIVELY_TABbable.has(this.tagName) ? 0 : -1;
  }
  set tabIndex(v: number) { this.tabindexAttr = String(v); }
  contains(node: unknown): boolean {
    let cur = node as FakeElement | null | undefined;
    while (cur) { if (cur === this) return true; cur = cur.parent; }
    return false;
  }
  focus(): void { doc.activeElement = this; }
  getClientRects(): Array<{ width: number }> { return [{ width: 100 }]; }
  matches(selector: string): boolean { return selector === ":disabled" ? this.disabled : false; }
  querySelectorAll(selector: string): FakeElement[] {
    const out: FakeElement[] = [];
    const parts = selector.split(",").map((s) => s.trim());
    const walk = (el: FakeElement): void => {
      for (const child of el.children) {
        if (FakeElement.matchesSelector(child, parts)) out.push(child);
        walk(child);
      }
    };
    walk(this);
    return out;
  }
  private static matchesSelector(el: FakeElement, parts: string[]): boolean {
    for (const part of parts) {
      if (part === "[tabindex]") { if (el.tabindexAttr !== undefined) return true; continue; }
      if (part.startsWith("[")) {
        const end = part.indexOf("]");
        const attr = end === -1 ? part.slice(1) : part.slice(1, end);
        if (el.attributes[attr] !== undefined) return true;
        continue;
      }
      if (el.tagName === part.toUpperCase()) return true;
    }
    return false;
  }
}

interface Built {
  handle: { el: FakeElement; setBusy(busy: boolean): void; setError(msg: string | null): void; close(): void };
  body: FakeElement;
  confirmBtn: FakeElement;
  cancelBtn: FakeElement;
  errorLine: FakeElement;
}

const doc = { activeElement: null as FakeElement | null };
const docListeners: Record<string, Array<{ fn: (e: FakeEvent) => void; capture: boolean }>> = {};
const observableDoc = {
  get activeElement(): FakeElement | null { return doc.activeElement; },
  set activeElement(v: FakeElement | null) { doc.activeElement = v; },
  addEventListener(type: string, fn: (e: FakeEvent) => void, capture = false): void {
    (docListeners[type] ??= []).push({ fn, capture });
  },
  removeEventListener(type: string, fn: (e: FakeEvent) => void, capture = false): void {
    docListeners[type] = (docListeners[type] ?? []).filter((l) => l.fn !== fn || l.capture !== capture);
  },
};

// A stand-in for <body>: not in the card's subtree, so card.contains(body) is
// false — the exact state a focused-then-disabled button produces when the
// browser drops document.activeElement to <body>.
const docBody = new FakeElement("body");

function h(tag: string, props: Record<string, unknown> & { class?: string } = {}, ...kids: Array<FakeElement | string>): FakeElement {
  const el = new FakeElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") el.className = v as string;
    else (el as unknown as Record<string, unknown>)[k] = v;
  }
  for (const kid of kids) el.append(kid);
  return el;
}
function mutationNotice(..._args: unknown[]): FakeElement { return new FakeElement("div"); }

function build(): Built {
  doc.activeElement = null;
  for (const key of Object.keys(docListeners)) delete docListeners[key];
  const sandbox: Record<string, unknown> = { h, mutationNotice, document: observableDoc };
  runInNewContext(COMPILED, sandbox);
  const modalChrome = sandbox.modalChrome as (o: unknown) => Built;
  return modalChrome({ title: "Test", confirmLabel: "Confirm", confirmClass: "af-primary", onCancel: () => {} });
}

function dispatchKey(key: string, shift = false): FakeEvent {
  const ev = new FakeEvent(key, shift);
  for (const { fn } of docListeners.keydown ?? []) fn(ev);
  return ev;
}

function cardOf(built: Built): FakeElement { return built.handle.el.children[0]; }

test("the Tab trap is installed at the document level in capture phase, not on the card", () => {
  const built = build();
  assert.equal((docListeners.keydown ?? []).length, 1, "exactly one document keydown listener");
  assert.equal(docListeners.keydown[0].capture, true, "the trap uses the capture phase");
  assert.equal(cardOf(built).listeners.keydown, undefined, "no card-level trap remains");
});

test("close() unregisters the document trap and removes the backdrop", () => {
  const built = build();
  built.handle.close();
  assert.equal((docListeners.keydown ?? []).length, 0, "close removes the trap listener");
  assert.equal(built.handle.el.isConnected, false, "close removes the backdrop from the DOM");
});

test("setBusy(true) moves focus off a focused footer button before disabling it", () => {
  const built = build();
  built.confirmBtn.focus();
  built.handle.setBusy(true);
  assert.equal(observableDoc.activeElement, cardOf(built), "focus moved inside the dialog before disabling");
  assert.equal(built.confirmBtn.disabled, true);
  assert.equal(built.cancelBtn.disabled, true);
});

test("setBusy(true) leaves a focused input alone so a user can keep typing mid-RPC", () => {
  const built = build();
  const input = new FakeElement("input");
  built.body.append(input);
  input.focus();
  built.handle.setBusy(true);
  assert.equal(observableDoc.activeElement, input, "the cursor stays in the field");
  assert.equal(built.confirmBtn.disabled, true, "buttons still disable");
  assert.equal(built.cancelBtn.disabled, true);
});

test("setError pulls focus back into the dialog when activeElement escaped to <body>", () => {
  const built = build();
  observableDoc.activeElement = docBody;
  built.handle.setError("daemon refused");
  assert.equal(observableDoc.activeElement, cardOf(built), "focus is pulled back inside");
  assert.equal(built.errorLine.hidden, false, "the error is shown");
});

test("setError leaves focus on a field the user is editing", () => {
  const built = build();
  const input = new FakeElement("input");
  built.body.append(input);
  input.focus();
  built.handle.setError("title taken");
  assert.equal(observableDoc.activeElement, input, "a validation error does not yank the cursor");
});

test("Tab is recovered into the dialog after focus escaped to <body> (the reported bug)", () => {
  const built = build();
  observableDoc.activeElement = docBody; // the state a disabled focused button produces
  const ev = dispatchKey("Tab", false);
  assert.equal(ev.defaultPrevented, true, "the rescue branch now fires from the body-escaped state");
  assert.equal(observableDoc.activeElement, built.cancelBtn, "focus is pulled back to the first control");
});

test("a busy-then-refused cycle keeps focus inside the dialog throughout", () => {
  // The everyday path from the report: a footer button had focus when the RPC
  // started, the daemon refuses, and the modal stays open with an error. Focus
  // must never land on <body>, and Tab must stay contained the whole time.
  const built = build();
  built.confirmBtn.focus();
  built.handle.setBusy(true);
  assert.notEqual(observableDoc.activeElement, docBody, "focus did not escape to body during busy");
  dispatchKey("Tab", false);
  assert.notEqual(observableDoc.activeElement, docBody, "Tab during busy did not escape the dialog");
  built.handle.setBusy(false);
  built.handle.setError("daemon refused");
  assert.notEqual(observableDoc.activeElement, docBody, "focus did not escape to body after the error");
  dispatchKey("Tab", false);
  assert.notEqual(observableDoc.activeElement, docBody, "Tab after the error did not escape the dialog");
  assert.equal(built.errorLine.hidden, false);
  assert.equal(built.handle.el.isConnected, true);
});
