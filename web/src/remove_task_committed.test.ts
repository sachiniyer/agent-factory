import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import ts from "typescript";

function topLevelFunction(source: string, name: string): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const node = ast.statements.find(statement =>
    ts.isFunctionDeclaration(statement) && statement.name?.text === name);
  assert.ok(node, `index.ts must declare ${name}`);
  return node.getText(ast);
}

test("a committed task removal closes the modal, refreshes, and surfaces a warning", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doRemoveTask");
  let confirm: (() => void) | undefined;
  const events: string[] = [];
  const modalHandle = {
    close: () => events.push("close"),
    setBusy: (busy: boolean) => events.push(`busy:${busy}`),
    setError: (message: string) => events.push(`error:${message}`),
  };
  const context = {
    removeTaskModal: (
      _name: string,
      onConfirm: () => void,
      _onCancel: () => void,
    ) => {
      confirm = onConfirm;
      return modalHandle;
    },
    removeTask: () =>
      Promise.reject(
        new Error(
          "task removal committed, but failed to reload task schedules: watcher shutting down",
        ),
      ),
    isMutationCommittedError: () => true,
    refreshTasks: () => events.push("refresh"),
    surfaceTabError: () => events.push("surface"),
    errorText: (error: Error) => error.message,
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${handler}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & { doRemoveTask(task: unknown): void };
  app.doRemoveTask({ id: "task-id", name: "task-name" });
  assert.ok(confirm, "doRemoveTask must open the confirmation modal");
  confirm();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(events, ["busy:true", "close", "refresh", "surface"]);
});

test("a non-committed task removal keeps the modal open with a retryable error", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doRemoveTask");
  let confirm: (() => void) | undefined;
  const events: string[] = [];
  const modalHandle = {
    close: () => events.push("close"),
    setBusy: (busy: boolean) => events.push(`busy:${busy}`),
    setError: (message: string) => events.push(`error:${message}`),
  };
  const context = {
    removeTaskModal: (
      _name: string,
      onConfirm: () => void,
      _onCancel: () => void,
    ) => {
      confirm = onConfirm;
      return modalHandle;
    },
    removeTask: () => Promise.reject(new Error("failed to remove task: not found")),
    isMutationCommittedError: () => false,
    refreshTasks: () => events.push("refresh"),
    surfaceTabError: () => events.push("surface"),
    errorText: (error: Error) => error.message,
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${handler}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & { doRemoveTask(task: unknown): void };
  app.doRemoveTask({ id: "task-id", name: "task-name" });
  assert.ok(confirm);
  confirm();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(events, ["busy:true", "busy:false", "error:failed to remove task: not found"]);
});

test("a successful task removal closes the modal and refreshes", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doRemoveTask");
  let confirm: (() => void) | undefined;
  const events: string[] = [];
  const modalHandle = {
    close: () => events.push("close"),
    setBusy: (busy: boolean) => events.push(`busy:${busy}`),
    setError: (message: string) => events.push(`error:${message}`),
  };
  const context = {
    removeTaskModal: (
      _name: string,
      onConfirm: () => void,
      _onCancel: () => void,
    ) => {
      confirm = onConfirm;
      return modalHandle;
    },
    removeTask: () => Promise.resolve(),
    isMutationCommittedError: () => true,
    refreshTasks: () => events.push("refresh"),
    surfaceTabError: () => events.push("surface"),
    errorText: (error: Error) => error.message,
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${handler}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & { doRemoveTask(task: unknown): void };
  app.doRemoveTask({ id: "task-id", name: "task-name" });
  assert.ok(confirm);
  confirm();
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(events, ["busy:true", "close", "refresh"]);
});
