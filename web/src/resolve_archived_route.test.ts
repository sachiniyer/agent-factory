import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

import { ApiError, errorText, shouldForgetToken } from "./api.js";
import { settleLookup } from "./route_lookup.js";
import { CreateSelectionIntent, OptimisticSessions } from "./optimistic.js";
import { Liveness, type SessionData } from "./types.js";

// Pins the "could not check vs gone" fix in resolveArchivedRoute()'s catch
// (web/src/index.ts). Before the fix, a NON-auth failure of the by-id deep-link
// read (fetchSessionsById rejecting — transport `ApiError(0, …)` or a 5xx,
// neither of which shouldForgetToken covers) fell out of the catch with `rows`
// still []. Execution then reached settleLookup(..., rows = [], ...), whose
// empty-rows ⇒ missing semantics fired missing(): the pane reset, "No session
// with that id in this daemon" was toasted, and replaceRoute(null) erased the
// #/session/<id> hash — false-reporting a momentary blip as a deleted session and
// losing the shared deep link. The fix leaves the lookup unsettled on such
// failures (the hash stays owned until a successful answer, so a refresh or full
// reconnect can re-resolve the still-present fragment) and surfaces a transient
// "could not check" notice instead — matching adoptSessionsById's documented
// "Throws when the read fails, so the caller can tell 'gone' from 'could not
// check'" contract and the archive paging path's (loadArchivedPage) error+Retry
// surface. Only a fetch that ANSWERED without the id settles as missing.
//
// resolveArchivedRoute() is a top-level function in index.ts, which can't be
// imported directly (DOM/WS side effects on import), so these tests reuse the
// VM-extraction harness of connect_expired_auth.test.ts /
// resolve_route_filter.test.ts: read the source, AST-extract the named function,
// transpile, and run it in a sandbox with the real settleLookup /
// CreateSelectionIntent / OptimisticSessions / ApiError / shouldForgetToken /
// errorText and a controllable fetchSessionsById, stubbing only the
// side-effecting collaborators (store, replaceRoute, showTransientNotice,
// disconnect, applySessions, resolveRoute).

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

function archivedRow(id: string): SessionData {
  return { id, title: id, branch: "b", liveness: Liveness.Archived, worktree: { repo_path: "/repo" } };
}

interface StoreState {
  sessions: SessionData[];
  selectedId: string | null;
  view: string;
  focus: string;
  activeTab: number;
  [k: string]: unknown;
}

interface Stage {
  resolveArchivedRoute(id: string, ticket: number): Promise<void>;
  setFetch(fn: (tok: string, ids: string[]) => Promise<SessionData[]>): void;
  setToken(tok: string | null): void;
  bumpGeneration(): void;
  snapshot(): number;
  navigate(): void;
  readonly state: StoreState;
  readonly sets: Partial<StoreState>[];
  readonly notices: string[];
  readonly replaceRouteCalls: unknown[];
  readonly disconnectCalls: number;
  readonly applySessionsCalls: number;
  readonly resolveRouteCalls: number;
}

