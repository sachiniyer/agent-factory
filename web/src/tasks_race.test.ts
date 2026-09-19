import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

// Reproduces the disconnect/reconnect race in the modal-less task/recovery RPC
// handlers doTriggerTask, toggleTask, and doRetryLimit (web/src/index.ts). Their
// .catch handlers write post-await failures into the shared transient toast
// (surfaceTabError) and the persistent mutation banner (surfaceMutationError) with
// no post-await guard, so a rejection landing after a disconnect+reconnect writes the
// dead connection's error onto the new connection's UI. The fix re-validates the
// connection generation + token at the top of each .catch — the same gate the sibling
// handlers doOpenAccountLogin / doRegisterAccount / applyConfigValueNow use. These
// tests run the transpiled handlers in a VM sandbox with controllable RPC promises,
// mirroring the account_login_race.test.ts idiom.
//
// The assertion is structural: every store.set is recorded, and a stale rejection
// that survives the gate would write a non-null tabError / a non-undefined mutationError
// after a same-token reconnect (a token-only guard cannot tell the new connection from
// the old one; only the generation counter can, since disconnect and connect each bump
// it). The soundness controls confirm the gate does not mute live reporting on any
// branch it now covers — including the load-bearing doRetryLimit committed banner and
// the toggleTask committed refreshTasks()+tabError pair.

// A committed daemon rejection (isMutationCommittedError == true): routes doRetryLimit
// to surfaceMutationError("confirmed") and toggleTask to refreshTasks()+surfaceTabError.
class CommittedError extends Error {
  status = 200;
  code = "mutation_committed";
  constructor(message: string) {
    super(message);
    this.name = "CommittedError";
  }
}

// A transport/non-committed rejection (isMutationCommittedError == false): routes to
// surfaceTabError alone.
class TransportError extends Error {
  status = 500;
  code = "";
  constructor(message: string) {
    super(message);
    this.name = "TransportError";
  }
}

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

// A controllable RPC: settles only when the test calls resolveRPC/rejectRPC. Every task/
// recovery handler calls exactly one RPC, so a single shared promise is wired to all
// three (triggerTask / updateTask / resumeFromLimit) — each test invokes one handler.
function stage(opts: { withSelectedSession?: boolean } = {}): {
  app: {
    doTriggerTask(task: { id: string; enabled: boolean }): void;
    toggleTask(task: { id: string; enabled: boolean }): void;
    doRetryLimit(): void;
    disconnect(): void;
    simulateConnect(candidate: string): void;
  };
  resolveRPC(value: unknown): void;
  rejectRPC(err: unknown): void;
  refreshTasksCalls(): number;
  storeSets: Record<string, unknown>[];
  tabError(): unknown;
  mutationError(): unknown;
} {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set([
    "disconnect",
    "doRetryLimit",
    "toggleTask",
    "doTriggerTask",
  ]));

  let resolveRPC!: (value: unknown) => void;
  let rejectRPC!: (err: unknown) => void;
  const rpcPromise = new Promise<unknown>((resolve, reject) => {
    resolveRPC = resolve;
    rejectRPC = reject;
  });

  let refreshTasksCount = 0;
  // The shared store recorder: disconnect clears tabError/mutationError; a stale leak
  // would re-write one of them after the reconnect. Initial authRequired feeds
  // disconnect's default param `authRequired = store.get().authRequired`.
  let storeState: Record<string, unknown> = {
    authRequired: false,
    tabError: null,
    mutationError: undefined,
  };
  const storeSets: Record<string, unknown>[] = [];
  const store = {
    get: () => storeState,
    set(patch: Record<string, unknown>): void {
      storeState = { ...storeState, ...patch };
      storeSets.push(patch);
    },
  };

  const msg = (e: unknown): string => (e instanceof Error ? e.message : String(e));

  const context = {
    // The three RPCs share one controllable promise.
    triggerTask: () => rpcPromise,
    updateTask: () => rpcPromise,
    resumeFromLimit: () => rpcPromise,
    // refreshTasks is stubbed to a counter — the .then(refreshTasks) leg and the
    // committed-branch refreshTasks() both increment it.
    refreshTasks: () => {
      refreshTasksCount += 1;
    },
    isMutationCommittedError: (e: unknown): boolean => e instanceof CommittedError,
    // The two commit surfaces are stubbed to record the SAME store.set shape the real
    // functions would write, so the assertion is structural against the store.
    surfaceTabError: (e: unknown): void => {
      store.set({ tabError: msg(e), tabNotice: false });
    },
    surfaceMutationError: (e: unknown, kind: string): void => {
      store.set({ mutationError: { kind, detail: msg(e) } });
    },
    selectedSession: () => (opts.withSelectedSession === false ? null : { id: "s1", title: "Session One" }),
    // disconnect dependencies (transport/overlay teardown) are no-ops here.
    connectionGate: { invalidate() {} },
    stopStream() {},
    closeOverlays() {},
    clearToken() {},
    optimisticSessions: { reset() {} },
    pendingRestores: { reset() {} },
    store,
  };

  const code = ts.transpileModule(
    `
    let token = "token";
    let connectionGeneration = 0;
    function simulateConnect(candidate) { token = candidate; connectionGeneration++; }
    ${handlers}
    `,
    { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } },
  ).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & {
    doTriggerTask(task: { id: string; enabled: boolean }): void;
    toggleTask(task: { id: string; enabled: boolean }): void;
    doRetryLimit(): void;
    disconnect(): void;
    simulateConnect(candidate: string): void;
  };

  return {
    app: {
      doTriggerTask: app.doTriggerTask.bind(app),
      toggleTask: app.toggleTask.bind(app),
      doRetryLimit: app.doRetryLimit.bind(app),
      disconnect: app.disconnect.bind(app),
      simulateConnect: app.simulateConnect.bind(app),
    },
    resolveRPC,
    rejectRPC,
    refreshTasksCalls: () => refreshTasksCount,
    storeSets,
    tabError: () => storeState.tabError,
    mutationError: () => storeState.mutationError,
  };
}

