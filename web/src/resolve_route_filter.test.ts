import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

import { parseRoute, parseProjectRoute } from "./route.js";
import {
  defaultFilter,
  filterSessions,
  loadFilter,
  persistFilter,
  withKind,
  type StatusFilter,
} from "./filter.js";
import { operatorKind, type OperatorKind } from "./status.js";
import { Liveness, type SessionData } from "./types.js";

// Pins the deep-link filter-reveal fix in resolveRoute() (web/src/index.ts).
//
// Before the fix, the deep-link session arm of resolveRoute() reused the filter
// menu's setter — setStatusFilter(operatorKind(session), true) — whose body
// unconditionally calls persistFilter(). A purely navigational act (resolving
// /#/session/<id>) therefore overwrote the user's durable af-status-filter
// preference in localStorage: opening a link to an archived session turned
// archived:true on permanently, and opening one to a session in a kind the user
// had narrowed re-enabled that kind permanently. The fix applies the widened
// filter to the store directly (a transient reveal for the visit) and leaves
// persistence to setStatusFilter / resetStatusFilter, which the genuine user
// actions still drive through persistFilter().
//
// resolveRoute() and setStatusFilter()/resetStatusFilter() are top-level
// functions in index.ts, which can't be imported directly (DOM/WS side effects
// on import), so these tests reuse the VM-extraction pattern of
// connect_expired_auth.test.ts: read the source, AST-extract the named function
// declarations, transpile, and run them in a sandbox with the real filter.ts /
// route.ts / status.ts helpers and a Map-backed localStorage on globalThis.

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

interface StoreState {
  phase: string;
  sessions: SessionData[];
  archive: { lazy: boolean; total: number };
  statusFilter: StatusFilter;
  selectedId: string | null;
  tasks: unknown[];
  registeredProjects: unknown[];
  [k: string]: unknown;
}

function sess(over: Partial<SessionData> = {}): SessionData {
  return { title: "s", branch: "b", ...over };
}

function archivedRow(id: string): SessionData {
  return sess({ id, title: id, liveness: Liveness.Archived, worktree: { repo_path: "/repo" } });
}

function workingRow(id: string): SessionData {
  return sess({ id, title: id, liveness: Liveness.Running, worktree: { repo_path: "/repo" } });
}

interface Stage {
  resolveRoute(): void;
  setStatusFilter(kind: OperatorKind, on: boolean): void;
  resetStatusFilter(): void;
  readonly state: StoreState;
  readonly sets: Partial<StoreState>[];
  readonly storage: Map<string, string>;
  setHash(hash: string): void;
  readonly switchProjectCalls: string[];
  readonly openFromRailCalls: string[];
  readonly replaceRouteCalls: unknown[];
  readonly durableFilter: StatusFilter;
  setDurableFilter(filter: StatusFilter): void;
  restore(): void;
}

/** Installs a Map-backed localStorage on the main-process globalThis so the real
 *  loadFilter/persistFilter (imported from filter.js, closed over that same
 *  global) read and write this test's map. Returns a restore fn. */
function stubLocalStorage(map: Map<string, string>): () => void {
  const prior = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    value: {
      getItem: (k: string) => map.get(k) ?? null,
      setItem: (k: string, v: string) => void map.set(k, v),
      removeItem: (k: string) => void map.delete(k),
    },
  });
  return () => {
    if (prior) Object.defineProperty(globalThis, "localStorage", prior);
    else delete (globalThis as { localStorage?: unknown }).localStorage;
  };
}

