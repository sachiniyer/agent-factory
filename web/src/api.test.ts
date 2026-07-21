// Tests for the write-side lifecycle callers (#1592 Phase 5 follow-up): they must
// send the session's STABLE id as the primary lookup key, not just the title. The
// daemon resolves the action target by id first, so a cross-repo duplicate title
// can't make a web kill/archive hit the wrong session. These pin the id in
// the request body without a daemon (fetch is stubbed).

import { test, afterEach } from "node:test";
import assert from "node:assert/strict";

import {
  ApiError,
  archiveSession,
  closeTab,
  createSession,
  type CreateSessionInput,
  createTab,
  errorText,
  killSession,
  listBackends,
  listPrograms,
  probeWebTab,
  removeTask,
  renameTab,
  reorderTab,
  restoreSession,
  resumeFromLimit,
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

// The backend-on-create contract (#1933). The daemon already accepted `backend`;
// these pin that the web now sends it — and, just as load-bearing, that it stays
// ABSENT when the user made no choice.

/** A create form submission with no backend chosen (the default state). */
function createInput(over: Partial<CreateSessionInput> = {}): CreateSessionInput {
  return { title: "feature", repoPath: "/repos/af", program: "", prompt: "", autoYes: false, ...over };
}

for (const backend of ["local", "docker", "ssh", "hook"]) {
  test(`createSession sends backend=${backend} when the user picks it`, async () => {
    const cap = stubFetch();
    await createSession(createInput({ backend }), "tok");
    assert.equal(cap.url, "/v1/CreateSession");
    assert.equal(cap.body.backend, backend, "an explicit choice must reach the daemon verbatim, as `af sessions create --backend` does");
    assert.equal(cap.body.repo_path, "/repos/af");
  });
}

test("createSession omits backend entirely when the user chose the repo default", async () => {
  // This is the subtle half. Sending "local" here would look equivalent and is
  // not: an explicit backend WINS over the repo's `backend` config key, so a repo
  // configured for docker would silently create local sessions from the web while
  // the CLI honoured docker. Absent is the only encoding of "let the repo decide".
  const cap = stubFetch();
  await createSession(createInput({ backend: "" }), "tok");

  assert.equal("backend" in cap.body, false, "no choice must send NO backend key, not an explicit local");
});

test("createSession omits backend when the field is absent altogether", async () => {
  const cap = stubFetch();
  await createSession(createInput(), "tok");

  assert.equal("backend" in cap.body, false, "an undefined backend is the same 'let the repo decide' as an empty one");
});

test("listBackends asks the daemon for the picked repo's catalog", async () => {
  const cap = stubFetch();
  await listBackends("/repos/af", "tok");

  assert.equal(cap.url, "/v1/ListBackends");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.repo_path, "/repos/af", "availability and the default are per-repo facts");
});

// #1934: the verb re-delivers a PROMPT into a pane, so it must key by stable id
// like kill/archive — not by title like restore. A title-resolved misroute here
// types someone's instruction into an unrelated repo's agent.
test("resumeFromLimit posts the stable id, so a duplicate title cannot misroute the prompt", async () => {
  const cap = stubFetch();
  await resumeFromLimit("id-repoB", "feature", "tok");

  assert.equal(cap.url, "/v1/ResumeFromLimit");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.id, "id-repoB", "the daemon resolves by id first");
  assert.equal(cap.body.title, "feature", "the title still rides along for the event and the title-only fallback");
  assert.equal(cap.body.repo_id, "", "an all-repos web client scopes by id, not repo");
});

test("listPrograms asks the daemon for the agent catalog (#1970)", async () => {
  const cap = stubFetch();
  await listPrograms("/repos/af", "tok");

  assert.equal(cap.url, "/v1/ListPrograms");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.repo_path, "/repos/af", "the repo sharpens which program 'repo default' resolves to");
});