const TASK = { id: "t1", enabled: true };
// Two microtask hops is enough for the rejection to traverse the .then link and land in
// the .catch (account_login_race.test.ts uses the same for its catch-path controls).
const settle = async (): Promise<void> => {
  await Promise.resolve();
  await Promise.resolve();
};

// === Stale rejection across a same-token reconnect is dropped (the fix) ============

// doTriggerTask: a rejection landing after a same-token reconnect must not write the
// dead connection's error onto the new connection's toast. A token-only guard would
// fail here (reconnected token === captured tok); only the generation gate drops it.
test("doTriggerTask: stale rejection after same-token reconnect is dropped (no tabError leak)", async () => {
  const s = stage();
  s.app.doTriggerTask(TASK); // captures tok="token", requestGeneration=0
  s.app.disconnect(); // connectionGeneration -> 1, token -> null, store cleared
  s.app.simulateConnect("token"); // connectionGeneration -> 2, token -> "token"

  s.rejectRPC(new TransportError("connection reset"));
  await settle();

  assert.equal(s.tabError(), null, "the stale rejection did not leak a tabError onto the new connection");
  assert.equal(s.refreshTasksCalls(), 0, "the stale rejected .then(refreshTasks) leg did not run");
});

// toggleTask: a non-committed stale rejection across a same-token reconnect is dropped.
test("toggleTask: stale non-committed rejection after same-token reconnect is dropped (no tabError leak)", async () => {
  const s = stage();
  s.app.toggleTask(TASK);
  s.app.disconnect();
  s.app.simulateConnect("token");

  s.rejectRPC(new TransportError("connection reset"));
  await settle();

  assert.equal(s.tabError(), null, "the stale rejection did not leak a tabError onto the new connection");
  assert.equal(s.refreshTasksCalls(), 0, "the stale rejected .then(refreshTasks) leg did not run");
});

// toggleTask: a COMMITTED stale rejection is dropped too, and crucially the in-.catch
// refreshTasks() (the committed branch) does NOT fire — the gate sits ABOVE the
// committed branch, so a stale committed outcome neither surfaces a toast nor refetches.
test("toggleTask: stale committed rejection after same-token reconnect is dropped (gate is above the committed branch)", async () => {
  const s = stage();
  s.app.toggleTask(TASK);
  s.app.disconnect();
  s.app.simulateConnect("token");

  s.rejectRPC(new CommittedError("toggle committed but ambiguous"));
  await settle();

  assert.equal(s.tabError(), null, "the stale committed rejection did not leak a tabError");
  assert.equal(s.refreshTasksCalls(), 0, "the gate returned before the committed-branch refreshTasks() ran");
});

// doRetryLimit: the load-bearing case. A committed stale rejection raises a mutationError
// banner that persists across navigation (only dismissNotice/disconnect/connect clear it);
// the gate must drop it before surfaceMutationError runs.
test("doRetryLimit: stale committed rejection after same-token reconnect is dropped (no mutation banner leak)", async () => {
  const s = stage();
  s.app.doRetryLimit(); // captures tok="token", requestGeneration=0
  s.app.disconnect();
  s.app.simulateConnect("token");

  s.rejectRPC(new CommittedError("resume committed but ambiguous"));
  await settle();

  assert.equal(
    s.mutationError(),
    undefined,
    "the stale committed rejection did not raise a persistent mutationError banner on the new connection",
  );
});

// doRetryLimit: a non-committed stale rejection (transport timeout / 5xx) routes to the
// toast and must likewise be dropped across a reconnect.
test("doRetryLimit: stale non-committed rejection after same-token reconnect is dropped (no tabError leak)", async () => {
  const s = stage();
  s.app.doRetryLimit();
  s.app.disconnect();
  s.app.simulateConnect("token");

  s.rejectRPC(new TransportError("connection reset"));
  await settle();

  assert.equal(s.tabError(), null, "the stale non-committed rejection did not leak a tabError");
});

