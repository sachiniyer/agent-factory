import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

// Reproduces the tokenless-client 401 recovery bug in connect()'s catch
// (web/src/index.ts). Before the fix, a rejected credential set
// loginCondition: "expired" but left authRequired at its stale-`false` value, so
// loginView re-routed to noAuthLoginView — which re-issued connect("") on every
// click and never surfaced a token field. The fix flips authRequired to true on
// shouldForgetToken, mirroring the resync path's disconnect(describeError(error),
// true). These tests run the transpiled connect() in a VM sandbox with a
// controllable fetchSessionSnapshot, exactly like account_login_race.test.ts.

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.name = "ApiError";
    this.status = status;
  }
}

// Faithful copy of shouldForgetToken (web/src/api.ts:103-105): 401 OR 403.
function shouldForgetToken(e: unknown): boolean {
  return e instanceof ApiError && (e.status === 401 || e.status === 403);
}

interface StorePatch {
  phase?: string;
  connecting?: boolean;
  authRequired?: boolean;
  loginError: string | null;
  loginCondition?: "unavailable" | "expired" | undefined;
  [k: string]: unknown;
}

interface Stage {
  connect(candidate: string): Promise<void>;
  sets: StorePatch[];
  readonly clears: number;
  setFetch(fn: (candidate: string) => Promise<unknown>): void;
}

function stage(): Stage {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set(["connect"]));

  const sets: StorePatch[] = [];
  let clears = 0;
  let fetchImpl: (candidate: string) => Promise<unknown> = async () => {
    throw new ApiError(401, "That token was rejected.");
  };

  const context = {
    ApiError,
    shouldForgetToken,
    clearToken: () => { clears += 1; },
    describeError: (e: unknown): string => (e instanceof Error ? e.message : String(e)),
    fetchSessionSnapshot: (candidate: string) => fetchImpl(candidate),
    connectionGate: { begin() { return { isCurrent: () => true }; } },
    pendingRestores: { reset() {}, beginSnapshot() { return 0; } },
    store: {
      get: () => ({}),
      set: (patch: StorePatch) => { sets.push(patch); },
    },
    // Success-path stubs (the catch path never reaches them; provided so a happy-
    // path control test can complete without the full dependency set).
    storeToken() {},
    listTasks: async () => [],
    fetchRegisteredProjects: async () => ({ projects: [], error: "" }),
    connectionAttemptMayCommit: () => true,
    reconcileProject: () => null,
    loadProjectChoice: () => null,
    projectRoots: () => [],
    optimisticSessions: { reset() {}, snapshotFence() { return 0; } },
    pickSelection: () => null,
    applySessions() {},
    startStream() {},
    resolveRoute() {},
    clearLoginRoute() {},
    requestResync() {},
    connectionGeneration: 0,
    resolvingRoute: false,
    pendingRestoreResync: false,
    token: null as string | null,
  };

  const code = ts.transpileModule(handlers, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & { connect(candidate: string): Promise<void> };
  return {
    connect: app.connect.bind(app),
    sets,
    get clears() { return clears; },
    setFetch: (fn) => { fetchImpl = fn; },
  };
}

test("a 401 on the empty-token credential flips authRequired so loginView routes to the paste form", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(401, "That token was rejected."); });

  await s.connect("");

  const last = s.sets[s.sets.length - 1];
  assert.equal(last.phase, "login");
  assert.equal(last.connecting, false);
  assert.equal(last.authRequired, true, "authRequired must flip to true so the paste-token form shows");
  assert.equal(last.loginCondition, "expired");
  assert.ok(last.loginError, "the rejection is surfaced as an actionable error");
  assert.equal(s.clears, 1, "the rejected credential is forgotten");
});

test("a 403 is treated identically to a 401 (shouldForgetToken covers both statuses)", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(403, "forbidden"); });

  await s.connect("");

  const last = s.sets[s.sets.length - 1];
  assert.equal(last.authRequired, true);
  assert.equal(last.loginCondition, "expired");
  assert.equal(s.clears, 1);
});

test("a rejected stored-token resume also flips authRequired (the paste form re-offers entry)", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(401, "rejected"); });

  await s.connect("stale-token");

  const last = s.sets[s.sets.length - 1];
  assert.equal(last.authRequired, true);
  assert.equal(last.loginCondition, "expired");
  assert.equal(s.clears, 1);
});

test("a transport failure (status 0) keeps the unavailable recovery path and does not flip authRequired", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(0, "cannot reach the daemon"); });

  await s.connect("");

  const last = s.sets[s.sets.length - 1];
  assert.equal(last.loginCondition, "unavailable");
  assert.equal(last.authRequired, undefined, "a transport failure is not evidence a token is now required");
  assert.equal(s.clears, 0, "the token is retained across a transport failure");
});

test("a 5xx is surfaced without flipping authRequired or forgetting the token", async () => {
  const s = stage();
  s.setFetch(async () => { throw new ApiError(500, "internal error"); });

  await s.connect("good-token");

  const last = s.sets[s.sets.length - 1];
  assert.equal(last.authRequired, undefined);
  assert.equal(last.loginCondition, undefined);
  assert.ok(last.loginError);
  assert.equal(s.clears, 0);
});

test("a successful connect commits phase app and does not touch authRequired (no happy-path regression)", async () => {
  const s = stage();
  s.setFetch(async () => ({
    sessions: [],
    operationLockTimeoutMs: undefined,
    operationClockMs: undefined,
    daemonBootId: undefined,
  }));

  await s.connect("good-token");

  const app = s.sets.find((p) => p.phase === "app");
  assert.ok(app, "the success path commits phase app");
  assert.equal(app!.authRequired, undefined, "the happy path does not flip authRequired");
  assert.equal(app!.connecting, false);
  assert.equal(s.clears, 0);
});
