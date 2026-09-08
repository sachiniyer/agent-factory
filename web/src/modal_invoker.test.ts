import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

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