// A stale rejection landing while STILL disconnected (before any reconnect) is also
// dropped — disconnect nulls the token, so the token leg of the gate catches it even
// before a new connect bumps the generation.
test("doTriggerTask: stale rejection landing while still disconnected is dropped (no tabError after the disconnect cleared it)", async () => {
  const s = stage();
  s.app.doTriggerTask(TASK); // captures tok="token", requestGeneration=0
  s.app.disconnect(); // connectionGeneration -> 1, token -> null, tabError cleared to null

  s.rejectRPC(new TransportError("connection reset"));
  await settle();

  assert.equal(
    s.tabError(),
    null,
    "the stale rejection on the dead connection did not re-arm a tabError on the login view",
  );
});

// === Soundness: same-connection rejections still surface (no over-suppression) =====

// doTriggerTask: on the same connection a rejection still reaches surfaceTabError.
test("doTriggerTask: same-connection rejection still surfaces a tabError", async () => {
  const s = stage();
  s.app.doTriggerTask(TASK);
  s.rejectRPC(new TransportError("flow refused"));
  await settle();

  assert.notEqual(s.tabError(), null, "a live rejection still surfaces a tabError");
  assert.equal(s.tabError(), "flow refused");
  assert.equal(s.refreshTasksCalls(), 0, "the rejected .then(refreshTasks) leg did not run");
});

// toggleTask: a same-connection non-committed rejection still surfaces a tabError.
test("toggleTask: same-connection non-committed rejection still surfaces a tabError", async () => {
  const s = stage();
  s.app.toggleTask(TASK);
  s.rejectRPC(new TransportError("flow refused"));
  await settle();

  assert.equal(s.tabError(), "flow refused", "a live non-committed rejection still surfaces a tabError");
  assert.equal(s.refreshTasksCalls(), 0, "non-committed rejection does not call the committed-branch refreshTasks()");
});

// toggleTask: a same-connection COMMITTED rejection still calls refreshTasks() AND
// surfaces a tabError — proves the gate does not absorb the committed branch on a live
// connection.
test("toggleTask: same-connection committed rejection still calls refreshTasks AND surfaces a tabError", async () => {
  const s = stage();
  s.app.toggleTask(TASK);
  s.rejectRPC(new CommittedError("toggle committed but ambiguous"));
  await settle();

  assert.equal(s.refreshTasksCalls(), 1, "the committed-branch refreshTasks() still runs on a live connection");
  assert.equal(s.tabError(), "toggle committed but ambiguous", "the committed rejection still surfaces a tabError");
});

// doRetryLimit: a same-connection COMMITTED rejection still raises the mutationError
// banner (the live load-bearing path).
test("doRetryLimit: same-connection committed rejection still raises a mutationError banner (live path)", async () => {
  const s = stage();
  s.app.doRetryLimit();
  s.rejectRPC(new CommittedError("resume committed but ambiguous"));
  await settle();

  assert.ok(
    typeof s.mutationError() === "object" && s.mutationError() !== null,
    "a live committed rejection still raises a mutationError banner",
  );
  assert.deepEqual(s.mutationError(), { kind: "confirmed", detail: "resume committed but ambiguous" });
  assert.equal(s.tabError(), null, "the committed path routes to the banner, not the toast");
});

// doRetryLimit: a same-connection non-committed rejection still surfaces a tabError.
test("doRetryLimit: same-connection non-committed rejection still surfaces a tabError", async () => {
  const s = stage();
  s.app.doRetryLimit();
  s.rejectRPC(new TransportError("connection reset"));
  await settle();

  assert.equal(s.tabError(), "connection reset", "a live non-committed rejection still surfaces a tabError");
  assert.equal(s.mutationError(), undefined, "the non-committed path routes to the toast, not the banner");
});

// === Happy path: the .then(refreshTasks) leg still wires up on resolve ===============

// doTriggerTask + toggleTask: a resolve still runs the .then(refreshTasks) refetch — the
// fix gates only the .catch, not the .then leg. (The real refreshTasks is itself fenced
// by readToken; here it is a counter, confirming the wiring is intact.)
test("doTriggerTask: resolve still runs the .then(refreshTasks) refetch (the .then leg is not gated)", async () => {
  const s = stage();
  s.app.doTriggerTask(TASK);
  s.resolveRPC(undefined);
  await settle();

  assert.equal(s.refreshTasksCalls(), 1, "the .then(refreshTasks) leg ran on resolve");
  assert.equal(s.tabError(), null, "no failure surfaced on a successful trigger");
});

test("toggleTask: resolve still runs the .then(refreshTasks) refetch", async () => {
  const s = stage();
  s.app.toggleTask(TASK);
  s.resolveRPC(undefined);
  await settle();

  assert.equal(s.refreshTasksCalls(), 1, "the .then(refreshTasks) leg ran on resolve");
  assert.equal(s.tabError(), null);
});
