// The web client's quota contract (#2983). Pure logic + a stubbed fetch,
// matching accounts.test.ts — the rendered section is proven in the Playwright
// selftest, and the TUI's equivalent section is proven in
// ui/config_pane_quota_test.go.
//
// What is worth pinning here is the wire: the request is a no-argument RPC, and
// the answer's strings are used verbatim — a client that re-derives vocabulary
// from the rows is exactly the drift the served-rendered-rows design prevents.

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { quotaReport } from "./api.js";
import { emptyQuotaState } from "./quota.js";

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

test("quotaReport: a no-argument POST to the daemon's QuotaReport", async () => {
  const cap = stubFetch({ agents: [], warnings: [] });
  const resp = await quotaReport("tok");
  assert.equal(cap.url, "/v1/QuotaReport");
  assert.deepEqual(cap.body, {});
  assert.deepEqual(resp.agents, []);
});

test("quotaReport: a sparse response yields empty lists, never undefined", async () => {
  const cap = stubFetch({});
  const resp = await quotaReport("tok");
  assert.deepEqual(resp.agents, []);
  assert.deepEqual(resp.warnings, []);
  assert.equal(cap.calls, 1);
});

test("quotaReport: the rendered rows and warnings pass through untouched", async () => {
  stubFetch({
    agents: [
      {
        program: "codex",
        quota: "not reported",
        observed: "limit reached",
        sessions: 3,
        limited_sessions: 1,
        reset_at: "2026-01-01T10:00:00Z",
        detail: "1 of 3 session(s) parked at a usage limit; earliest reset 2026-01-01T10:00:00Z (in 5m0s)",
      },
    ],
    warnings: ["1 project record file(s) could not be read, so this report is INCOMPLETE; sessions in them are not counted: [repo-a]"],
  });
  const resp = await quotaReport("tok");
  assert.equal(resp.agents[0].program, "codex");
  assert.equal(resp.agents[0].observed, "limit reached");
  assert.equal(resp.agents[0].limited_sessions, 1);
  assert.equal(resp.warnings?.length, 1);
});

test("emptyQuotaState: the shell's starting point renders as 'nothing yet', not as an error", () => {
  const state = emptyQuotaState();
  assert.equal(state.loaded, false);
  assert.equal(state.error, "");
  assert.deepEqual(state.agents, []);
});