function stage(initial: Partial<StoreState> = {}): Stage {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(
    source,
    new Set(["resolveRoute", "setStatusFilter", "resetStatusFilter"]),
  );

  const storage = new Map<string, string>();
  const restoreStorage = stubLocalStorage(storage);

  const sets: Partial<StoreState>[] = [];
  const switchProjectCalls: string[] = [];
  const openFromRailCalls: string[] = [];
  const replaceRouteCalls: unknown[] = [];

  let state: StoreState = {
    phase: "app",
    sessions: [],
    archive: { lazy: false, total: 0 },
    statusFilter: defaultFilter(),
    selectedId: null,
    tasks: [],
    registeredProjects: [],
    ...initial,
  };

  const store = {
    get: () => state,
    set: (patch: Partial<StoreState>) => {
      sets.push(patch);
      state = { ...state, ...patch };
    },
  };

  const locationObj = { hash: "" };

  const context = {
    // Real helpers so the kind mapping and partitioning are honest.
    parseRoute,
    parseProjectRoute,
    withKind,
    operatorKind,
    defaultFilter,
    persistFilter,
    // resolveRoute reads/writes this module-level flag.
    resolvingRoute: false,
    // navigate()/snapshot() are all resolveRoute touches; the filter assertions
    // don't depend on the ordering ticket, so a stub is sufficient.
    createSelectionIntent: { navigate() {}, snapshot() { return 0; } },
    store,
    // Stubs for the side-effecting collaborators resolveRoute reaches for.
    stashLoginRoute() {},
    switchProject: (root: string) => { switchProjectCalls.push(root); },
    openFromRail: (id: string) => { openFromRailCalls.push(id); },
    replaceRoute: (route: unknown) => { replaceRouteCalls.push(route); },
    showTransientNotice() {},
    resolveArchivedRoute() {},
    pickerProjects: () => [],
    projectRoots: () => [],
    // resolveRoute reads location.hash.
    location: locationObj,
    // The module-level durable filter (mirrors loadFilter() at boot in the real
    // app). setStatusFilter/resetStatusFilter read and write it; resolveRoute's
    // deep-link arm reads it for the transient reveal but never writes it.
    durableFilter: loadFilter(),
  };

  const code = ts.transpileModule(handlers, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & {
    resolveRoute(): void;
    setStatusFilter(kind: OperatorKind, on: boolean): void;
    resetStatusFilter(): void;
  };

  return {
    resolveRoute: app.resolveRoute.bind(app),
    setStatusFilter: app.setStatusFilter.bind(app),
    resetStatusFilter: app.resetStatusFilter.bind(app),
    get state() { return state; },
    sets,
    storage,
    setHash: (h: string) => { locationObj.hash = h; },
    switchProjectCalls,
    openFromRailCalls,
    replaceRouteCalls,
    get durableFilter() { return context.durableFilter; },
    setDurableFilter: (f: StatusFilter) => { context.durableFilter = f; },
    restore: restoreStorage,
  };
}

test("a deep link to an archived session reveals it in the rail WITHOUT persisting the filter", () => {
  const s = stage({ sessions: [archivedRow("s-arch")] });
  try {
    s.setHash("#/session/s-arch");

    s.resolveRoute();

    // The reveal works for the visit: the in-memory filter now admits Archived…
    assert.equal(s.state.statusFilter.archived, true, "Archived is revealed for the visit");
    // …which is exactly what the rail draws — the deep-linked row is visible.
    assert.deepEqual(
      filterSessions(s.state.sessions, s.state.statusFilter).map((r) => r.id),
      ["s-arch"],
      "the deep-linked archived row is shown",
    );
    // …and the row was opened.
    assert.deepEqual(s.openFromRailCalls, ["s-arch"]);

    // But NOTHING was written to localStorage — the durable preference is intact.
    assert.equal(s.storage.has("af-status-filter"), false, "no localStorage write on navigation");
    // So a fresh page load resumes the default (archived hidden), not the reveal.
    assert.equal(loadFilter().archived, false, "loadFilter resumes the untouched default");
  } finally {
    s.restore();
  }
});

test("a deep link to a working session widens the rail for the visit without touching the saved narrow", () => {
  // The user deliberately narrowed working OFF and that choice is what's stored.
  const narrowed = withKind(defaultFilter(), "working", false);
  const s = stage({ sessions: [workingRow("s-work")], statusFilter: narrowed });
  try {
    persistFilter(narrowed);
    assert.equal(s.storage.get("af-status-filter"), JSON.stringify(narrowed), "fixture: narrow is stored");
    s.setHash("#/session/s-work");

    s.resolveRoute();

    // The reveal works for the visit…
    assert.equal(s.state.statusFilter.working, true, "Working is revealed for the visit");
    assert.deepEqual(
      filterSessions(s.state.sessions, s.state.statusFilter).map((r) => r.id),
      ["s-work"],
      "the deep-linked working row is shown",
    );
    assert.deepEqual(s.openFromRailCalls, ["s-work"]);

    // …but the stored narrow is untouched — no navigational widening of the preference.
    assert.equal(
      s.storage.get("af-status-filter"),
      JSON.stringify(narrowed),
      "the stored narrow is unchanged by the deep link",
    );
    // So the very next page load resumes the user's narrow, not the visit's reveal.
    assert.equal(loadFilter().working, false, "loadFilter resumes the user's saved narrow");
  } finally {
    s.restore();
  }
});

test("re-resolving the route (the lazy-archive re-entry path) still does not persist the filter", () => {
  // resolveArchivedRoute() ends by re-running resolveRoute(); the fix lives in
  // resolveRoute's session arm, so that re-entry must be clean too. Calling
  // resolveRoute twice simulates the re-resolve without the async by-id lookup.
  const s = stage({ sessions: [archivedRow("s-arch")] });
  try {
    s.setHash("#/session/s-arch");
    s.resolveRoute();
    s.resolveRoute();

    assert.equal(s.state.statusFilter.archived, true, "the reveal still applies");
    assert.equal(s.storage.has("af-status-filter"), false, "two resolves write nothing");
    assert.equal(loadFilter().archived, false, "still the default after re-entry");
  } finally {
    s.restore();
  }
});

test("setStatusFilter (the user's explicit toggle) STILL persists — no happy-path regression", () => {
  const s = stage({ statusFilter: defaultFilter() });
  try {
    assert.equal(s.storage.has("af-status-filter"), false, "nothing stored at first");

    s.setStatusFilter("archived", true);

    assert.equal(s.state.statusFilter.archived, true, "the toggle updates the store");
    assert.equal(
      s.storage.get("af-status-filter"),
      JSON.stringify(withKind(defaultFilter(), "archived", true)),
      "the user's toggle is persisted to localStorage",
    );
  } finally {
    s.restore();
  }
});

test("resetStatusFilter (the menu's reset) STILL persists the default — no happy-path regression", () => {
  const narrowed = withKind(defaultFilter(), "archived", true);
  const s = stage({ statusFilter: narrowed });
  try {
    persistFilter(narrowed);
    assert.equal(s.storage.get("af-status-filter"), JSON.stringify(narrowed));

    s.resetStatusFilter();

    assert.equal(s.state.statusFilter.archived, false, "the reset restores the default in store");
    assert.equal(
      s.storage.get("af-status-filter"),
      JSON.stringify(defaultFilter()),
      "the reset is persisted to localStorage",
    );
  } finally {
    s.restore();
  }
});

test("a menu toggle after a deep-link reveal does NOT persist the transient widening", () => {
  // Pins the review follow-up to the deep-link fix. A deep link widens the
  // in-memory statusFilter for the visit (archived:true so the row shows), but
  // that widening is route-only. A subsequent explicit menu change must base
  // persistence on the DURABLE filter (the user's saved preference), not the
  // transiently-widened in-memory one — otherwise the route-only archived:true
  // becomes durable after an unrelated checkbox edit, and the next reload
  // still shows Archived despite this PR's intent (#6323).
  const narrowed = withKind(defaultFilter(), "working", false);
  const s = stage({ sessions: [archivedRow("s-arch")], statusFilter: narrowed });
  try {
    persistFilter(narrowed);
    // Mirror the user's saved narrow as the durable filter (in the real app,
    // durableFilter = loadFilter() at boot reads this stored narrow).
    s.setDurableFilter(narrowed);
    s.setHash("#/session/s-arch");

    s.resolveRoute();

    // The visit reveals archived in the in-memory filter…
    assert.equal(s.state.statusFilter.archived, true, "the deep link reveals archived");
    // …but the durable filter is untouched by the route-only widening.
    assert.deepEqual(s.durableFilter, narrowed, "the durable filter keeps the saved narrow");

    // Now an explicit menu toggle of an unrelated kind persists from the durable
    // filter, excluding the transient archived:true.
    s.setStatusFilter("needs-you", false);

    const stored = s.storage.get("af-status-filter");
    assert.ok(stored, "the menu toggle persisted");
    const persisted = JSON.parse(stored) as StatusFilter;
    assert.equal(persisted.archived, false, "the transient archived widening is NOT persisted");
    assert.equal(persisted.working, false, "the saved narrow on working is preserved");
    assert.equal(persisted["needs-you"], false, "the new toggle is applied");
    // And the in-memory store now matches durable+toggle, dropping the reveal.
    assert.equal(s.state.statusFilter.archived, false, "the reveal is dropped from the store");
  } finally {
    s.restore();
  }
});
