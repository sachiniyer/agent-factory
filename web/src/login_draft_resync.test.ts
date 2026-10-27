import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { clearLoginDraft, renderLogin, type AppState, type Actions } from "./ui.js";

// Behavioral coverage for renderLogin's in-progress-token draft WeakMap, which had
// zero direct tests: login_view.test.ts exercises only the loginView/noAuthLoginView
// builders, and overlay_teardown.test.ts stubs renderLogin as a no-op. The WeakMap
// is module-private to ui.ts, so these tests drive the real renderLogin against a
// hand-rolled eventful DOM (the same MockEl approach as login_view.test.ts) extended
// with `querySelector` so renderLogin's `root.querySelector("#af-token")` resolves.
//
// The bug (commit 795885e): on a resync 401 the app phase returns to renderLogin with
// a truthy loginError and no #af-token in the DOM, so the `?? loginDrafts.get(root)`
// fallback pre-fills the paste form with the now-rejected token the user pasted
// earlier this session. The fix clears the stale in-memory draft in disconnect()
// (auth-driven only) via clearLoginDraft(root); connect()'s own catch does NOT go
// through disconnect(), so the just-typed draft there is preserved as intended.

class MockEl extends EventTarget {
  tagName: string;
  className = "";
  children: (MockEl | string)[] = [];
  attrs = new Map<string, string>();
  value = "";
  constructor(tag: string) {
    super();
    this.tagName = tag;
  }
  append(...kids: (MockEl | string)[]): void {
    for (const kid of kids) this.children.push(kid);
  }
  replaceChildren(...kids: (MockEl | string)[]): void {
    this.children = [];
    this.append(...kids);
  }
  setAttribute(k: string, v: string): void { this.attrs.set(k, v); }
  getAttribute(k: string): string | null { return this.attrs.get(k) ?? null; }
  // `h()` in dom.ts assigns `el.id = "af-token"` as a plain property, so querySelector
  // matches the live `id` property; setAttribute-based ids are covered too.
  querySelector(selector: string): MockEl | null {
    const out = descendants(this);
    if (selector.startsWith("#")) {
      const id = selector.slice(1);
      return out.find((e) => (e as { id?: unknown }).id === id || e.getAttribute("id") === id) ?? null;
    }
    return out.find((e) => e.tagName === selector) ?? null;
  }
  get classList(): { add(c: string): void; remove(c: string): void; contains(c: string): boolean } {
    const self = this;
    return {
      add(c: string) { self.className = self.className ? `${self.className} ${c}` : c; },
      remove(c: string) { self.className = self.className.split(" ").filter((x) => x !== c).join(" "); },
      contains(c: string) { return self.className.split(" ").includes(c); },
    };
  }
  get textContent(): string {
    return this.children.map((c) => (typeof c === "string" ? c : c.textContent)).join("");
  }
}

// Depth-first pre-order listing of an element's descendants (excluding self), the
// order a real querySelector walks the live DOM.
function descendants(el: MockEl, out: MockEl[] = []): MockEl[] {
  for (const c of el.children) {
    if (c instanceof MockEl) {
      out.push(c);
      descendants(c, out);
    }
  }
  return out;
}

const doc = Object.assign(new EventTarget(), {
  documentElement: { getAttribute: () => null },
  createElement: (tag: string) => new MockEl(tag),
  createElementNS: (_ns: string, tag: string) => new MockEl(tag),
});
Object.assign(globalThis, { document: doc });

function walk(el: MockEl, out: MockEl[] = []): MockEl[] {
  out.push(el);
  for (const c of el.children) if (c instanceof MockEl) walk(c, out);
  return out;
}
function h1Text(root: MockEl): string {
  return walk(root).filter((e) => e.tagName === "h1").map((e) => e.textContent).join("");
}
function tokenInput(root: MockEl): MockEl | undefined {
  return descendants(root).find((e) => (e as { id?: unknown }).id === "af-token" || e.getAttribute("id") === "af-token");
}

function baseState(over: Partial<AppState> = {}): AppState {
  return {
    authRequired: true,
    connecting: false,
    loginError: null,
    phase: "login",
    ...over,
  } as AppState & Partial<AppState>;
}

function recordingActions(): Actions {
  return { connect() {}, disconnect() {} } as unknown as Actions;
}