function stage(): Stage {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set(["resolveArchivedRoute"]));

  const sets: Partial<StoreState>[] = [];
  const notices: string[] = [];
  const replaceRouteCalls: unknown[] = [];
  let disconnectCalls = 0;
  let applySessionsCalls = 0;
  let resolveRouteCalls = 0;

  let state: StoreState = {
    sessions: [],
    selectedId: null,
    view: "sessions",
    focus: "rail",
    activeTab: 0,
  };

  const store = {
    get: () => state,
    set: (patch: Partial<StoreState>) => {
      sets.push(patch);
      state = { ...state, ...patch };
    },
  };

  // Real collaborators so the lookup contract (settleLookup's staleness gate,
  // the optimisticSessions adopt fence, the intent clock) is exercised honestly.
  const createSelectionIntent = new CreateSelectionIntent();
  const optimisticSessions = new OptimisticSessions();

  let fetchImpl: (tok: string, ids: string[]) => Promise<SessionData[]> = async () => [];

  const context = {
    token: "tok" as string | null,
    connectionGeneration: 0,
    optimisticSessions,
    createSelectionIntent,
    fetchSessionsById: (tok: string, ids: string[]) => fetchImpl(tok, ids),
    shouldForgetToken,
    errorText,
    describeError: (e: unknown): string => (e instanceof Error ? e.message : String(e)),
    disconnect: () => { disconnectCalls += 1; },
    showTransientNotice: (msg: string) => { notices.push(msg); },
    replaceRoute: (route: unknown) => { replaceRouteCalls.push(route); },
    applySessions: () => { applySessionsCalls += 1; },
    resolveRoute: () => { resolveRouteCalls += 1; },
    store,
    settleLookup,
  };

  const code = ts.transpileModule(handlers, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & {
    resolveArchivedRoute(id: string, ticket: number): Promise<void>;
  };
  return {
    resolveArchivedRoute: app.resolveArchivedRoute.bind(app),
    setFetch: (fn) => { fetchImpl = fn; },
    setToken: (tok) => { context.token = tok; },
    bumpGeneration: () => { context.connectionGeneration += 1; },
    snapshot: () => createSelectionIntent.snapshot(),
    navigate: () => createSelectionIntent.navigate(),
    get state() { return state; },
    sets,
    notices,
    replaceRouteCalls,
    get disconnectCalls() { return disconnectCalls; },
    get applySessionsCalls() { return applySessionsCalls; },
    get resolveRouteCalls() { return resolveRouteCalls; },
  };
}

test("a 5xx by-id read does NOT settle as missing: no 'No session' toast, hash preserved, no disconnect", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(500, "internal error"); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 0, "a 5xx is not an auth failure: no disconnect");
  assert.equal(s.replaceRouteCalls.length, 0, "the #/session/<id> hash is NOT erased (replaceRoute(null) not called)");
  assert.equal(s.sets.length, 0, "no missing() patch resets the pane");
  assert.equal(s.applySessionsCalls, 0, "no row adopted from a failed read");
  assert.equal(s.resolveRouteCalls, 0, "the route is not re-run from a failed read");
  assert.equal(s.notices.length, 1, "exactly one 'could not check' toast, not the false 'missing' one");
  assert.match(s.notices[0], /Could not look up that session/, "the toast says the lookup failed, not that the session is missing");
  assert.doesNotMatch(s.notices[0], /No session with that id/, "no false 'session was deleted' message");
  assert.match(s.notices[0], /link is preserved/, "the toast tells the user the deep link is kept");
});

test("a transport failure (status 0) behaves identically to a 5xx — 'could not check', not 'gone'", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(0, "cannot reach the daemon: network error"); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 0, "a transport failure is not an auth failure");
  assert.equal(s.replaceRouteCalls.length, 0, "the hash is preserved");
  assert.equal(s.sets.length, 0, "no pane reset");
  assert.equal(s.notices.length, 1);
  assert.match(s.notices[0], /Could not look up that session/);
  assert.match(s.notices[0], /cannot reach the daemon/, "the underlying transport error is surfaced in the toast");
});

test("a 401 by-id read STILL disconnects and does NOT toast or erase the hash", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(401, "That token was rejected."); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 1, "an auth failure disconnects");
  assert.equal(s.notices.length, 0, "no toast from this path — disconnect surfaces its own login feedback");
  assert.equal(s.replaceRouteCalls.length, 0, "the hash is not touched by the auth path");
  assert.equal(s.sets.length, 0, "no missing() patch");
});

test("a 403 is treated identically to a 401 (shouldForgetToken covers both statuses)", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(403, "forbidden"); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 1);
  assert.equal(s.notices.length, 0);
  assert.equal(s.replaceRouteCalls.length, 0);
});

