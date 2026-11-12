import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

// Exercise the real openConfirm handler from index.ts in a vm sandbox (the same
// technique modal_invoker.test.ts and revert_remount_guard.test.ts use) to pin
// the dismissal fallback for a confirmation-required restore refusal.
//
// A confirmation-required (sandbox/unknown-backend) restore keeps the confirm
// dialog mounted and busy for the whole restore RPC. The UI allows dismissing it
// while busy (Escape via the global key handler, or a backdrop click), which
// detaches the dialog element. When the daemon later returns a definitive
// refusal — after pendingRestores.waitForRefusalResync resolves — showRefusal
// must route the error to a persistent surface (surfaceMutationError) instead of
// writing it into the detached handle. Every sibling mutation flow already does
// this (rebind, create, add-project, kill/archive revert guards, and the
// immediate/local restore path); the confirmation-required restore path was the
// outlier that swallowed the refusal.

function extractHandlers(names: Set<string>): string {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter(node => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map(node => node.getText(ast))
    .join("\n");
}

const NAMES = new Set(["openModal", "mountOverlay", "closeModal", "closeOverlays", "openConfirm"]);

const REFUSAL = "stale daemon identity";

interface Harness {
  app: { openConfirm(action: string, session: object, invoker: object): void; closeModal(): void };
  session: object;
  confirm: () => { onConfirm(): void } | undefined;
  busy: boolean[];
  errors: Array<string | null>;
  surfaced: string[];
  finishResync: () => void;
  resyncs: () => number;
  replaceConnection: () => void;
}

function buildHarness(requiresConfirmation: boolean): Harness {
  const session = { id: "session", title: "Session", is_root: false, backend_type: "local", lifecycle_action: "restore" };
  const body = { closest: () => null, getAttribute: () => null, isConnected: true, getClientRects: () => [{}] };
  const card = { focus() {}, isConnected: true, getClientRects: () => [{}], matches: () => false };
  const busy: boolean[] = [];
  const errors: Array<string | null> = [];
  const surfaced: string[] = [];
  let finishResync!: () => void;
  const resync = new Promise<void>(resolve => { finishResync = resolve; });
  let resyncCount = 0;
  let confirms: { onConfirm(): void } | undefined;
  let generation = 1;
  const context = {
    document: { activeElement: body, body }, CSS: { escape: (value: string) => value },
    getComputedStyle: () => ({ visibility: "visible" }), root: { querySelector: () => null },
    modalHost: { replaceChildren() {} }, closeConfigAssistant() {}, closeAccountLogin() {}, focusRail() {}, token: "",
    connectionGeneration: generation, pendingRestoreResync: false,
    store: { get: () => ({ phase: "app", sessions: [session] }), subscribe: () => () => {} },
    confirmModal: (options: { onConfirm(): void }) => {
      confirms = options;
      return {
        el: { querySelector: () => card },
        close() {},
        setBusy: (value: boolean) => { busy.push(value); },
        setError: (value: string | null) => { errors.push(value); },
      };
    },
    restoreRequiresConfirmation: () => requiresConfirmation,
    isActionableSession: () => true,
    isArchived: () => false, isOffBoxWorkspace: () => false,
    optimisticSessions: { begin: () => null },
    pendingRestores: {
      has: () => false, captureArchiveSuccess: () => null,
      waitForRefusalResync: () => { resyncCount++; return resync; },
      run: (_id: string, request: (daemonBootId: string) => Promise<unknown>) => request("daemon-a"),
    },
    restoreSession: (_id: string, _title: string, _token: string, daemonBootId: string) => {
      assert.equal(daemonBootId, "daemon-a");
      return Promise.reject(new Error(REFUSAL));
    },
    killSession: () => assert.fail("wrong action"), archiveSession: () => assert.fail("wrong action"),
    isMutationCommittedError: () => false, isMutationOutcomeUncertain: () => false,
    requestResync() {},
    requestPendingRestoreResync: () => assert.fail("the refusal ticket owns reconciliation"),
    errorText: (error: Error) => error.message,
    surfaceMutationError: (error: unknown) => { surfaced.push((error as Error).message); },
  };
  const code = ts.transpileModule(
    `let modal = null, restoreModalFocus = null, stopModalProjectionWatch = null, modalGeneration = 0;\nlet configAssistant = null, accountLogin = null;\n${extractHandlers(NAMES)}`,
    { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } },
  ).outputText;
  runInNewContext(code, context);
  const app = context as typeof context & { openConfirm(action: string, session: object, invoker: object): void; closeModal(): void };
  return {
    app,
    session,
    confirm: () => confirms,
    busy,
    errors,
    surfaced,
    finishResync,
    resyncs: () => resyncCount,
    replaceConnection: () => { context.connectionGeneration = ++generation; },
  };
}