// Unlike listBackends, a repo-less request is legitimate: the agent enum is global,
// so a form with no project picked yet still gets the list. Sending the field as ""
// rather than omitting it keeps the wire shape uniform — the daemon treats an empty
// repo_path as "no repo context" and answers with the global default.
test("listPrograms works with no repo picked", async () => {
  const cap = stubFetch();
  await listPrograms("", "tok");

  assert.equal(cap.url, "/v1/ListPrograms");
  assert.equal(cap.body.repo_path, "");
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

// restoreSession is the archive round-trip's missing return leg (#1932): the web
// could archive but never restore. Unlike kill/archive it resolves by TITLE only —
// the daemon's RestoreSessionRequest (daemon/control_types.go) has no `id` field —
// so these pin BOTH that it hits the RestoreSession route the CLI/TUI use AND that
// it sends NO `id`: a web request carries no client-version header, so the daemon
// decodes with DisallowUnknownFields and a stray `id` would be a 400.
test("restoreSession posts to RestoreSession by title, with an empty repo_id", async () => {
  const cap = stubFetch();
  await restoreSession("feature", "tok");
  assert.equal(cap.url, "/v1/RestoreSession", "must hit the same route af sessions restore / the TUI `r` use");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.repo_id, "", "web is an all-repos client; repo_id stays empty, as it does for archive/kill");
});

test("restoreSession does NOT send an id (RestoreSessionRequest has no id field)", async () => {
  const cap = stubFetch();
  await restoreSession("feature", "tok");
  // The daemon decodes web requests with DisallowUnknownFields; an `id` key it does
  // not know would be rejected as a 400 "unknown field". Archive/kill send `id`
  // only because THEIR request structs accept it — restore's does not.
  assert.equal("id" in cap.body, false, "no id key may reach the daemon, or DisallowUnknownFields 400s the restore");
});

test("createTab / closeTab post the stable id alongside the title", async () => {
  const cap = stubFetch();
  await createTab("id-repoB", "feature", "tok");
  assert.equal(cap.url, "/v1/CreateTab");
  assert.equal(cap.body.id, "id-repoB", "CreateTab must resolve by id, not the cross-repo title");
  assert.equal(cap.body.title, "feature");
  assert.equal(cap.body.repo_id, "");
  assert.equal(cap.body.shell, true, "the web `t` creates a $SHELL tab, like the TUI");

  await closeTab("id-repoB", "feature", "shell", "tab-abc", "tok");
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
    () => closeTab("", "feature", "shell", "tab-abc", "tok"),
    (e: unknown) => e instanceof ApiError && /no stable id/.test((e as ApiError).message),
    "closeTab with an empty id must reject",
  );
  assert.equal(cap.calls, 0, "no request may be issued for a tab op with a missing id");
});

// --- tab rename/reorder/close carry the STABLE tab id (#1929, #1971) -------
//
// The daemon resolves the tab id-first and falls back to the name/index path, so these
// send `tab_id` alongside the name the fallback still needs. Name-keying alone is what
// makes a rename/reorder racy: the name is the very thing a rename CHANGES, and a
// concurrent one from another window (or the CLI) leaves this request naming a tab that
// no longer exists — or, worse, a different tab that has since taken the name.
//
// Close carries it for the same reason and one more: it is the DESTRUCTIVE verb, so its
// misroute kills a tmux session rather than mislabelling a tab (#1971).

test("renameTab posts the stable tab id alongside the current name (#1929)", async () => {
  const cap = stubFetch();
  await renameTab("id-repoB", "feature", "alpha", "storefront", "tab-abc", "tok");
  assert.equal(cap.url, "/v1/RenameTab");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(cap.body.tab_id, "tab-abc", "the daemon must be able to resolve the tab by identity, not name");
  assert.equal(cap.body.tab_name, "alpha", "the name stays for the daemon's documented fallback path");
  assert.equal(cap.body.new_name, "storefront");
});

test("reorderTab posts the stable tab id alongside the current name (#1929)", async () => {
  const cap = stubFetch();
  await reorderTab("id-repoB", "feature", "alpha", 3, "tab-abc", "tok");
  assert.equal(cap.url, "/v1/ReorderTab");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(cap.body.tab_id, "tab-abc");
  assert.equal(cap.body.tab_name, "alpha");
  assert.equal(cap.body.new_index, 3, "the wire field is new_index; `index` is what comes BACK");
});

