// The web client's account contracts (#3385). Pure logic + a stubbed fetch,
// matching the rest of web/src/*.test.ts — the rendered section is proven in the
// Playwright selftest, and the TUI's equivalent rows are proven in
// ui/config_pane_accounts_test.go.
//
// What is worth pinning here is the wire: WHAT this client sends and what it does
// with the answer. The standing constraint of the accounts epic (#3388) is that no
// credential material crosses af, and certainly not the web transport — so one of
// these tests is simply that no request body from this surface carries anything
// but an agent and a name.

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { register } from "node:module";

import { listAccounts, registerAccount, startAccountLogin } from "./api.js";
import { emptyAccountsState } from "./accounts.js";
import type { AccountLoginResponse } from "./types.js";

// account_login_overlay.ts → terminal.ts → xterm's stylesheet + UMD bundle,
// neither of which plain node can load. Stub them (config_assistant.test.ts
// precedent), then import dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { loginTerminalStatusCopy, loginWithoutPaneCopy } = (await import("./account_login_overlay.js")) as {
  loginTerminalStatusCopy: (status: string, login: AccountLoginResponse) => string;
  loginWithoutPaneCopy: (login: AccountLoginResponse) => { status: string; detail: string };
};

interface Captured {
  url: string;
  body: Record<string, unknown>;
  calls: number;
}

function stubFetch(data: unknown, opts: { ok?: boolean; status?: number; error?: string } = {}): Captured {
  const cap: Captured = { url: "", body: {}, calls: 0 };
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    cap.calls += 1;
    cap.url = url;
    cap.body = JSON.parse(String(init.body));
    return {
      ok: opts.ok ?? true,
      status: opts.status ?? 200,
      statusText: "OK",
      json: async () => ({
        data: opts.error === undefined ? data : null,
        error: opts.error === undefined ? null : { message: opts.error },
      }),
    } as unknown as Response;
  };
  return cap;
}

afterEach(() => {
  delete (globalThis as { fetch?: unknown }).fetch;
});

function loginResponse(over: Partial<AccountLoginResponse> = {}): AccountLoginResponse {
  return {
    agent: "codex",
    name: "work",
    dir: "/home/u/.agent-factory/accounts/codex/work",
    program: "codex login",
    session_name: "af_af-login-codex-work",
    socket_path: "/tmp/tmux-1000/default",
    reused: false,
    finished: false,
    logged_in: false,
    ...over,
  };
}

test("listAccounts: reads the daemon's accounts and roster", async () => {
  const cap = stubFetch({
    entries: [
      { agent: "codex", name: "work", dir: "/d/codex/work", registration_only: false, logged_in: true },
    ],
    agents: ["claude", "codex", "gemini"],
  });
  const resp = await listAccounts("T0KEN");
  assert.equal(cap.url, "/v1/ListAccounts");
  assert.equal(resp.entries.length, 1);
  assert.equal(resp.entries[0]?.logged_in, true);
  assert.deepEqual(resp.agents, ["claude", "codex", "gemini"]);
});

test("listAccounts: an older daemon that omits the lists yields empty ones, not undefined", async () => {
  // A client that renders `undefined.map` on a field an older daemon does not send
  // is a blank config view, which reads as "you have no accounts" — the exact
  // confusion the section's error line exists to avoid.
  stubFetch({});
  const resp = await listAccounts("T");
  assert.deepEqual(resp.entries, []);
  assert.deepEqual(resp.agents, []);
});

test("registerAccount: sends only the agent and the name", async () => {
  const cap = stubFetch({ entry: { agent: "codex", name: "work", dir: "/d", registration_only: false, logged_in: false } });
  await registerAccount("codex", "work", "T");
  assert.equal(cap.url, "/v1/RegisterAccount");
  assert.deepEqual(cap.body, { agent: "codex", name: "work" });
});

test("registerAccount: the daemon's refusal reaches the caller verbatim", async () => {
  // The name rule lives where the directory is created. A second copy in the
  // browser is how a UI comes to accept a name the writer rejects, so this client
  // sends the name unvalidated and shows what comes back.
  stubFetch(null, { ok: false, status: 500, error: 'account name "Work" collides with existing account "work"' });
  await assert.rejects(registerAccount("codex", "Work", "T"), (e: Error) =>
    e.message.includes("collides with existing account"),
  );
});

test("startAccountLogin: sends only the agent and the name — never a credential", async () => {
  const cap = stubFetch(loginResponse());
  const login = await startAccountLogin("codex", "work", "T");
  assert.equal(cap.url, "/v1/AccountLogin");
  // The whole standing constraint of #3388, as a check on the actual bytes: this
  // surface asks af to run the AGENT's flow against a directory, and there is no
  // field here through which a token could travel in either direction.
  assert.deepEqual(Object.keys(cap.body).sort(), ["agent", "name"]);
  assert.equal(login.program, "codex login");
  assert.equal(login.session_name, "af_af-login-codex-work");
});

test("loginWithoutPaneCopy: a flow that finished with a credential is a SUCCESS, not a failure", () => {
  // `codex login` against an account that already holds a credential prints and
  // exits. af reports that by the account's artifact rather than by the launch
  // error, and the copy has to follow — telling someone their login broke when it
  // just worked is the failure this branch exists to prevent.
  const copy = loginWithoutPaneCopy(loginResponse({ finished: true, logged_in: true, session_name: "" }));
  assert.equal(copy.status, "Logged in");
  assert.match(copy.detail, /without needing the terminal/);
});

test("loginWithoutPaneCopy: a flow that left the account empty says so and names the way to retry", () => {
  const copy = loginWithoutPaneCopy(loginResponse({ finished: true, logged_in: false, session_name: "" }));
  assert.equal(copy.status, "Not logged in");
  assert.match(copy.detail, /registered but not logged in/);
  assert.match(copy.detail, /af accounts login codex work/);
});

test("loginWithoutPaneCopy: the daemon's notices ride along with a finished login", () => {
  const copy = loginWithoutPaneCopy(
    loginResponse({ finished: true, logged_in: true, session_name: "", notices: ["CODEX_HOME relocates the whole home."] }),
  );
  assert.match(copy.detail, /relocates the whole home/);
});

test("loginTerminalStatusCopy: a pane that exits is the FLOW ending, not a dropped stream", () => {
  // The assistant's copy says "disconnected" here. For a login the pane ending is
  // the expected outcome — the sign-in finished — and reading it as a failure at
  // the moment of success is exactly backwards.
  const copy = loginTerminalStatusCopy("exited", loginResponse());
  assert.match(copy, /login flow ended/);
  assert.doesNotMatch(copy, /disconnect/i);
});

test("loginTerminalStatusCopy: joining a flow already open says so", () => {
  assert.match(loginTerminalStatusCopy("open", loginResponse({ reused: true })), /Joined/);
  assert.equal(loginTerminalStatusCopy("open", loginResponse({ reused: false })), "Live");
});

test("emptyAccountsState: the shell's starting point renders as 'nothing yet', not as an error", () => {
  const state = emptyAccountsState();
  assert.deepEqual(state.entries, []);
  assert.deepEqual(state.agents, []);
  assert.equal(state.error, "");
  assert.equal(state.status, null);
});
