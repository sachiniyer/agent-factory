import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

function topLevelIdentifierUsers(source: string, identifier: string): string[] {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  const uses = (node: ts.Node): boolean => {
    if (ts.isIdentifier(node) && node.text === identifier) return true;
    let found = false;
    node.forEachChild((child) => {
      if (!found && uses(child)) found = true;
    });
    return found;
  };
  return ast.statements
    .filter(ts.isFunctionDeclaration)
    .filter(uses)
    .map((node) => node.name?.text ?? "")
    .sort();
}

class MockWebSocket {
  static readonly OPEN = 1;
  static readonly CLOSED = 3;
  readyState = MockWebSocket.OPEN;

  close(): void {
    this.readyState = MockWebSocket.CLOSED;
  }
}

function stage(): {
  app: {
    disconnect(): void;
    installAccountLogin(controller: { close(): void }): void;
    installConfigAssistant(controller: { close(): void }): void;
    doOpenAccountLogin(agent: string, name: string): void;
    doOpenConfigAssistant(): void;
    openModal(modal: { el: { querySelector(): null }; close(): void }): void;
    rerender(): void;
  };
  accountSocket: MockWebSocket;
  assistantSocket: MockWebSocket;
} {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set([
    "captureModalInvoker",
    "closeAccountLogin",
    "closeConfigAssistant",
    "closeModal",
    "closeOverlays",
    "disconnect",
    "doOpenAccountLogin",
    "doOpenConfigAssistant",
    "mountOverlay",
    "openModal",
    "rerender",
  ]));
  const context = {
    CSS: { escape: (value: string) => value },
    clearToken() {},
    connectionGate: { invalidate() {} },
    document: { activeElement: null, body: {} },
    focusRail() {},
    getComputedStyle: () => ({ visibility: "visible" }),
    modalHost: { replaceChildren() {} },
    openAccountLogin: () => ({ close() {} }),
    openConfigAssistant: () => ({ close() {} }),
    optimisticSessions: { reset() {} },
    pendingRestores: { reset() {} },
    refreshAccounts() {},
    root: {},
    actions: {},
    disposeSplit() {},
    renderLogin() {},
    stashLoginRoute() {},
    startAccountLogin: async () => ({
      finished: false,
      logged_in: false,
      notices: [],
      program: "codex login",
      session_name: "account-login",
    }),
    stopStream() {},
    setAccountStatus() {},
    store: {
      get: () => ({ authRequired: false, phase: "login" }),
      set() {},
    },
  };
  const code = ts.transpileModule(`
    let modal = null;
    let configAssistant = null;
    let accountLogin = null;
    let restoreModalFocus = null;
    let stopModalProjectionWatch = null;
    let shell = null;
    let token = "token";
    let connectionGeneration = 0;
    function installAccountLogin(controller) { accountLogin = controller; }
    function installConfigAssistant(controller) { configAssistant = controller; }
    ${handlers}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & {
    disconnect(): void;
    installAccountLogin(controller: { close(): void }): void;
    installConfigAssistant(controller: { close(): void }): void;
    doOpenAccountLogin(agent: string, name: string): void;
    doOpenConfigAssistant(): void;
    openModal(modal: { el: { querySelector(): null }; close(): void }): void;
    rerender(): void;
  };
  const accountSocket = new MockWebSocket();
  const assistantSocket = new MockWebSocket();
  app.installAccountLogin({ close: () => accountSocket.close() });
  app.installConfigAssistant({ close: () => assistantSocket.close() });
  return { app, accountSocket, assistantSocket };
}

test("disconnect closes the account-login WebSocket", () => {
  const { app, accountSocket } = stage();

  app.disconnect();

  assert.equal(accountSocket.readyState, MockWebSocket.CLOSED);
});

test("opening a modal closes the account-login WebSocket", () => {
  const { app, accountSocket } = stage();

  app.openModal({ el: { querySelector: () => null }, close() {} });

  assert.equal(accountSocket.readyState, MockWebSocket.CLOSED);
});

test("rendering the login phase closes the account-login WebSocket", () => {
  const { app, accountSocket } = stage();

  app.rerender();

  assert.equal(accountSocket.readyState, MockWebSocket.CLOSED);
});

test("opening the config assistant closes the account-login WebSocket", () => {
  const { app, accountSocket } = stage();

  app.doOpenConfigAssistant();

  assert.equal(accountSocket.readyState, MockWebSocket.CLOSED);
});

test("opening account login closes the config-assistant WebSocket", async () => {
  const { app, assistantSocket } = stage();

  app.doOpenAccountLogin("codex", "primary");
  await Promise.resolve();

  assert.equal(assistantSocket.readyState, MockWebSocket.CLOSED);
});

test("overlay openers cannot access modalHost outside the teardown seam", () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");

  assert.deepEqual(
    topLevelIdentifierUsers(source, "modalHost"),
    ["mountOverlay", "rerender"],
    "rerender may attach the host to AppShell; every controller opener must use mountOverlay",
  );
});
