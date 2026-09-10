import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";
import { PendingRestores } from "./pending_restores.js";

// Exercise the real handlers without starting a daemon or browser. The DOM stub
// models the important transition: optimistic removal disconnects the action and
// moves focus to body; rollback renders a fresh action before retaining the form.
for (const operation of ["kill", "archive"]) {
  test(`rejected optimistic ${operation} retains the original row invoker`, async () => {
    const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
    const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
    const names = new Set(["openModal", "closeModal", "openConfirm", "captureModalInvoker"]);
    const handlers = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
      .map(node => node.getText(ast)).join("\n");
    const doc = { activeElement: null as any, body: { closest: () => null, getAttribute: () => null, isConnected: true, getClientRects: () => [{}] } };
    let rowPresent = true;
    let focusedAction = false;
    let railFocused = false;
    let confirms: { onConfirm(): void };
    const session = { id: "original-session", title: "Original", is_root: false };
    const actionLabel = `${operation === "kill" ? "Kill" : "Archive"} session “Original”`;
    const focusable = (focus: () => void) => ({
      isConnected: true, getClientRects: () => [{}], matches: () => false,
      focus, closest: () => null, getAttribute: () => null,
    });
    const action = () => ({ ...focusable(() => { focusedAction = true; doc.activeElement = currentAction; }),
      closest: (selector: string) => selector === ".af-row" ? row : null,
      getAttribute: () => actionLabel,
    });
    const menu = {
      dataset: { sessionId: session.id },
      querySelector: (selector: string) => rowPresent &&
        (selector === "button" || selector.includes(actionLabel)) ? currentAction : null,
    };
    const row = { querySelector: () => menu };
    let currentAction = action();
    doc.activeElement = currentAction;
    const rail = focusable(() => { railFocused = true; });
    const card = focusable(() => { focusedAction = false; doc.activeElement = card; });
    let projectionCount = 0;
    const context = {
      isArchived: () => false, isOffBoxWorkspace: () => false,
      document: doc, CSS: { escape: (value: string) => value },
      getComputedStyle: () => ({ visibility: "visible" }),
      root: { querySelector: (selector: string) => selector.includes("data-session-id")
        ? rowPresent ? menu : null : selector === ".af-rail" ? rail : null },
      modalHost: { replaceChildren() {} }, closeConfigAssistant() {},
      focusRail() {}, token: "", store: { get: () => ({ sessions: [session] }), subscribe: () => () => {} },
      confirmModal: (options: typeof confirms) => {
        confirms = options;
        return { el: { querySelector: () => card }, close: () => { doc.activeElement = doc.body; }, setBusy() {}, setError() {} };
      },
      optimisticSessions: { begin: () => ({}), project: () => [], reject: () => "reverted" },
      pendingRestores: { has: () => false, captureArchiveSuccess: () => () => {} },
      applySessions: () => {
        projectionCount++;
        currentAction.isConnected = false;
        rowPresent = projectionCount > 1;
        if (rowPresent) currentAction = action();
        doc.activeElement = doc.body;
      },
      killSession: () => Promise.reject(new Error("Rejected")),
      archiveSession: () => Promise.reject(new Error("Rejected")),
      isMutationCommittedError: () => false, isMutationOutcomeUncertain: () => false,
      requestResync() {}, errorText: (error: Error) => error.message,
      surfaceMutationError: () => assert.fail("must retain the rejected dialog"),
    };
    const code = ts.transpileModule(`let modal = null, restoreModalFocus = null, stopModalProjectionWatch = null;\n${handlers}`, {
      compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
    }).outputText;
    runInNewContext(code, context);
    const app = context as typeof context & { openConfirm(action: string, target: typeof session): void; closeModal(): void };
    app.openConfirm(operation, session);
    confirms!.onConfirm();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(projectionCount, 2, "optimistic hide followed by definitive rollback");
    app.closeModal();
    assert.equal(focusedAction, true, "retained dialog must return to the restored row action");
    assert.equal(railFocused, false, "the rail is only a fallback when the original row is gone");
  });
}

