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

test("a committed account handoff closes the stale modal and surfaces a confirmed warning", async () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doHandoff");
  let submit: ((to: string, account?: string) => void) | undefined;
  const events: string[] = [];
  const modalHandle = {
    close: () => events.push("close"),
    setBusy: (busy: boolean) => events.push(`busy:${busy}`),
    setError: (message: string) => events.push(`error:${message}`),
  };
  const context = {
    selectedSessionData: () => ({
      id: "session-id", title: "worker", current_agent: "claude", account: "work",
      worktree: { repo_path: "/work/repo" },
    }),
    canHandoff: () => true,
    canHandoffAccount: () => true,
    handoffModal: (_title: string, _agent: string, callbacks: { onSubmit(to: string, account?: string): void }) => {
      submit = callbacks.onSubmit;
      return modalHandle;
    },
    loadPrograms: () => Promise.resolve({}),
    loadCreateAccounts: () => Promise.resolve({}),
    handoffSession: () => Promise.reject(new Error("handoff committed; settlement pending")),
    isMutationCommittedError: () => true,
    requestResync: () => events.push("resync"),
    surfaceMutationError: (_error: unknown, kind: string) => events.push(`surface:${kind}`),
    errorText: (error: Error) => error.message,
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${handler}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & { doHandoff(): void };
  app.doHandoff();
  assert.ok(submit);
  submit("codex", "personal");
  await new Promise(resolve => setImmediate(resolve));

  assert.deepEqual(events, ["busy:true", "close", "resync", "surface:confirmed"]);
});

// The reserved root's account move is the one handoff a session can take when
// the agent axis is closed (#4433): the modal must run account-only exactly
// when can_handoff is absent but can_handoff_account answers yes — and offer
// nothing at all when both are absent, on an older daemon or an ineligible row.
test("doHandoff runs the modal account-only when only the account gate passes", () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = topLevelFunction(source, "doHandoff");

  for (const [agent, account, modalOpts] of [
    // Ordinary row: both gates pass → full agent picker.
    [true, true, false],
    // Reserved root: agent axis refused, account axis open → account-only.
    [false, true, true],
  ] as const) {
    let seenAccountOnly: boolean | undefined;
    const context = {
      selectedSessionData: () => ({
        id: "id", title: "root", current_agent: "codex", account: "work",
        worktree: { repo_path: "/work/repo" },
      }),
      canHandoff: () => agent,
      canHandoffAccount: () => account,
      handoffModal: (_t: string, _a: string, callbacks: { accountOnly?: boolean }) => {
        seenAccountOnly = callbacks.accountOnly;
        return { close() {}, setBusy() {}, setError() {} };
      },
      loadPrograms: () => Promise.resolve({}),
      loadCreateAccounts: () => Promise.resolve({}),
    };
    const code = ts.transpileModule(`
      let token = "token", modal = null;
      function openModal(next) { modal = next; }
      function closeModal() { if (modal) modal.close(); modal = null; }
      ${handler}
    `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
    runInNewContext(code, context);
    (context as typeof context & { doHandoff(): void }).doHandoff();
    assert.equal(seenAccountOnly, modalOpts,
      `can_handoff=${agent} can_handoff_account=${account} → accountOnly must be ${modalOpts}`);
  }

  // Neither gate → the modal is never built.
  let built = false;
  const context = {
    selectedSessionData: () => ({ id: "id", title: "s", current_agent: "codex", worktree: { repo_path: "/r" } }),
    canHandoff: () => false,
    canHandoffAccount: () => false,
    handoffModal: () => { built = true; return { close() {}, setBusy() {}, setError() {} }; },
    loadPrograms: () => Promise.resolve({}),
    loadCreateAccounts: () => Promise.resolve({}),
  };
  const code = ts.transpileModule(`
    let token = "token", modal = null;
    function openModal(next) { modal = next; }
    function closeModal() { if (modal) modal.close(); modal = null; }
    ${topLevelFunction(source, "doHandoff")}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);
  (context as typeof context & { doHandoff(): void }).doHandoff();
  assert.equal(built, false, "a session with neither capability must not open the modal");
});
