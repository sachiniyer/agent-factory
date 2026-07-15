// Tests for the write-side lifecycle callers (#1592 Phase 5 follow-up): they must
// send the session's STABLE id as the primary lookup key, not just the title. The
// daemon resolves the action target by id first, so a cross-repo duplicate title
// can't make a web kill/archive/prompt hit the wrong session. These pin the id in
// the request body without a daemon (fetch is stubbed).

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import {
  ApiError,
  archiveSession,
  closeTab,
  createTab,
  errorText,
  killSession,
  removeTask,
  sendPrompt,
  triggerTask,
  updateTask,
} from "./api.js";

interface Captured {
  url: string;
  body: Record<string, unknown>;
  auth: string | undefined;
  calls: number;
}

// Stubs global.fetch, capturing the last request (and a call count) and returning a
// 200 {data,error} envelope. Returns the capture box the test asserts against.
function stubFetch(): Captured {
  const cap: Captured = { url: "", body: {}, auth: undefined, calls: 0 };
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    cap.calls += 1;
    cap.url = url;
    cap.body = JSON.parse(String(init.body));
    cap.auth = (init.headers as Record<string, string>).Authorization;
    return {
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ data: { ok: true, name: "shell" }, error: null }),
    } as unknown as Response;
  };
  return cap;
}

afterEach(() => {
  delete (globalThis as { fetch?: unknown }).fetch;
});

test("killSession posts the stable id as the primary key alongside the title", async () => {
  const cap = stubFetch();
  await killSession("id-repoB", "feature", "tok");
  assert.equal(cap.url, "/v1/KillSession");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.id, "id-repoB", "id must be sent so the daemon resolves by id, not title");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.repo_id, "", "web is an all-repos client; repo_id stays empty");
});

test("archiveSession posts the stable id alongside the title", async () => {
  const cap = stubFetch();
  await archiveSession("id-repoB", "feature", "tok");
  assert.equal(cap.url, "/v1/ArchiveSession");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.repo_id, "");
});

test("sendPrompt posts the stable id alongside the title and prompt", async () => {
  const cap = stubFetch();
  await sendPrompt("id-repoB", "feature", "do the thing", "tok");
  assert.equal(cap.url, "/v1/SendPrompt");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.prompt, "do the thing");
  assert.equal(cap.body.repo_id, "");
});

test("createTab / closeTab post the stable id alongside the title", async () => {
  const cap = stubFetch();
  await createTab("id-repoB", "feature", "tok");
  assert.equal(cap.url, "/v1/CreateTab");
  assert.equal(cap.body.id, "id-repoB", "CreateTab must resolve by id, not the cross-repo title");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.repo_id, "");
  assert.equal(cap.body.shell, true, "the web `t` creates a $SHELL tab, like the TUI");

  await closeTab("id-repoB", "feature", "shell", "tok");
  assert.equal(cap.url, "/v1/CloseTab");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(cap.body.tab_name, "shell");
  assert.equal(cap.body.repo_id, "");
});

