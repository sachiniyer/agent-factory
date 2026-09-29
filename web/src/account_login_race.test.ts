import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

// Reproduces the disconnect/reconnect race in doOpenAccountLogin (web/src/index.ts):
// the overlay was mounted inside startAccountLogin's .then with no post-await guard,
// so a disconnect during the RPC left accountLogin null for closeOverlays()'s reaper
// and the stale response mounted an orphan into the detached (reused) modalHost. The
// fix re-validates the connection generation + token before committing — the same gate
// openConfirm's restore branch uses. These tests run the transpiled functions in a VM
// sandbox with a controllable startAccountLogin promise, exactly like overlay_teardown.

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

// A finished/non-pane resolve value, plus one that needs the overlay.
const FINISHED_LOGIN = {
  finished: true,
  logged_in: true,
  notices: [],
  program: "codex login",
  session_name: "",
};
const PANE_LOGIN = {
  finished: false,
  logged_in: false,
  notices: [],
  program: "codex login",
  session_name: "account-login",
};

function stage(): {
  app: {
    doOpenAccountLogin(agent: string, name: string): void;
    disconnect(): void;
    getAccountLogin(): { close(): void } | null;
    simulateConnect(candidate: string): void;
  };
  openAccountLoginCalls: number;
  accountStatusCalls: number;
  refreshAccountsCalls: number;
  resolveStart(login: unknown): void;
  rejectStart(err: unknown): void;
} {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set([
    "closeAccountLogin",
    "closeConfigAssistant",
    "closeModal",
    "closeOverlays",
    "disconnect",
    "doOpenAccountLogin",
    "mountOverlay",
  ]));

  // The controllable AccountLogin RPC: settles only when the test calls
  // resolveStart or rejectStart.
  let resolveStart!: (login: unknown) => void;
  let rejectStart!: (err: unknown) => void;
  const startPromise = new Promise<unknown>((resolve, reject) => {
    resolveStart = resolve;
    rejectStart = reject;
  });

  const counters = { openAccountLoginCalls: 0, accountStatusCalls: 0, refreshAccountsCalls: 0 };

  const context = {
    clearToken() {},
    connectionGate: { invalidate() {} },
    errorText: () => "",
    loginWithoutPaneCopy: () => ({ status: "", detail: "" }),
    modalHost: { replaceChildren() {}, append() {} },
    openAccountLogin: () => {
      counters.openAccountLoginCalls += 1;
      return { close() {} };
    },
    optimisticSessions: { reset() {} },
    pendingRestores: { reset() {} },
    refreshAccounts: () => {
      counters.refreshAccountsCalls += 1;
    },
    setAccountStatus: () => {
      counters.accountStatusCalls += 1;
    },
    startAccountLogin: () => startPromise,
    stopStream() {},
    store: {
      get: () => ({ authRequired: false, phase: "app" }),
      set() {},
    },
  };

  const code = ts.transpileModule(
    `
    let modal = null;
    let configAssistant = null;
    let accountLogin = null;
    let restoreModalFocus = null;
    let stopModalProjectionWatch = null;
    let token = "token";
    let connectionGeneration = 0;
    function getAccountLogin() { return accountLogin; }
    // A minimal mirror of connect()'s commit (token = candidate; connectionGeneration++)
    // so a reconnect can be simulated without transpiling connect's full dependency set.
    function simulateConnect(candidate) { token = candidate; connectionGeneration++; }
    ${handlers}
    `,
    { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } },
  ).outputText;
  runInNewContext(code, context);

  const app = context as typeof context & {
    doOpenAccountLogin(agent: string, name: string): void;
    disconnect(): void;
    getAccountLogin(): { close(): void } | null;
    simulateConnect(candidate: string): void;
  };

  return {
    app: {
      doOpenAccountLogin: app.doOpenAccountLogin.bind(app),
      disconnect: app.disconnect.bind(app),
      getAccountLogin: app.getAccountLogin.bind(app),
      simulateConnect: app.simulateConnect.bind(app),
    },
    get openAccountLoginCalls() {
      return counters.openAccountLoginCalls;
    },
    get accountStatusCalls() {
      return counters.accountStatusCalls;
    },
    get refreshAccountsCalls() {
      return counters.refreshAccountsCalls;
    },
    resolveStart,
    rejectStart,
  };
}

