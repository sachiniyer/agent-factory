// Tests for the write-side lifecycle callers (#1592 Phase 5 follow-up): they must
// send the session's STABLE id as the primary lookup key, not just the title. The
// daemon resolves the action target by id first, so a cross-repo duplicate title
// can't make a web kill/archive/prompt hit the wrong session. These pin the id in
// the request body without a daemon (fetch is stubbed).

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import { archiveSession, killSession, sendPrompt } from "./api.js";

interface Captured {
  url: string;
  body: Record<string, unknown>;
  auth: string | undefined;
}

// Stubs global.fetch, capturing the last request and returning a 200 {data,error}
// envelope. Returns the capture box the test asserts against.
function stubFetch(): Captured {
  const cap: Captured = { url: "", body: {}, auth: undefined };
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    cap.url = url;
    cap.body = JSON.parse(String(init.body));
    cap.auth = (init.headers as Record<string, string>).Authorization;
    return {
      ok: true,
      status: 200,
      statusText: "OK",
      json: async () => ({ data: { ok: true }, error: null }),
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
