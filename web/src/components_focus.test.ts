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
  // The document-level Tab trap is swapped via a module-level `activeModalTrap`
  // that modalChrome reaps when a new modal opens. Extract that declaration too,
  // so the sandbox reproduces the real disposal behavior across modalChrome calls
  // (a per-call scope would leave every modal's trap live and break the reap).
  const trapDecl = ast.statements.find((s) =>
    ts.isVariableStatement(s) && s.declarationList.declarations.some(
      (d) => ts.isIdentifier(d.name) && d.name.text === "activeModalTrap"));
  assert.ok(trapDecl, "components.ts must declare the activeModalTrap swap slot");
  const declText = trapDecl.getText(ast).replace(/^export\s+/, "");
  // Strip `export`: the sandbox runs module=CommonJS-style None, where an
  // `export` token would be a syntax error. The body is unchanged.
  return `${declText}\n${node.getText(ast).replace(/^export\s+/, "")}`;
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
  handle: { el: FakeElement; setBusy(busy: boolean): void; setError(msg: string | null): void; close(): void; reactivate(): void };
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
  return buildN(1)[0];
}

function buildN(count: number): Built[] {
  doc.activeElement = null;
  for (const key of Object.keys(docListeners)) delete docListeners[key];
  const sandbox: Record<string, unknown> = { h, mutationNotice, document: observableDoc };
  runInNewContext(COMPILED, sandbox);
  const modalChrome = sandbox.modalChrome as (o: unknown) => Built;
  const built: Built[] = [];
  for (let i = 0; i < count; i++) {
    built.push(modalChrome({ title: "Test", confirmLabel: "Confirm", confirmClass: "af-primary", onCancel: () => {} }));
  }
  return built;
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

test("close() detaches the backdrop; the trap stays registered and inert while detached (no self-removal)", () => {
  const built = build();
  built.handle.close();
  assert.equal(built.handle.el.isConnected, false, "close removes the backdrop from the DOM");
  // The trap is NOT unregistered by close, and a keydown while the backdrop is
  // detached must NOT remove it: a refused create/kill/archive RPC closes this
  // modal, the RPC is in flight with the backdrop disconnected, and a Tab pressed
  // in that window used to self-remove the trap — stranding the remounted error
  // dialog with no Tab containment. The trap stays registered and is inert (it
  // skips the event without removing itself) until the next modalChrome reaps it.
  assert.equal((docListeners.keydown ?? []).length, 1, "close does not unregister the trap");
  const ev = dispatchKey("Tab", false);
  assert.equal(ev.defaultPrevented, false, "the trap is inert while the backdrop is detached");
  assert.equal((docListeners.keydown ?? []).length, 1, "a detached-window keydown does not remove the trap");
});

test("a close → remount cycle keeps the trap installed so Tab stays contained on the reopened modal", () => {
  // The reported regression: index.ts closeModal() then openModal(m) on a refused
  // create/kill/archive. close() detaches the backdrop; openModal re-inserts the
  // same handle. The trap must survive the round trip, otherwise the reopened
  // error dialog has no Tab containment and keyboard focus escapes the overlay.
  const built = build();
  built.handle.close();
  // openModal re-inserts the backdrop (mountHost.replaceChildren(m.el)).
  built.handle.el.isConnected = true;
  observableDoc.activeElement = docBody;
  const ev = dispatchKey("Tab", false);
  assert.equal(ev.defaultPrevented, true, "the trap still contains Tab after a remount");
  assert.notEqual(observableDoc.activeElement, docBody, "focus is pulled back inside the reopened dialog");
});

test("a keydown during the detached RPC window does not strand the trap on remount", () => {
  // The Codex finding on c32653e2: closeModal() detaches the backdrop while the
  // RPC is in flight, a Tab reaches the trap's disconnected guard, and the guard
  // used to self-remove — so the remounted error dialog had no Tab containment.
  // The trap must survive a keydown pressed during the detached interval.
  const built = build();
  built.handle.close();                  // closeModal: backdrop detaches, RPC in flight
  dispatchKey("Tab", false);             // a Tab during the detached window
  built.handle.el.isConnected = true;    // openModal(m) re-inserts the same handle on refusal
  observableDoc.activeElement = docBody;
  const ev = dispatchKey("Tab", false);
  assert.equal(ev.defaultPrevented, true, "the trap still contains Tab after a remount that followed a detached-window keydown");
  assert.notEqual(observableDoc.activeElement, docBody, "focus is pulled back into the reopened dialog");
});

test("opening a new modal reaps the previous modal's document-level trap", () => {
  // modalChrome swaps out the previous active trap so the live trap count is
  // bounded to one and a retained handle's trap is reclaimed once a different
  // modal takes over (the disposal path now that close() no longer removes it).
  // Two modalChrome calls in one sandbox share the module-level activeModalTrap,
  // which a per-build sandbox would not, so buildN(2) keeps them in one context.
  const [first, second] = buildN(2);
  assert.equal((docListeners.keydown ?? []).length, 1, "the second modal reaps the first and leaves one trap");
  // The first modal's trap is gone: with the first backdrop connected but the
  // second's not, only the second's trap would respond. Flip that — disconnect
  // the second and connect the first — and the first trap must NOT handle Tab.
  first.handle.el.isConnected = true;
  second.handle.el.isConnected = false;
  observableDoc.activeElement = docBody;
  const ev = dispatchKey("Tab", false);
  assert.equal(ev.defaultPrevented, false, "the reaped first trap no longer handles Tab");
});

test("reactivate reinstalls the trap after an intervening modal opened and closed", () => {
  // The Codex finding on 1b68319a: modal A is optimistically closed while its RPC
  // is pending, the user opens and closes modal B in between, and A is then
  // definitively refused. modalChrome(B) reaped A's trap, and close(B) left B's
  // inert trap as the live one. index.ts openModal re-inserts A's handle without
  // rebuilding it (no modalChrome), so A's reopened error dialog had no live Tab
  // trap. reactivate() reclaims the live slot for A's trap on the remount.
  const [a, b] = buildN(2);
  // A is optimistically closed while its RPC is in flight; B opens (reaping A's
  // trap) and then closes, leaving B's inert trap as the live one.
  a.handle.close();
  b.handle.close();
  // openModal(A) re-inserts A's backdrop but does NOT rebuild it, so the trap is
  // gone until reactivate reinstalls it. Before reactivate, Tab escapes A.
  a.handle.el.isConnected = true;
  observableDoc.activeElement = docBody;
  const beforeReactivate = dispatchKey("Tab", false);
  assert.equal(beforeReactivate.defaultPrevented, false,
    "before reactivate, the reaped A trap is gone and Tab escapes the remounted dialog");
  // reactivate() — the seam openModal calls on every (re)mount — reclaims the live
  // slot for A's trap, restoring Tab containment on the remounted handle.
  a.handle.reactivate();
  observableDoc.activeElement = docBody;
  const afterReactivate = dispatchKey("Tab", false);
  assert.equal(afterReactivate.defaultPrevented, true,
    "reactivate restored Tab containment on the remounted handle");
  assert.notEqual(observableDoc.activeElement, docBody, "focus is pulled back into the remounted dialog");
  assert.equal((docListeners.keydown ?? []).length, 1, "reactivate reaps the stale trap and leaves one live trap");
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