for (const [name, committed, optimisticConfirmed] of [
  ["committed archive releases its restore fence when optimistic outcome is confirmed", true, true],
  ["committed archive releases its restore fence when optimistic outcome is stale", true, false],
  ["event-confirmed archive releases its restore fence after an uncertain response", false, true],
] as const) {
  test(name, async () => {
    const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
    const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
    const names = new Set(["openModal", "closeModal", "openConfirm"]);
    const handlers = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
      .map(node => node.getText(ast)).join("\n");
    let confirms: { onConfirm(): void };
    let releases = 0;
    const session = { id: "session", title: "Session", is_root: false };
    const body = { closest: () => null, getAttribute: () => null, isConnected: true, getClientRects: () => [{}] };
    const card = { focus() {}, isConnected: true, getClientRects: () => [{}], matches: () => false };
    const context = {
      document: { activeElement: body, body }, CSS: { escape: (value: string) => value },
      getComputedStyle: () => ({ visibility: "visible" }), root: { querySelector: () => null },
      modalHost: { replaceChildren() {} }, closeConfigAssistant() {}, focusRail() {}, token: "",
      store: { get: () => ({ sessions: [session] }), subscribe: () => () => {} },
      confirmModal: (options: typeof confirms) => {
        confirms = options;
        return { el: { querySelector: () => card }, close() {}, setBusy() {}, setError() {} };
      },
      isArchived: () => false, isOffBoxWorkspace: () => false,
      optimisticSessions: {
        begin: () => ({}), project: () => [],
        succeed: () => {
          assert.equal(committed, true);
          return optimisticConfirmed;
        },
        reject: () => {
          assert.equal(committed, false);
          return optimisticConfirmed ? "confirmed" : "uncertain";
        },
      },
      pendingRestores: {
        has: () => false,
        captureArchiveSuccess: () => () => { releases++; },
      },
      applySessions() {}, archiveSession: () => Promise.reject(new Error("committed warning")),
      killSession: () => assert.fail("wrong action"), restoreSession: () => assert.fail("wrong action"),
      isMutationCommittedError: () => committed, isMutationOutcomeUncertain: () => !committed,
      requestResync() {}, errorText: (error: Error) => error.message, surfaceMutationError() {},
    };
    const code = ts.transpileModule(`let modal = null, restoreModalFocus = null, stopModalProjectionWatch = null;\n${handlers}`, {
      compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
    }).outputText;
    runInNewContext(code, context);
    const app = context as typeof context & { openConfirm(action: string, target: typeof session, invoker: object): void };
    app.openConfirm("archive", session, { sessionId: session.id, actionLabel: null, header: false });
    confirms!.onConfirm();
    await new Promise(resolve => setImmediate(resolve));
    assert.equal(releases, 1, "the confirmed archive supersedes the restore fence");
  });
}

