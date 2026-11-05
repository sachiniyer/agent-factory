import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Mirrors add_project_flow.test.ts and modal_invoker.test.ts: extracts the real
// newSession and openConfirm handlers from index.ts and runs them in a sandbox,
// so the test exercises the shipped revert paths rather than copies of their logic.
// The add-project flow already guards its refusal-path remount against a live
// config-assistant or account-login overlay (add_project_flow.test.ts pins it);
// this file pins the same guard for the sibling create-reverted and kill/archive
// revert paths, which close their modal before awaiting the RPC and so leave the
// overlay slot reoccupiable by the time a definitive refusal arrives.

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  return node.getText(ast).replace(/^export\s+/, "");
}

/** An error the stubbed api classifies: "refused" is a definitive daemon refusal
 *  (daemon_rejected envelope), "uncertain" a lost reply / transport failure. */
class StubError extends Error {
  constructor(message: string, readonly kind: "refused" | "uncertain") { super(message); }
}

interface Deferred<T> { resolve(v: T): void; reject(e: Error): void }

const settle = () => new Promise(resolve => setImmediate(resolve));

// --- reverted create: newSession's create-reverted catch ----------------------
//
// newSession closes its modal before awaiting createSession's RPC, so `modal` is
// null for the whole request. If the user opens the config assistant or account
// login overlay during that window and the create is then definitively refused,
// the reverted path must NOT remount the dialog (which would reap the live overlay
// via openModal → mountOverlay → closeOverlays). Instead it falls back to
// surfaceMutationError, matching the add-project flow's guard.