// renderLogin/clearLoginDraft are typed against the real DOM's HTMLElement; the
// hand-rolled MockEl stands in for it at runtime (same approach as
// split_reconcile_focus.test.ts's `null as unknown as HTMLElement`).
function render(root: MockEl, state: AppState): void {
  renderLogin(root as unknown as HTMLElement, state, recordingActions());
}
function clearDraft(root: MockEl): void {
  clearLoginDraft(root as unknown as HTMLElement);
}

// Drive renderLogin through the exact production sequence that exposes the bug:
// 1. the empty paste form is drawn (so #af-token exists in the DOM),
// 2. the user types a token into it,
// 3. a `connecting: true` re-render reads the typed value out of the OLD DOM and
//    seeds loginDrafts (loginView then returns connectingView, with no #af-token),
// 4. the app phase sits with that stale draft in the WeakMap.
function seedPasteTokenDraft(root: MockEl, typed: string): void {
  // 1. empty paste form
  render(root, baseState());
  const initial = tokenInput(root);
  assert.ok(initial, "the initial paste form renders a #af-token input");
  // 2. simulate the user typing
  initial!.value = typed;
  // 3. connecting re-render seeds the draft from the OLD DOM's input value
  render(root, baseState({ connecting: true }));
  assert.equal(tokenInput(root), undefined, "the connecting placeholder shows no token field");
  // 4. loginDrafts now holds `typed` for this root for the page lifetime
}

test("resync-401: clearLoginDraft prevents the rejected token from pre-filling the expired-login paste form (the fix)", () => {
  const root = new MockEl("div");
  seedPasteTokenDraft(root, "my-secret-token");

  // The resync catch calls disconnect(describeError(error), true), which (after the
  // fix) clears the in-memory draft BEFORE the phase flip re-renders login. The app
  // phase's DOM had no #af-token, so without the clear the ?? loginDrafts.get(root)
  // fallback would restore the stale credential.
  clearDraft(root);
  render(root, baseState({ loginError: "That token was rejected.", loginCondition: "expired" }));

  const input = tokenInput(root);
  assert.ok(input, "the expired paste form is rendered for re-entry");
  assert.equal(input!.value, "", "the now-rejected token is NOT pre-filled into the paste form");
  assert.match(h1Text(root), /Login expired/);
});

test("manual disconnect leaves the paste form empty without clearLoginDraft (no regression)", () => {
  const root = new MockEl("div");
  seedPasteTokenDraft(root, "manual-disconnect-token");

  // Manual Disconnect calls disconnect(null): loginError is null, so the fix's
  // `if (loginError)` guard skips clearLoginDraft. The renderLogin guard is then
  // falsy (no loginError / connecting / loginCondition), so `draft` resolves to ""
  // and loginDrafts is overwritten with "" — the form is correctly empty.
  render(root, baseState());

  const input = tokenInput(root);
  assert.ok(input);
  assert.equal(input!.value, "", "manual disconnect does not surface the old token");
  assert.match(h1Text(root), /Sign in/);
});

test("connect()-catch preserves the just-typed token (intended draft restoration is not broken)", () => {
  const root = new MockEl("div");
  seedPasteTokenDraft(root, "just-typed-token");

  // connect()'s own catch does NOT call disconnect()/clearLoginDraft: it store.sets
  // directly with loginCondition "expired". The connecting re-render just saved the
  // value the user typed seconds ago, so restoring it shows what they submitted —
  // the intended Config-refresh-survival behavior the WeakMap was added for.
  render(root, baseState({ loginError: "That token was rejected.", loginCondition: "expired" }));

  const input = tokenInput(root);
  assert.ok(input);
  assert.equal(input!.value, "just-typed-token", "the just-typed draft is preserved on the immediate connect failure");
  assert.match(h1Text(root), /Login expired/);
});

test("disconnect() clears the in-memory draft only on an auth rejection (wiring guard)", () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const disconnect = source.slice(source.indexOf("function disconnect("), source.indexOf("function tabIdsOf("));
  // The auth-driven clear must be present and gated on a truthy loginError so the
  // manual-Disconnect path (loginError null) is left to the natural falsy guard.
  assert.match(disconnect, /if \(loginError && root\) clearLoginDraft\(root\)/);
  // Sanity: disconnect still flips loginCondition to "expired" only for auth errors.
  assert.match(disconnect, /loginCondition: loginError \? "expired" : undefined/);
});