test("renameTab / reorderTab omit tab_id for a legacy tab that has none (#1929)", async () => {
  // A pre-#1738 record carries no stable id. Sending "" is the documented fallback —
  // the daemon resolves by name/index exactly as it does today — NOT a bug, and NOT a
  // reason to refuse: unlike the session id (whose absence is the #1678 cross-repo
  // landmine that makes closeTab fail closed), a tab name is only ever resolved WITHIN
  // an already-id-resolved session, so the blast radius is that one session's own bar.
  const cap = stubFetch();
  await renameTab("id-repoB", "feature", "alpha", "storefront", "", "tok");
  assert.equal(cap.body.tab_id, "", "an absent tab id is the empty string — the daemon's name fallback");
  assert.equal(cap.body.tab_name, "alpha", "…and the name path still carries the rename");
  assert.equal(cap.calls, 1, "a legacy tab is still renameable — the missing id must not refuse it");

  await reorderTab("id-repoB", "feature", "alpha", 3, "", "tok");
  assert.equal(cap.body.tab_id, "");
  assert.equal(cap.body.tab_name, "alpha");
  assert.equal(cap.calls, 2);

  await closeTab("id-repoB", "feature", "alpha", "", "tok");
  assert.equal(cap.body.tab_id, "", "a legacy tab must still be closable by name");
  assert.equal(cap.body.tab_name, "alpha");
  assert.equal(cap.calls, 3);
});

test("closeTab posts the stable tab id alongside the current name (#1971)", async () => {
  const cap = stubFetch();
  await closeTab("id-repoB", "feature", "alpha", "tab-abc", "tok");
  assert.equal(cap.url, "/v1/CloseTab");
  assert.equal(cap.body.id, "id-repoB");
  assert.equal(
    cap.body.tab_id,
    "tab-abc",
    "close must resolve by identity: a freed name is reissued to the next tab, so name-keying kills the wrong tmux session",
  );
  assert.equal(cap.body.tab_name, "alpha", "the name stays for the daemon's documented fallback path");
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

// --- probeWebTab: the out-of-band web-tab health probe (#1813 / Codex P2) ------
//
// It reports "dead" for exactly the states the designed fallback exists for — a 502/504
// the DAEMON generated (marked as its own, #1909), a transport failure, and (Codex P2) a
// probe that never answers within the timeout — and "ok" for everything else, since any
// other answer means the dev server itself answered and its page should render.

/** Stubs global.fetch with one that resolves to `status` plus `headers`, honoring an
 *  abort signal. Real `Response`s always carry a Headers object, so the stub does too:
 *  the probe reads the #1909 marker off it. */
function stubProbeStatus(status: number, headers: Record<string, string> = {}): void {
  (globalThis as { fetch: unknown }).fetch = async (_url: string, init: RequestInit): Promise<Response> => {
    if (init.signal?.aborted) {
      throw new DOMException("aborted", "AbortError");
    }
    return { status, headers: new Headers(headers) } as unknown as Response;
  };
}

/** The marker the daemon sets on a 502 IT generated (daemon/webtab_proxy.go). */
const AF_ERR = { "x-af-webtab-error": "upstream-unreachable" };

test("probeWebTab: a 502/504 the DAEMON generated is dead (#1909)", async () => {
  // Marked = af's own ErrorHandler replaced the response because the dev server never
  // answered. This is the state the dead-server fallback exists for.
  for (const s of [502, 504]) {
    stubProbeStatus(s, AF_ERR);
    assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "dead", `af-generated ${s} must be dead`);
  }
});

test("probeWebTab: an UPSTREAM's own 502/504 means the app answered — render its page (#1909)", async () => {
  // The #1909 bug: the proxy forwards upstream statuses unchanged, so an app that
  // serves its own 502 (a framework proxy whose backend is down, a gateway error page)
  // looked exactly like af's. Keying on the bare status suppressed the app's real page
  // and showed af's dead-server fallback instead. The marker's ABSENCE is what says
  // "the app answered".
  for (const s of [502, 504]) {
    stubProbeStatus(s); // no marker: af did not generate this
    assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "ok", `upstream-generated ${s} must render`);
  }
});