const settle = () => new Promise(resolve => setImmediate(resolve));

const INVOKER = { sessionId: "session", actionLabel: null, header: false };

// --- Control: dialog left open, refusal lands inline --------------------------

test("confirmation-required restore refusal surfaces inline when the dialog stays open", async () => {
  const h = buildHarness(true);
  h.app.openConfirm("restore", h.session, INVOKER);
  await settle();
  // Confirmation required: the dialog stays mounted, onConfirm not auto-called.
  h.confirm()!.onConfirm();
  await settle();
  assert.equal(h.resyncs(), 1, "a refusal must refresh the cached daemon identity");
  assert.deepEqual(h.busy, [true], "the dialog stays busy until the resync finishes");
  // Dialog still open — resolve the resync and the refusal lands inline.
  h.finishResync();
  await settle();
  assert.deepEqual(h.busy, [true, false], "busy clears after the resync");
  assert.deepEqual(h.errors, [REFUSAL], "the refusal reaches the inline error surface");
  assert.deepEqual(h.surfaced, [], "no persistent notice when the dialog is still mounted");
});

// --- Bug case: dialog dismissed mid-flight, refusal must surface --------------

test("confirmation-required restore refusal surfaces persistently when the dialog is dismissed mid-flight", async () => {
  const h = buildHarness(true);
  h.app.openConfirm("restore", h.session, INVOKER);
  await settle();
  h.confirm()!.onConfirm();
  await settle();
  assert.equal(h.resyncs(), 1, "a refusal must refresh the cached daemon identity");
  // Escape-dismiss (or backdrop-click) the busy dialog while the resync is pending.
  h.app.closeModal();
  h.finishResync();
  await settle();
  assert.deepEqual(h.busy, [true, false], "busy clears on the (detached) handle");
  assert.deepEqual(h.errors, [REFUSAL], "setError still runs on the detached handle");
  assert.deepEqual(h.surfaced, [REFUSAL],
    "the refusal must reach a persistent surface when the dialog was dismissed");
});

// --- Regression: immediate restore dismissal fallback still works -------------

test("immediate restore refusal still surfaces persistently when dismissed (regression)", async () => {
  const h = buildHarness(false);
  h.app.openConfirm("restore", h.session, INVOKER);
  // immediateRestore auto-confirms, so onConfirm is called by mountWith.
  await settle();
  assert.equal(h.resyncs(), 1, "a refusal must refresh the cached daemon identity");
  // Dismiss the progress dialog while the resync is still pending.
  h.app.closeModal();
  h.finishResync();
  await settle();
  assert.deepEqual(h.surfaced, [REFUSAL],
    "the immediate path's dismissal fallback must still work after the fix");
});

// --- Guard: a replaced connection suppresses the old connection's refusal ----

test("a replaced connection suppresses the dismissed confirmation-required restore refusal", async () => {
  const h = buildHarness(true);
  h.app.openConfirm("restore", h.session, INVOKER);
  await settle();
  h.confirm()!.onConfirm();
  await settle();
  assert.equal(h.resyncs(), 1, "a refusal must refresh the cached daemon identity");
  h.app.closeModal();
  h.replaceConnection();
  h.finishResync();
  await settle();
  assert.deepEqual(h.errors, [], "the old connection's refusal must not publish into the new one");
  assert.deepEqual(h.surfaced, [], "the old connection's refusal must not surface after reconnect");
});
