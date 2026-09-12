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
    openModal(modal: { el: { querySelector(): null }; close(): void }): void;
    rerender(): void;
  };
  socket: MockWebSocket;
} {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set([
    "captureModalInvoker",
    "closeAccountLogin",
    "closeConfigAssistant",
    "closeModal",
    "closeOverlays",
    "disconnect",
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
    optimisticSessions: { reset() {} },
    pendingRestores: { reset() {} },
    root: {},
    actions: {},
    disposeSplit() {},
    renderLogin() {},
    stashLoginRoute() {},
    stopStream() {},
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
    ${handlers}
  `, { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } }).outputText;
  runInNewContext(code, context);

  const socket = new MockWebSocket();
  const app = context as typeof context & {
    disconnect(): void;
    installAccountLogin(controller: { close(): void }): void;
    openModal(modal: { el: { querySelector(): null }; close(): void }): void;
    rerender(): void;
  };
  app.installAccountLogin({ close: () => socket.close() });
  return { app, socket };
}

test("disconnect closes the account-login WebSocket", () => {
  const { app, socket } = stage();

  app.disconnect();

  assert.equal(socket.readyState, MockWebSocket.CLOSED);
});

test("opening a modal closes the account-login WebSocket", () => {
  const { app, socket } = stage();

  app.openModal({ el: { querySelector: () => null }, close() {} });

  assert.equal(socket.readyState, MockWebSocket.CLOSED);
});

test("rendering the login phase closes the account-login WebSocket", () => {
  const { app, socket } = stage();

  app.rerender();

  assert.equal(socket.readyState, MockWebSocket.CLOSED);
});
