// Endpoint URL shapes (#2467). The load-bearing invariant is that
// sessionStreamEndpoint produces the EXACT URL AttachTerminal built inline before the
// endpoint seam existed — the refactor that let the config assistant reuse the
// terminal must not have changed a single session-stream request. These pin the wire
// bytes for both endpoints without importing xterm (stream_endpoint.ts is pure).

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import {
  accountLoginStreamEndpoint,
  configAssistantStreamEndpoint,
  sessionStreamEndpoint,
  wsScheme,
} from "./stream_endpoint.js";

// stream_endpoint reads window.location for the scheme and host. Node has no window,
// so stub a minimal one and restore it after each test.
function stubWindow(protocol: string, host: string): void {
  (globalThis as { window?: unknown }).window = { location: { protocol, host } };
}

afterEach(() => {
  delete (globalThis as { window?: unknown }).window;
});

test("sessionStreamEndpoint: the agent tab (tab 0, no id) carries only the token — byte-identical to the pre-seam URL", () => {
  stubWindow("http:", "localhost:8080");
  const ep = sessionStreamEndpoint("sess-1", "", 0);
  assert.equal(ep.url("T0KEN", null), "ws://localhost:8080/v1/sessions/sess-1/stream?access_token=T0KEN");
  // Tab 0 is an agent composer (Shift+Enter → LF).
  assert.equal(ep.composerNewline, true);
});

test("sessionStreamEndpoint: a stable tab id rides ?tab_id=, and wins over an ordinal", () => {
  stubWindow("http:", "localhost:8080");
  const ep = sessionStreamEndpoint("sess-1", "tab-abc", 3);
  assert.equal(
    ep.url("T", null),
    "ws://localhost:8080/v1/sessions/sess-1/stream?access_token=T&tab_id=tab-abc",
  );
  // A non-agent tab is NOT a composer.
  assert.equal(ep.composerNewline, false);
});

test("sessionStreamEndpoint: a legacy tab with no id falls back to the ordinal ?tab=", () => {
  stubWindow("http:", "localhost:8080");
  const ep = sessionStreamEndpoint("sess-1", "", 2);
  assert.equal(ep.url("T", null), "ws://localhost:8080/v1/sessions/sess-1/stream?access_token=T&tab=2");
});

test("sessionStreamEndpoint: a reconnect appends ?since= (the replay cursor), last", () => {
  stubWindow("http:", "localhost:8080");
  const ep = sessionStreamEndpoint("sess-1", "tab-abc", 1);
  assert.equal(
    ep.url("T", 4096n),
    "ws://localhost:8080/v1/sessions/sess-1/stream?access_token=T&tab_id=tab-abc&since=4096",
  );
});

test("sessionStreamEndpoint: the session id is URL-encoded", () => {
  stubWindow("http:", "localhost:8080");
  const ep = sessionStreamEndpoint("a/b c", "", 0);
  assert.equal(ep.url("T", null), "ws://localhost:8080/v1/sessions/a%2Fb%20c/stream?access_token=T");
});

test("wsScheme / endpoints: an https page yields wss", () => {
  stubWindow("https:", "af.example.com");
  assert.equal(wsScheme(), "wss:");
  const ep = sessionStreamEndpoint("s", "", 0);
  assert.equal(ep.url("T", null), "wss://af.example.com/v1/sessions/s/stream?access_token=T");
});

test("configAssistantStreamEndpoint: names no session, carries only the token, is a composer", () => {
  stubWindow("http:", "localhost:8080");
  const ep = configAssistantStreamEndpoint();
  assert.equal(ep.url("T", null), "ws://localhost:8080/v1/config-assistant/stream?access_token=T");
  assert.equal(ep.composerNewline, true);
});

test("configAssistantStreamEndpoint: a reconnect appends ?since= but never a tab param", () => {
  stubWindow("http:", "localhost:8080");
  const ep = configAssistantStreamEndpoint();
  const url = ep.url("T", 10n);
  assert.equal(url, "ws://localhost:8080/v1/config-assistant/stream?access_token=T&since=10");
  assert.ok(!url.includes("tab"), "config-assistant stream must not carry a tab selector");
});

test("accountLoginStreamEndpoint: names the ACCOUNT, not a tmux session, and is a composer", () => {
  stubWindow("http:", "localhost:8080");
  const ep = accountLoginStreamEndpoint("codex", "work");
  assert.equal(
    ep.url("T", null),
    "ws://localhost:8080/v1/account-login/stream?access_token=T&agent=codex&name=work",
  );
  // The login flow is interactive and the operator types into the agent's own
  // prompts, so it takes the composer newline like the assistant.
  assert.equal(ep.composerNewline, true);
});

test("accountLoginStreamEndpoint: a reconnect appends ?since= and keeps the account", () => {
  stubWindow("http:", "localhost:8080");
  const url = accountLoginStreamEndpoint("claude", "personal").url("T", 42n);
  assert.equal(
    url,
    "ws://localhost:8080/v1/account-login/stream?access_token=T&agent=claude&name=personal&since=42",
  );
});

test("accountLoginStreamEndpoint: an account name is escaped rather than pasted into the query", () => {
  stubWindow("http:", "localhost:8080");
  // agentaccount.ValidateName refuses a name like this where the directory is
  // created, so this URL addresses nothing — which is the point. The client must
  // still not be the thing that breaks: an unescaped value would end the query
  // and turn a refusal into a request for a DIFFERENT account.
  const url = accountLoginStreamEndpoint("codex", "a&name=other").url("T", null);
  assert.ok(url.includes("name=a%26name%3Dother"), `name was not escaped: ${url}`);
  assert.equal(url.split("name=").length - 1, 1, `the query carries two name params: ${url}`);
});