test("a fetch that ANSWERED without the id STILL settles as missing — no regression in the 'gone' path", async () => {
  const s = stage();
  s.setFetch(async () => []); // daemon answered: no such id
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-gone", ticket);

  assert.equal(s.disconnectCalls, 0, "an answered-without-id read is not an auth failure");
  assert.equal(s.notices.length, 1);
  assert.equal(s.notices[0], "No session with that id in this daemon", "the definitive 'gone' toast still fires");
  assert.deepEqual(s.replaceRouteCalls, [null], "the hash IS erased for a genuinely missing id");
  assert.ok(
    s.sets.some((p) => p.selectedId === null && p.view === "sessions" && p.focus === "rail" && p.activeTab === 0),
    "the pane resets to the empty state",
  );
});

test("a fetch that ANSWERED with the row adopts it and re-runs the route — no toast, hash not erased by missing()", async () => {
  const s = stage();
  const row = archivedRow("s-arch");
  s.setFetch(async () => [row]);
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 0);
  assert.equal(s.notices.length, 0, "no toast on a successful lookup");
  assert.equal(s.replaceRouteCalls.length, 0, "missing()'s replaceRoute(null) did NOT fire; the route re-run owns the hash");
  assert.equal(s.applySessionsCalls, 1, "the adopted row is applied");
  assert.equal(s.resolveRouteCalls, 1, "the route re-runs against the list that now has the row");
});

test("a non-auth failure after a navigation superseded the ticket toasts nothing and settles nothing", async () => {
  // A rail click / new hash / back-forward since the lookup was issued made the
  // ticket stale (settleLookup's staleness contract). The user moved on, so a
  // transient 5xx on that stale lookup must not settle as missing AND must not
  // toast about a session the user navigated away from — that would itself be a
  // false report. The hash the user moved TO is owned by that newer navigation
  // and is left untouched here.
  const s = stage();
  s.setFetch(async () => { throw new ApiError(500, "internal error"); });
  const ticket = s.snapshot();
  s.navigate(); // supersede the ticket, as a rail click would

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.notices.length, 0, "no toast for a stale lookup — the user moved on");
  assert.equal(s.replaceRouteCalls.length, 0, "the hash the user moved to is not touched");
  assert.equal(s.sets.length, 0, "no pane reset");
  assert.equal(s.disconnectCalls, 0);
});

test("a non-auth failure after a disconnect (generation bumped) toasts nothing; the new connection owns the route", async () => {
  // A disconnect/reconnect during the read bumped connectionGeneration. The
  // stale lookup must not toast (the new connection's resolveRoute already
  // re-read location.hash and re-attempted against the preserved fragment) and
  // must not disconnect (a 5xx is not an auth failure).
  const s = stage();
  s.setFetch(async (tok) => { s.bumpGeneration(); throw new ApiError(500, "internal error"); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 0, "the 5xx is not an auth failure");
  assert.equal(s.notices.length, 0, "no toast — the connection changed; the new connection owns the feedback");
  assert.equal(s.replaceRouteCalls.length, 0, "the hash is left for the new connection's resolveRoute");
  assert.equal(s.sets.length, 0);
});

test("a tokenless (empty-string) connection still distinguishes 'could not check' from 'gone' for the deep link", async () => {
  // A tokenless ("") client is authorized (#1696); tok is "" (not null), so the
  // by-id read runs. A transport blip on it must still be 'could not check', not
  // 'missing' — the bug must not regress for the no-auth daemon path.
  const s = stage();
  s.setToken("");
  s.setFetch(async () => { throw new ApiError(0, "cannot reach the daemon"); });
  const ticket = s.snapshot();

  await s.resolveArchivedRoute("s-arch", ticket);

  assert.equal(s.disconnectCalls, 0);
  assert.equal(s.replaceRouteCalls.length, 0, "the hash is preserved on a tokenless client too");
  assert.equal(s.notices.length, 1);
  assert.match(s.notices[0], /Could not look up that session/);
});
