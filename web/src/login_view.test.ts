import { test } from "node:test";
import assert from "node:assert/strict";
import { loginView, noAuthLoginView, type AppState, type Actions } from "./ui.js";

// Behavioral coverage for the login view routing. The login view had no
// behavioral tests (ui.test.ts exercises only selectors, overlay_teardown.test.ts
// stubs renderLogin as a no-op), which is why a stale-authRequired 401 could strand
// a tokenless client on a self-contradictory "No token needed." screen that re-issued
// the empty-token request just rejected. These tests instantiate loginView /
// noAuthLoginView against a hand-rolled eventful DOM (the same approach as
// phone-header.test.ts): createElement feeds h(), scopeRecovery reads
// document.documentElement for currentMode(), and EventTarget drives the submit
// handlers.

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
function findInput(root: MockEl): MockEl | undefined {
  return walk(root).find((e) => e.tagName === "input");
}
function findByTag(root: MockEl, tag: string): MockEl[] {
  return walk(root).filter((e) => e.tagName === tag);
}
function prop(el: MockEl, name: string): unknown {
  return (el as unknown as Record<string, unknown>)[name];
}

function baseState(over: Partial<AppState> = {}): AppState {
  return {
    authRequired: false,
    connecting: false,
    loginError: null,
    phase: "login",
    ...over,
  } as AppState & Partial<AppState>;
}

function recordingActions(): Actions & { connectCalls: string[] } {
  const calls: string[] = [];
  const actions = {
    connect(token: string) { calls.push(token); },
  } as unknown as Actions;
  return Object.assign(actions, { connectCalls: calls });
}

test("a tokenless client (authRequired false, no error) sees the one-click tokenless Connect", () => {
  const actions = recordingActions();
  const el = loginView(baseState(), actions) as unknown as MockEl;

  assert.match(el.textContent, /No token needed\./);
  assert.equal(findInput(el), undefined, "no token field is offered when the daemon exempts this peer");
  assert.doesNotMatch(el.textContent, /Login expired/);

  const form = findByTag(el, "form")[0];
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  assert.deepEqual((actions as unknown as { connectCalls: string[] }).connectCalls, [""],
    "the tokenless form re-issues the empty-token sentinel");
});

test("a tokenless client whose login expired degrades to the paste form (defense in depth)", () => {
  const actions = recordingActions();
  const state = baseState({
    loginCondition: "expired",
    loginError: "That token was rejected. Check `af token show` on the host and try again.",
  });
  const el = noAuthLoginView(state, actions) as unknown as MockEl;

  // The self-contradictory "No token needed." copy must not appear next to a
  // token-rejection error.
  assert.doesNotMatch(el.textContent, /No token needed\./);
  assert.match(el.textContent, /Login expired/);
  assert.match(el.textContent, /af token show/);

  const input = findInput(el);
  assert.ok(input, "a token field is surfaced so the user can recover in-app");
  assert.equal(prop(input!, "type"), "password");
  assert.equal(prop(input!, "id"), "af-token");
  assert.match(el.textContent, /That token was rejected\./);

  // Submitting the degraded form pastes the entered token, not the empty sentinel.
  (input! as unknown as Record<string, unknown>).value = "new-token";
  const form = findByTag(el, "form")[0];
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  assert.deepEqual((actions as unknown as { connectCalls: string[] }).connectCalls, ["new-token"]);
});

test("the degraded tokenless form does not submit an empty token", () => {
  const actions = recordingActions();
  const state = baseState({ loginCondition: "expired", loginError: "rejected" });
  const el = noAuthLoginView(state, actions) as unknown as MockEl;

  const form = findByTag(el, "form")[0];
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  assert.deepEqual((actions as unknown as { connectCalls: string[] }).connectCalls, [],
    "an empty paste is not submitted as a credential");
});

test("loginView routes a stale-authRequired expired 401 to noAuthLoginView, which degrades (the reported bug)", () => {
  const actions = recordingActions();
  // The exact state connect()'s catch produced before the fix: authRequired still
  // false, loginCondition "expired". The primary fix flips authRequired to true so
  // loginView takes the paste-form branch directly; this test asserts the
  // defense-in-depth still holds if authRequired ever arrives stale.
  const state = baseState({ loginCondition: "expired", loginError: "That token was rejected." });
  const el = loginView(state, actions) as unknown as MockEl;

  assert.ok(findInput(el), "a token field is reachable without a page reload");
  assert.doesNotMatch(el.textContent, /No token needed\./);
  assert.match(el.textContent, /Login expired/);
});

test("loginView routes a required-auth expired login to the paste form with the Login expired title", () => {
  const actions = recordingActions();
  const state = baseState({ authRequired: true, loginCondition: "expired", loginError: "rejected" });
  const el = loginView(state, actions) as unknown as MockEl;

  const input = findInput(el);
  assert.ok(input);
  assert.equal(prop(input!, "type"), "password");
  assert.match(el.textContent, /Login expired/);
});

test("loginView routes a required-auth fresh login to the Sign in paste form", () => {
  const actions = recordingActions();
  const el = loginView(baseState({ authRequired: true }), actions) as unknown as MockEl;

  assert.ok(findInput(el));
  assert.match(el.textContent, /Sign in/);
  assert.doesNotMatch(el.textContent, /No token needed\./);

  const form = findByTag(el, "form")[0];
  form.dispatchEvent(new Event("submit", { cancelable: true }));
  assert.deepEqual((actions as unknown as { connectCalls: string[] }).connectCalls, [],
    "the paste form does not submit an empty token");
});

test("loginView routes an unavailable daemon to the recovery screen, not the tokenless view", () => {
  const actions = recordingActions();
  const el = loginView(baseState({ loginCondition: "unavailable", loginError: "cannot reach daemon" }), actions) as unknown as MockEl;

  assert.match(el.textContent, /Cannot reach the daemon/);
  assert.equal(findInput(el), undefined);
  assert.doesNotMatch(el.textContent, /No token needed\./);
});
