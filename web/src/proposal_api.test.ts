import assert from "node:assert/strict";
import { afterEach, test } from "node:test";
import { ApiError } from "./api.js";
import {
  addDoc,
  changeNamesDoc,
  getDoc,
  getProposal,
  holdPush,
  parseDocText,
  parsePushStatus,
  parseProposalPush,
  parseProposalChange,
  parseProposalInfo,
  parseSaveResponse,
  putDoc,
  resetFenceSupport,
  sendChanges,
} from "./proposal_api.js";

const realFetch = globalThis.fetch;
afterEach(() => {
  globalThis.fetch = realFetch;
  resetFenceSupport();
});

function recordFetch(status: number, body: unknown): { url: string; init: RequestInit }[] {
  const calls: { url: string; init: RequestInit }[] = [];
  globalThis.fetch = (async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    return new Response(JSON.stringify(body), { status });
  }) as typeof fetch;
  return calls;
}

test("parseProposalInfo reads the GetProposal detail", () => {
  const raw = {
    slug: "auth-rework",
    path: "/abs",
    head: "9c1d",
    docs: [{ name: "goals", file: "goals.md" }, { name: "api", file: "api.md" }],
    templates: ["ui", "future"],
    open_questions: 2,
    attached_sessions: ["auth-planner"],
  };
  assert.deepEqual(parseProposalInfo(raw), {
    slug: "auth-rework",
    head: "9c1d",
    docs: raw.docs,
    templates: ["ui", "future"],
    open_questions: 2,
    push: null,
  });
  assert.deepEqual(parseProposalInfo({ slug: "s", docs: ["goals.md"] }).docs, [{ name: "goals", file: "goals.md" }]);
});

test("parseDocText needs text", () => {
  assert.deepEqual(parseDocText({ slug: "s", doc: "goals.md", text: "# Goals", commit: "abc" }), { text: "# Goals", commit: "abc" });
  assert.throws(() => parseDocText({ commit: "abc" }), ApiError);
});

test("parseSaveResponse reads a save, a merge and a conflict", () => {
  assert.deepEqual(parseSaveResponse(200, { data: { slug: "s", doc: "api.md", text: "t", commit: "c1", merged: false }, error: null }),
    { kind: "saved", text: "t", commit: "c1", merged: false });
  assert.deepEqual(parseSaveResponse(200, { data: { text: "m", commit: "c2", merged: true }, error: null }),
    { kind: "saved", text: "m", commit: "c2", merged: true });
  assert.deepEqual(parseSaveResponse(409, {
    data: { ours: "o", theirs: "t", base: "b", head: "9c1d" },
    error: { message: "api.md changed since base 3f2a, and the edits conflict", code: "proposal_conflict" },
  }), { kind: "conflict", ours: "o", theirs: "t", base: "b", head: "9c1d" });
  assert.throws(() => parseSaveResponse(409, { data: null, error: { message: "stale" } }), /stale/);
  assert.throws(() => parseSaveResponse(500, { data: null, error: { message: "boom", code: "x" } }), /boom/);
});

test("the calls use the documented RPCs and address the proposal by session", async () => {
  let calls = recordFetch(200, { data: { slug: "auth", docs: [], templates: [], head: "h" }, error: null });
  await getProposal("s1", "auth", "tok");
  assert.equal(calls[0]!.url, "/v1/GetProposal");
  assert.deepEqual(JSON.parse(calls[0]!.init.body as string), { session_id: "s1", slug: "auth" });
  assert.equal((calls[0]!.init.headers as Record<string, string>).Authorization, "Bearer tok");

  calls = recordFetch(200, { data: { text: "x", commit: "c" }, error: null });
  assert.deepEqual(await getDoc("s1", "auth", "api", ""), { text: "x", commit: "c" });
  assert.equal(calls[0]!.url, "/v1/ReadProposalDoc");
  assert.deepEqual(JSON.parse(calls[0]!.init.body as string), { session_id: "s1", slug: "auth", doc: "api" });

  calls = recordFetch(409, { data: { ours: "o", theirs: "t", base: "b", head: "h" }, error: { message: "m", code: "proposal_conflict" } });
  assert.equal((await putDoc("s1", "auth", "api", "o", "c", "")).kind, "conflict");
  assert.equal(calls[0]!.url, "/v1/SaveProposalDoc");
  assert.deepEqual(JSON.parse(calls[0]!.init.body as string), { session_id: "s1", slug: "auth", doc: "api", text: "o", base: "c" });

  calls = recordFetch(200, { data: { slug: "auth", doc: "risks.md", path: "/p", commit: "c" }, error: null });
  await addDoc("s1", "auth", "risks", "");
  assert.equal(calls[0]!.url, "/v1/AddProposalDoc");
  assert.deepEqual(JSON.parse(calls[0]!.init.body as string), { session_id: "s1", slug: "auth", name: "risks", author: "human" });
});