function createHarness() {
  const indexSource = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const events: string[] = [];
  const submits: Array<(values: { title: string; repoPath: string; program: string; prompt: string; account?: string }) => void> = [];
  const pending: Array<Deferred<unknown>> = [];
  const context = {
    store: {
      get: () => ({ sessions: [], tasks: [], registeredProjects: [], selectedProject: null, view: "sessions" }),
      set: () => undefined,
    },
    pickerProjects: () => [] as string[],
    projectRoots: () => [] as string[],
    newSessionModal: (_projects: string[], _defaultProject: string | null, callbacks: { onSubmit: (v: unknown) => void }) => {
      submits.push(callbacks.onSubmit as typeof submits[number]);
      const n = submits.length;
      return {
        el: { querySelector: () => null },
        close: () => events.push(`close:${n}`),
        setBusy: (busy: boolean) => events.push(`busy:${n}:${busy}`),
        setError: (msg: string) => events.push(`error:${n}:${msg}`),
      };
    },
    listBackends: () => Promise.resolve({}),
    loadPrograms: () => Promise.resolve({}),
    loadCreateAccounts: () => Promise.resolve({}),
    suggestSessionName: () => Promise.resolve(""),
    createSession: () => new Promise<unknown>((resolve, reject) => pending.push({ resolve, reject })),
    captureModalInvoker: () => ({ actionLabel: null, header: false }),
    createSelectionIntent: { submit: () => ({}), isCurrent: () => true },
    optimisticSessions: { beginCreate: () => ({}), project: () => [], reject: () => "reverted" as const },
    applySessions: () => undefined,
    requestResync: () => undefined,
    accountSkewMessage: () => "",
    surfaceTabError: () => undefined,
    clearTabError: () => undefined,
    isMutationOutcomeUncertain: (e: StubError) => e.kind !== "refused",
    errorText: (e: Error) => e.message,
    surfaceMutationError: (e: Error, kind = "failed") => events.push(`outcome:${kind}:${e.message}`),
    // Non-modal overlay owners — the remount guard must not reap them. Context
    // properties (not prelude lets) so a test can open one mid-flight.
    configAssistant: null as { close(): void } | null,
    accountLogin: null as { close(): void } | null,
    recordEvent: (msg: string) => events.push(msg),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { recordEvent("openModal"); modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(indexSource, "newSession")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & { newSession(): void };
  return { app, context, events, submits, pending };
}

const createValues = { title: "s", repoPath: "/repo", program: "", prompt: "" };

test("a reverted create remounts the form when no overlay is live", async () => {
  const { app, events, submits, pending } = createHarness();
  app.newSession();
  submits[0](createValues);
  pending[0].reject(new StubError("create refused: bad repo", "refused"));
  await settle();
  assert.equal(events.filter(e => e === "openModal").length, 2,
    "the initial open and the reverted remount both mount");
  assert.ok(!events.some(e => e.startsWith("outcome:")), "a reverted refusal with no overlay is not an outcome notice");
});

test("a reverted create does not remount over a live config-assistant overlay", async () => {
  const { app, context, events, submits, pending } = createHarness();
  app.newSession();
  submits[0](createValues);
  context.configAssistant = { close: () => undefined };
  pending[0].reject(new StubError("create refused: bad repo", "refused"));
  await settle();
  assert.equal(events.filter(e => e === "openModal").length, 1,
    "a live overlay is never reaped for the remount — only the initial open may mount");
  assert.ok(events.some(e => e.startsWith("outcome:failed:")),
    "the refusal lands as an outcome notice instead of the inline remount");
});

test("a reverted create does not remount over a live account-login overlay", async () => {
  const { app, context, events, submits, pending } = createHarness();
  app.newSession();
  submits[0](createValues);
  context.accountLogin = { close: () => undefined };
  pending[0].reject(new StubError("create refused: bad repo", "refused"));
  await settle();
  assert.equal(events.filter(e => e === "openModal").length, 1,
    "a live overlay is never reaped for the remount — only the initial open may mount");
  assert.ok(events.some(e => e.startsWith("outcome:failed:")),
    "the refusal lands as an outcome notice instead of the inline remount");
});

// --- reverted kill/archive: openConfirm's reverted catch ---------------------
//
// openConfirm closes its confirm modal before awaiting the kill/archive RPC, so
// `modal` is null for the whole request. Same guard, same fallback as the create
// and add-project flows.

function confirmHarness(action: "kill" | "archive") {
  const indexSource = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const events: string[] = [];
  const confirms: Array<{ onConfirm(): void }> = [];
  const pending: Array<Deferred<void>> = [];
  const session = { id: "session", title: "Session", is_root: false };
  const context = {
    captureModalInvoker: () => ({ actionLabel: null, header: false }),
    confirmModal: (opts: { onConfirm(): void }) => {
      confirms.push(opts);
      return { el: { querySelector: () => null }, close: () => undefined, setBusy: () => undefined, setError: () => undefined };
    },
    store: { get: () => ({ sessions: [session] }), subscribe: () => () => undefined },
    isArchived: () => false, isOffBoxWorkspace: () => false,
    optimisticSessions: { begin: () => ({}), project: () => [], reject: () => "reverted" as const },
    pendingRestores: { has: () => false, captureArchiveSuccess: () => null, run: () => undefined, waitForRefusalResync: () => Promise.resolve() },
    applySessions: () => undefined, requestResync: () => undefined,
    killSession: () => new Promise<void>((resolve, reject) => pending.push({ resolve, reject })),
    archiveSession: () => new Promise<void>((resolve, reject) => pending.push({ resolve, reject })),
    restoreSession: () => Promise.reject(new Error("wrong action")),
    isMutationCommittedError: () => false, isMutationOutcomeUncertain: (e: StubError) => e.kind !== "refused",
    errorText: (e: Error) => e.message,
    surfaceMutationError: (e: Error, kind = "failed") => events.push(`outcome:${kind}:${e.message}`),
    createSelectionIntent: { snapshot: () => 0, isCurrent: () => true },
    fetchKeptBranch: () => Promise.resolve({}),
    connectionGeneration: 1,
    restoreRequiresConfirmation: () => true,
    isActionableSession: () => false,
    configAssistant: null as { close(): void } | null,
    accountLogin: null as { close(): void } | null,
    recordEvent: (msg: string) => events.push(msg),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null, modalGeneration = 0, stopModalProjectionWatch = null, pendingRestoreResync = false;
    function openModal(next) { recordEvent("openModal"); modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(indexSource, "openConfirm")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & { openConfirm(action: string, session: { id: string; title: string; is_root: boolean }, invoker: unknown): void };
  return { app, context, events, confirms, pending, session };
}

for (const action of ["kill", "archive"] as const) {
  test(`a reverted ${action} remounts the confirm dialog when no overlay is live`, async () => {
    const { app, events, confirms, pending, session } = confirmHarness(action);
    app.openConfirm(action, session, { sessionId: session.id, actionLabel: null, header: false });
    await settle();
    assert.ok(confirms.length > 0, "the confirm modal mounted");
    confirms[0].onConfirm();
    pending[0].reject(new StubError(`${action} refused`, "refused"));
    await settle();
    assert.equal(events.filter(e => e === "openModal").length, 2,
      "the initial open and the reverted remount both mount");
    assert.ok(!events.some(e => e.startsWith("outcome:")), "a reverted refusal with no overlay is not an outcome notice");
  });

  test(`a reverted ${action} does not remount over a live config-assistant overlay`, async () => {
    const { app, context, events, confirms, pending, session } = confirmHarness(action);
    app.openConfirm(action, session, { sessionId: session.id, actionLabel: null, header: false });
    await settle();
    assert.ok(confirms.length > 0, "the confirm modal mounted");
    confirms[0].onConfirm();
    context.configAssistant = { close: () => undefined };
    pending[0].reject(new StubError(`${action} refused`, "refused"));
    await settle();
    assert.equal(events.filter(e => e === "openModal").length, 1,
      "a live overlay is never reaped for the remount — only the initial open may mount");
    assert.ok(events.some(e => e.startsWith("outcome:failed:")),
      "the refusal lands as an outcome notice instead of the inline remount");
  });

  test(`a reverted ${action} does not remount over a live account-login overlay`, async () => {
    const { app, context, events, confirms, pending, session } = confirmHarness(action);
    app.openConfirm(action, session, { sessionId: session.id, actionLabel: null, header: false });
    await settle();
    assert.ok(confirms.length > 0, "the confirm modal mounted");
    confirms[0].onConfirm();
    context.accountLogin = { close: () => undefined };
    pending[0].reject(new StubError(`${action} refused`, "refused"));
    await settle();
    assert.equal(events.filter(e => e === "openModal").length, 1,
      "a live overlay is never reaped for the remount — only the initial open may mount");
    assert.ok(events.some(e => e.startsWith("outcome:failed:")),
      "the refusal lands as an outcome notice instead of the inline remount");
  });
}