test("a definitive restore refusal refreshes daemon identity before enabling retry", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const names = new Set(["openModal", "closeModal", "openConfirm"]);
  const handlers = ast.statements.filter(node => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map(node => node.getText(ast)).join("\n");
  const session = { id: "session", title: "Session", is_root: false, backend_type: "local", lifecycle_action: "restore" };
  const body = { closest: () => null, getAttribute: () => null, isConnected: true, getClientRects: () => [{}] };
  const card = { focus() {}, isConnected: true, getClientRects: () => [{}], matches: () => false };
  const busy: boolean[] = [];
  const errors: Array<string | null> = [];
  let finishResync!: () => void;
  const resync = new Promise<void>(resolve => { finishResync = resolve; });
  let resyncs = 0;
  const context = {
    document: { activeElement: body, body }, CSS: { escape: (value: string) => value },
    getComputedStyle: () => ({ visibility: "visible" }), root: { querySelector: () => null },
    modalHost: { replaceChildren() {} }, closeConfigAssistant() {}, focusRail() {}, token: "",
    connectionGeneration: 1, pendingRestoreResync: false,
    store: { get: () => ({ phase: "app", sessions: [session] }), subscribe: () => () => {} },
    confirmModal: () => ({
      el: { querySelector: () => card }, close() {},
      setBusy: (value: boolean) => { busy.push(value); },
      setError: (value: string | null) => { errors.push(value); },
    }),
    restoreRequiresConfirmation: () => false, isActionableSession: () => true,
    isArchived: () => false, isOffBoxWorkspace: () => false,
    optimisticSessions: { begin: () => null },
    pendingRestores: {
      has: () => false, captureArchiveSuccess: () => null,
      run: (_id: string, request: (daemonBootId: string) => Promise<unknown>) => request("daemon-a"),
    },
    restoreSession: (_id: string, _title: string, _token: string, daemonBootId: string) => {
      assert.equal(daemonBootId, "daemon-a");
      return Promise.reject(new Error("stale daemon identity"));
    },
    killSession: () => assert.fail("wrong action"), archiveSession: () => assert.fail("wrong action"),
    isMutationCommittedError: () => false, isMutationOutcomeUncertain: () => false,
    requestResync() {},
    requestPendingRestoreResync: () => { resyncs++; return resync; },
    errorText: (error: Error) => error.message, surfaceMutationError() {},
  };
  const code = ts.transpileModule(`let modal = null, restoreModalFocus = null, stopModalProjectionWatch = null;\n${handlers}`, {
    compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None },
  }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & { openConfirm(action: string, target: typeof session, invoker: object): void };
  app.openConfirm("restore", session, { sessionId: session.id, actionLabel: null, header: false });
  await new Promise(resolve => setImmediate(resolve));
  assert.equal(resyncs, 1, "a refusal must refresh the cached daemon identity");
  assert.deepEqual(busy, [true], "retry stays disabled until that Snapshot finishes");
  finishResync();
  await new Promise(resolve => setImmediate(resolve));
  assert.deepEqual(busy, [true, false]);
  assert.deepEqual(errors, ["stale daemon identity"]);
});

test("reconnect retains a completed restore through its pre-response initial Snapshot", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const connect = ast.statements.find(node => ts.isFunctionDeclaration(node) && node.name?.text === "connect");
  assert.ok(connect && ts.isFunctionDeclaration(connect));

  const rows = [{ id: "session", restoreEligible: true }];
  const pending = new PendingRestores(() => {});
  pending.observe(rows, { kind: "snapshot", generation: pending.beginSnapshot() });
  let finishRestore!: () => void;
  const restore = pending.run("session", () => new Promise<void>(resolve => { finishRestore = resolve; }), true)!;
  let finishTasks!: (tasks: never[]) => void;
  const tasks = new Promise<never[]>(resolve => { finishTasks = resolve; });
  const context = {
    connectionGate: { begin: () => ({ isCurrent: () => true }) },
    pendingRestores: pending,
    store: { set() {}, get: () => ({ selectedId: null }) },
    fetchSessionSnapshot: async () => ({ sessions: rows }),
    shouldForgetToken: () => false, clearToken() {}, describeError: () => "",
    storeToken() {}, listTasks: () => tasks,
    fetchRegisteredProjects: async () => ({ projects: [], error: "" }),
    errorText: () => "", connectionAttemptMayCommit: () => true,
    reconcileProject: () => "", loadProjectChoice: () => null,
    optimisticSessions: { reset() {} }, pickSelection: () => null,
    applySessions: (_sessions: unknown, evidence: Parameters<PendingRestores["observe"]>[1]) => {
      pending.observe(rows, evidence);
    },
    resolveRoute() {}, clearLoginRoute() {}, startStream() {}, requestResync() {},
  };
  const code = ts.transpileModule(
    `let token = null, connectionGeneration = 0, resolvingRoute = false, pendingRestoreResync = false;\n${connect.getText(ast)}`,
    { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } },
  ).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & { connect(candidate: string): Promise<void> };

  const reconnect = app.connect("");
  await new Promise(resolve => setImmediate(resolve));
  finishRestore();
  await restore;
  assert.equal(pending.has("session"), true, "the in-flight reconnect still owns its restore fence");
  finishTasks([]);
  await reconnect;
  assert.equal(pending.has("session"), true,
    "a Snapshot issued before success cannot release the ticket when reconnect commits it later");
});