test("parseProposalChange reads the event data, including a null commit", () => {
  const ev = {
    type: "proposal.changed",
    data: { repo_id: "r", session_ids: ["s1"], slug: "auth", doc: "api.md", commit: "9c1d", author: "agent" },
  };
  const want = { repoId: "r", slug: "auth", doc: "api.md", commit: "9c1d", author: "agent" };
  assert.deepEqual(parseProposalChange(ev), want);
  assert.equal(parseProposalChange({ ...ev, data: { ...ev.data, commit: null, author: "external" } })?.commit, "");
  assert.equal(parseProposalChange({ type: "session.updated" }), null);
  assert.equal(parseProposalChange({ type: "proposal.changed", data: {} }), null);
  assert.ok(changeNamesDoc(want, { name: "api", file: "api.md" }));
  assert.ok(changeNamesDoc({ ...want, doc: "api" }, { name: "api", file: "api.md" }));
  assert.ok(!changeNamesDoc(want, { name: "ui", file: "ui.md" }));
});

test("parsePushStatus reads the push status and refuses other values", () => {
  assert.deepEqual(parsePushStatus({
    session_id: "s1", slug: "plan", mode: "auto", state: "countdown",
    due_at: "2026-10-20T22:41:24.000Z", due_in_ms: 6000, unsent: 2,
  }), { sessionId: "s1", slug: "plan", mode: "auto", state: "countdown", dueInMs: 6000, unsent: 2 });
  assert.deepEqual(parsePushStatus({ state: "idle", mode: "manual", due_in_ms: null }), {
    sessionId: "", slug: "", mode: "manual", state: "idle", dueInMs: null, unsent: 0,
  });
  assert.equal(parsePushStatus({ state: "later" }), null);
  assert.equal(parsePushStatus(null), null);
  assert.equal(parseProposalInfo({ slug: "p", docs: [] }).push, null);
  assert.equal(parseProposalInfo({ slug: "p", docs: [], push: { state: "held", unsent: 1 } }).push?.state, "held");
});

test("parseProposalPush reads only proposal.push events that name a session and slug", () => {
  const data = { session_id: "s1", slug: "plan", mode: "auto", state: "sent", due_at: null, due_in_ms: null, unsent: 0 };
  assert.equal(parseProposalPush({ type: "proposal.push", data })?.state, "sent");
  assert.equal(parseProposalPush({ type: "proposal.changed", data }), null);
  assert.equal(parseProposalPush({ type: "proposal.push", data: { ...data, session_id: "" } }), null);
});

test("push routes post the session and parse the status", async () => {
  const reply = { data: { session_id: "s1", slug: "plan", mode: "manual", state: "idle", unsent: 0 }, error: null };
  const calls = recordFetch(200, reply);
  assert.equal((await holdPush("s1", "tok")).mode, "manual");
  await sendChanges("s1", "tok");
  assert.deepEqual(calls.map((c) => [c.url, JSON.parse(String(c.init.body))]), [
    ["/v1/HoldProposalPush", { session_id: "s1" }],
    ["/v1/SendProposalChanges", { session_id: "s1" }],
  ]);
  recordFetch(200, { data: {}, error: null });
  await assert.rejects(sendChanges("s1", ""), /push status/);
});

test("putDoc sends keepalive only when the serialized body fits the browser's 64 KiB cap", async () => {
  const calls = recordFetch(200, { data: { text: "x", commit: "c2", merged: false } });
  // 40,000 quotes are 40,000 bytes of text but more than 80,000 once JSON-escaped.
  await putDoc("s1", "auth", "api", '"'.repeat(40_000), "c1", "", true);
  assert.equal(calls[0]!.init.keepalive, false, "an escaped body over the cap is sent without keepalive");
  await putDoc("s1", "auth", "api", "short", "c1", "", true);
  assert.equal(calls[1]!.init.keepalive, true);
  await putDoc("s1", "auth", "api", "short", "c1", "", false);
  assert.equal(calls[2]!.init.keepalive, false, "an ordinary save is not keepalive");
});