test("probeWebTab: the marker is keyed on presence, not value, and read case-insensitively", async () => {
  // The daemon may add a second failure reason without a client change, and header
  // names are case-insensitive on the wire — a probe that matched one exact spelling
  // would silently stop recognizing af's own failures.
  stubProbeStatus(502, { "X-AF-Webtab-Error": "some-future-reason" });
  assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "dead");
});

test("probeWebTab: any other status means the dev server answered — render it", async () => {
  for (const s of [200, 204, 404, 500, 503]) {
    stubProbeStatus(s);
    assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "ok", `status ${s} must be ok`);
  }
});

test("probeWebTab: the probe does not follow redirects — it cannot be steered off-origin", async () => {
  // fetch FOLLOWS redirects by default, so a preview that answers with a redirect to a
  // cross-origin destination (an OAuth/SSO login is the everyday case) made the PARENT
  // document's probe follow it off-origin — the probe steerable by the very thing it
  // probes, subverting the same-origin assumption it rests on. Worse, if the final
  // origin disallows CORS the fetch rejects and we report the dev server dead, though
  // the frame would have followed that redirect happily.
  let seenRedirect: RequestRedirect | undefined;
  (globalThis as { fetch: unknown }).fetch = async (_url: string, init: RequestInit): Promise<Response> => {
    seenRedirect = init.redirect;
    return { status: 200, headers: new Headers() } as unknown as Response;
  };
  await probeWebTab("/v1/webtab/s/t/", "", 1000);
  assert.equal(seenRedirect, "manual", "the probe must not follow a redirect the probed server chose");
});

test("probeWebTab: a redirect is an ANSWERING server — ok, and the frame follows it itself", async () => {
  // Under redirect:"manual" the browser hands back an opaque-redirect response: status
  // 0, empty headers. It is not a failure — the server answered — and the frame is
  // entitled to follow the redirect on its own, which is what a real preview does.
  (globalThis as { fetch: unknown }).fetch = async (): Promise<Response> =>
    ({ status: 0, type: "opaqueredirect", headers: new Headers() }) as unknown as Response;
  assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "ok", "a redirecting server is alive");
});

test("probeWebTab: a transport failure is dead", async () => {
  (globalThis as { fetch: unknown }).fetch = async (): Promise<Response> => {
    throw new TypeError("Failed to fetch");
  };
  assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 1000), "dead");
});

test("probeWebTab: a fetch that never resolves is aborted at the timeout and reported dead (Codex P2)", async () => {
  // The exact bug: a target that ACCEPTS the connection but never sends headers. The
  // stub honors the AbortController signal the probe arms; without the timeout this
  // promise would hang forever (and the pane would stay blank).
  (globalThis as { fetch: unknown }).fetch = (_url: string, init: RequestInit): Promise<Response> =>
    new Promise((_resolve, reject) => {
      init.signal?.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")));
      // never resolves otherwise
    });
  const started = Date.now();
  assert.equal(await probeWebTab("/v1/webtab/s/t/", "", 30), "dead");
  assert.ok(Date.now() - started < 2000, "must resolve at the timeout, not hang");
});

test("probeWebTab: the token rides the Authorization header, never the URL", async () => {
  let seenAuth: string | undefined;
  let seenUrl = "";
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    seenUrl = url;
    seenAuth = (init.headers as Record<string, string>).Authorization;
    return { status: 200 } as unknown as Response;
  };
  await probeWebTab("/v1/webtab/s/t/", "secret-tok", 1000);
  assert.equal(seenAuth, "Bearer secret-tok", "the probe is the parent's request — header, not ?access_token");
  assert.ok(!seenUrl.includes("secret-tok"), "the token must never appear in the probe URL");
});
