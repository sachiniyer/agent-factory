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