test("putDoc sends the writer fence when it is given, and nothing extra when it is not", async () => {
  let calls = recordFetch(200, { data: { text: "t", commit: "c2", merged: false }, error: null });
  await putDoc("s1", "auth", "api", "t", "c1", "", false, { writer: "sync-1", seq: 7 });
  assert.deepEqual(JSON.parse(calls[0]!.init.body as string), { session_id: "s1", slug: "auth", doc: "api", text: "t", base: "c1", writer: "sync-1", seq: 7 });
  calls = recordFetch(200, { data: { text: "t", commit: "c2", merged: false }, error: null });
  await putDoc("s1", "auth", "api", "t", "c1", "");
  assert.deepEqual(Object.keys(JSON.parse(calls[0]!.init.body as string)).sort(), ["base", "doc", "session_id", "slug", "text"]);
});

test("putDoc sends the save again without the fence when the daemon does not know its fields, and stops sending it", async () => {
  const calls: { body: Record<string, unknown> }[] = [];
  const warnings: unknown[] = [];
  const realWarn = console.warn;
  console.warn = (...a: unknown[]) => void warnings.push(a);
  globalThis.fetch = (async (_url: string, init: RequestInit) => {
    const body = JSON.parse(init.body as string) as Record<string, unknown>;
    calls.push({ body });
    if ("writer" in body) {
      const refusal = { data: null, error: { message: 'malformed JSON request body: json: unknown field "writer"', code: "" } };
      return new Response(JSON.stringify(refusal), { status: 400 });
    }
    return new Response(JSON.stringify({ data: { text: "t", commit: "c2", merged: false }, error: null }), { status: 200 });
  }) as typeof fetch;
  try {
    const fence = { writer: "sync-1", seq: 3 };
    const first = await putDoc("s1", "auth", "api", "t", "c1", "", false, fence);
    assert.equal(first.kind, "saved", "the save goes through");
    assert.equal(calls.length, 2, "it was sent twice: refused, then again");
    assert.ok("writer" in calls[0]!.body && "seq" in calls[0]!.body);
    assert.ok(!("writer" in calls[1]!.body) && !("seq" in calls[1]!.body), "the second has no fence fields");
    assert.deepEqual(calls[1]!.body, { session_id: "s1", slug: "auth", doc: "api", text: "t", base: "c1" });
    await putDoc("s1", "auth", "api", "t2", "c2", "", false, { writer: "sync-1", seq: 4 });
    assert.equal(calls.length, 3, "later saves go once, without the fence");
    assert.ok(!("writer" in calls[2]!.body));
    assert.equal(warnings.length, 1, "logged once");
  } finally {
    console.warn = realWarn;
  }
});

test("putDoc does not retry a refusal that is not about the fence's fields", async () => {
  let n = 0;
  globalThis.fetch = (async () => {
    n++;
    return new Response(JSON.stringify({ data: null, error: { message: 'malformed JSON request body: json: unknown field "textt"', code: "" } }), { status: 400 });
  }) as typeof fetch;
  await assert.rejects(putDoc("s1", "auth", "api", "t", "c1", "", false, { writer: "w", seq: 1 }));
  assert.equal(n, 1);
});

test("a stale-save refusal is a plain refusal: it is not retried and does not switch the fence off", async () => {
  let n = 0;
  globalThis.fetch = (async () => {
    n++;
    return new Response(JSON.stringify({ data: null, error: { message: "stale", code: "proposal_stale_save" } }), { status: 409 });
  }) as typeof fetch;
  await assert.rejects(putDoc("s1", "auth", "api", "t", "c1", "", false, { writer: "w", seq: 1 }), (e: unknown) => (e as { code?: string }).code === "proposal_stale_save");
  assert.equal(n, 1);
  let sent: Record<string, unknown> = {};
  globalThis.fetch = (async (_u: string, init: RequestInit) => {
    sent = JSON.parse(init.body as string) as Record<string, unknown>;
    return new Response(JSON.stringify({ data: { text: "t", commit: "c", merged: false }, error: null }), { status: 200 });
  }) as typeof fetch;
  await putDoc("s1", "auth", "api", "t", "c1", "", false, { writer: "w", seq: 2 });
  assert.equal(sent.writer, "w", "the fence is still on");
});