test("createTab / closeTab FAIL CLOSED on a missing id — no title-scoped request", async () => {
  const cap = stubFetch();
  // A session with no stable id (a legacy/disk-only row) must NOT be tab-mutated by
  // an all-repo title match — the #1678 cross-repo landmine. Both refuse BEFORE any
  // request, so the daemon never sees an empty id to title-resolve.
  await assert.rejects(
    () => createTab("", "feature", "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "createTab with an empty id must reject",
  );
  await assert.rejects(
    () => closeTab("", "feature", "shell", "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "closeTab with an empty id must reject",
  );
  assert.equal(cap.calls, 0, "no request may be issued for a tab op with a missing id");
});

// --- task mutations (#1592 Phase 5 PR8) ------------------------------------

test("updateTask posts a field-level patch keyed by the stable id", async () => {
  const cap = stubFetch();
  await updateTask("t-abc123", { enabled: false }, "tok");
  assert.equal(cap.url, "/v1/UpdateTask");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.id, "t-abc123", "the daemon resolves the task by its unique id, not its name");
  const sent = cap.body.update as Record<string, unknown>;
  assert.equal(sent.enabled, false, "the enable/disable toggle rides UpdateTask as an `enabled`-only patch");
  // A toggle must carry ONLY the flipped field — never the rest of a cached task
  // that could clobber a concurrent edit (#1700).
  assert.deepEqual(Object.keys(sent), ["enabled"], "the toggle patch carries only the enabled field");
});

test("triggerTask / removeTask post the stable id", async () => {
  const cap = stubFetch();
  await triggerTask("t-abc123", "tok");
  assert.equal(cap.url, "/v1/TriggerTask");
  assert.equal(cap.body.id, "t-abc123");

  await removeTask("t-abc123", "tok");
  assert.equal(cap.url, "/v1/RemoveTask");
  assert.equal(cap.body.id, "t-abc123");
});

test("updateTask / triggerTask / removeTask FAIL CLOSED on a missing task id", async () => {
  const cap = stubFetch();
  // A task with no id must NOT be mutated by a daemon first-match on another task —
  // the task analogue of the #1678 id-scoping class. Each refuses BEFORE any request.
  await assert.rejects(
    () => updateTask("", { enabled: false }, "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "updateTask with an empty id must reject",
  );
  await assert.rejects(
    () => triggerTask("", "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "triggerTask with an empty id must reject",
  );
  await assert.rejects(
    () => removeTask("", "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "removeTask with an empty id must reject",
  );
  assert.equal(cap.calls, 0, "no request may be issued for a task mutation with a missing id");
});

// --- error rendering ------------------------------------------------------
//
// The daemon marshals the envelope's error as an OBJECT ({"message":"..."},
// apiproto.EnvelopeError), but this client used to type it as a string and pass it
// straight to `new ApiError(status, env.error)`. That coerced the object via
// String(), so every error surface — the login screen, the modals, the tab toast —
// rendered the literal text "[object Object]" instead of what went wrong. These
// pin the wire shape and the extraction, because the old tests only ever mocked
// `error: null` and so never touched the error path at all.

/** Stubs fetch with one canned response, for the error paths. */
function stubFetchResponse(resp: { ok: boolean; status: number; statusText?: string; json: () => Promise<unknown> }): void {
  (globalThis as { fetch: unknown }).fetch = async (): Promise<Response> =>
    ({ statusText: "", ...resp }) as unknown as Response;
}

test("an envelope error object renders its message, never [object Object]", async () => {
  stubFetchResponse({
    ok: false,
    status: 400,
    statusText: "Bad Request",
    json: async () => ({ data: null, error: { message: "session \"nope\" not found" } }),
  });
  const err = await killSession("id", "nope", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.status, 400);
  assert.equal(err.message, 'session "nope" not found');
  assert.doesNotMatch(err.message, /\[object Object\]/);
});

test("a 200 carrying an envelope error still surfaces the real message", async () => {
  stubFetchResponse({
    ok: true,
    status: 200,
    statusText: "OK",
    json: async () => ({ data: null, error: { message: "tab cap reached" } }),
  });
  const err = await createTab("id", "s", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.message, "tab cap reached");
});

test("a string-shaped envelope error is still rendered (wire tolerance)", async () => {
  // Not the daemon's shape, but an older daemon or a proxy could send it. The unwrap
  // is liberal on purpose — the alternative is an unreadable error surface.
  stubFetchResponse({
    ok: false,
    status: 400,
    statusText: "Bad Request",
    json: async () => ({ data: null, error: "legacy string error" }),
  });
  const err = await killSession("id", "s", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.message, "legacy string error");
});

test("an error with no usable message falls back to the status line", async () => {
  stubFetchResponse({
    ok: false,
    status: 503,
    statusText: "Service Unavailable",
    json: async () => ({ data: null, error: {} }),
  });
  const err = await killSession("id", "s", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.message, "503 Service Unavailable");
  assert.doesNotMatch(err.message, /\[object Object\]|undefined/);
});

test("a non-JSON error body falls back to the status line", async () => {
  stubFetchResponse({
    ok: false,
    status: 502,
    statusText: "Bad Gateway",
    json: async () => {
      throw new SyntaxError("Unexpected token < in JSON");
    },
  });
  const err = await killSession("id", "s", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.message, "502 Bad Gateway");
});

test("a transport failure names the cause and stays readable", async () => {
  (globalThis as { fetch: unknown }).fetch = async (): Promise<Response> => {
    throw new TypeError("Failed to fetch");
  };
  const err = await killSession("id", "s", "tok").then(
    () => null,
    (e: unknown) => e,
  );
  assert.ok(err instanceof ApiError);
  assert.equal(err.status, 0);
  assert.equal(err.message, "cannot reach the daemon: Failed to fetch");
  assert.doesNotMatch(err.message, /\[object Object\]|undefined/);
});

test("errorText extracts a readable string from every throwable shape", () => {
  assert.equal(errorText(new Error("boom")), "boom");
  assert.equal(errorText(new ApiError(401, "unauthorized")), "unauthorized");
  assert.equal(errorText("plain string"), "plain string");
  // The envelope error object — the exact shape that produced "[object Object]".
  assert.equal(errorText({ message: "from the envelope" }), "from the envelope");
  // No message anywhere: JSON is still more useful than "[object Object]".
  assert.equal(errorText({ code: 7 }), '{"code":7}');
  // Nothing usable at all falls back rather than rendering "undefined".
  assert.equal(errorText(undefined), "unknown error");
  assert.equal(errorText(null), "unknown error");
  assert.equal(errorText({}), "unknown error");
  assert.equal(errorText(new Error("")), "unknown error");
  assert.equal(errorText(undefined, "custom fallback"), "custom fallback");
  // A circular object cannot be JSON'd — must not throw, must not leak the coercion.
  const circular: Record<string, unknown> = {};
  circular.self = circular;
  assert.equal(errorText(circular), "unknown error");
});