// The fix. A disconnect during the AccountLogin RPC must drop the stale response: no
// orphan overlay controller is installed and openAccountLogin never reaches the host.
// Pre-fix this asserted the OPPOSITE (controller non-null, openAccountLoginCalls === 1)
// to demonstrate the race; the guard now suppresses the post-await commit entirely.
test("disconnect during startAccountLogin suppresses the orphan overlay", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary");
  s.app.disconnect(); // reaper runs while accountLogin is null → no-op
  assert.equal(s.app.getAccountLogin(), null);
  assert.equal(s.openAccountLoginCalls, 0);

  s.resolveStart(PANE_LOGIN);
  await Promise.resolve();

  assert.equal(
    s.app.getAccountLogin(),
    null,
    "no orphan overlay controller installed after disconnect — the stale response is dropped",
  );
  assert.equal(
    s.openAccountLoginCalls,
    0,
    "openAccountLogin never reached the mount host after a disconnect",
  );
});

// Soundness control: without a disconnect the overlay mounts normally through the
// .then, so the harness is not spuriously suppressing the happy path.
test("without a disconnect the account-login overlay mounts normally", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary");
  assert.equal(s.openAccountLoginCalls, 0, "still awaiting the RPC before resolve");

  s.resolveStart(PANE_LOGIN);
  await Promise.resolve();

  assert.notEqual(s.app.getAccountLogin(), null, "the overlay controller was installed");
  assert.equal(s.openAccountLoginCalls, 1, "openAccountLogin reached the mount host once");
  assert.equal(s.accountStatusCalls, 2, "Starting… then Running… status lines both set");
});

// The generation guard (not just a token check) distinguishes a fresh connection from
// the captured one: disconnect and the new connect (even with the SAME credential) both
// bump connectionGeneration, so the stale response is dropped. A token-only guard would
// mistakenly mount here because the reconnected token equals the captured tok.
test("reconnect with the same token before the response still suppresses the orphan", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary"); // captures tok="token", requestGeneration=0
  s.app.disconnect(); // connectionGeneration -> 1, token -> null
  // Re-paste the SAME token the original flow captured (the realistic worst case the
  // report calls out), which a token-only guard would fail to distinguish from the old
  // connection:
  s.app.simulateConnect("token"); // connectionGeneration -> 2, token -> "token"

  s.resolveStart(PANE_LOGIN);
  await Promise.resolve();

  assert.equal(
    s.app.getAccountLogin(),
    null,
    "a same-token reconnect is still a new connection generation, so the stale response is dropped",
  );
  assert.equal(s.openAccountLoginCalls, 0);
});

// The guard is at the TOP of the .then, so the post-await row-only path is guarded too:
// a finished login that arrives after a disconnect must not commit a stale status line.
test("a finished login arriving after disconnect is dropped (row-only path guarded)", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary"); // pre-await "Starting…" status: accountStatusCalls=1
  s.app.disconnect();
  assert.equal(s.accountStatusCalls, 1);

  s.resolveStart(FINISHED_LOGIN);
  await Promise.resolve();

  assert.equal(s.openAccountLoginCalls, 0, "no overlay for a finished login");
  assert.equal(s.app.getAccountLogin(), null);
  assert.equal(
    s.accountStatusCalls,
    1,
    "only the pre-await Starting… status was set; the stale finished response wrote nothing more",
  );
  assert.equal(s.refreshAccountsCalls, 0, "the stale response did not trigger an accounts refresh");
});

// The complementary await-exit takes the same gate: a rejection that lands after
// a disconnect must not write the dead connection's error status onto the row.
test("a rejection landing after disconnect is dropped (catch path guarded)", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary"); // pre-await "Starting…" status: accountStatusCalls=1
  s.app.disconnect();
  assert.equal(s.accountStatusCalls, 1);

  s.rejectStart(new Error("connection reset"));
  // Two microtask hops: the rejection propagates through the .then link first,
  // then the .catch handler runs — one tick alone could pass vacuously.
  await Promise.resolve();
  await Promise.resolve();

  assert.equal(
    s.accountStatusCalls,
    1,
    "the stale rejection wrote no error status — the .catch takes the same generation+token gate",
  );
});

// Soundness control for the guard above: on the same connection a rejection
// still reaches setAccountStatus, so the gate is not suppressing live errors.
test("without a disconnect a rejection writes the error status", async () => {
  const s = stage();

  s.app.doOpenAccountLogin("codex", "primary");
  s.rejectStart(new Error("flow refused"));
  await Promise.resolve();
  await Promise.resolve();

  assert.equal(s.accountStatusCalls, 2, "Starting… then the error status line both set");
});
