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
  assert.deepEqual(resp.accounts, []);
  assert.equal(resp.mover, "");
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
    mover: "halted: move-failed since 2026-01-01T09:00:00Z",
  });
  const resp = await quotaReport("tok");
  assert.equal(resp.agents[0].program, "codex");
  assert.equal(resp.agents[0].observed, "limit reached");
  assert.equal(resp.agents[0].limited_sessions, 1);
  assert.equal(resp.warnings?.length, 1);
  // The mover line is the daemon's own words — a recorded halt arrives verbatim
  // so the Usage section can never silently drop it (#6776 R16).
  assert.equal(resp.mover, "halted: move-failed since 2026-01-01T09:00:00Z");
});

test("quotaReport: per-account evidence rows pass through untouched (#6776)", async () => {
  stubFetch({
    agents: [],
    accounts: [
      {
        agent: "codex",
        name: "work",
        observed: "observed",
        windows: "primary 42%, secondary 4%",
        detail: "recorded 2026-01-01T09:00:00Z",
      },
      {
        agent: "codex",
        name: "personal",
        observed: "no evidence",
        windows: "—",
        detail: "no rate_limits claims found",
      },
    ],
    mover: "shadow",
  });
  const resp = await quotaReport("tok");
  assert.equal(resp.accounts?.length, 2);
  assert.equal(resp.accounts?.[0].agent, "codex");
  assert.equal(resp.accounts?.[0].name, "work");
  assert.equal(resp.accounts?.[0].observed, "observed");
  assert.equal(resp.accounts?.[0].windows, "primary 42%, secondary 4%");
  assert.equal(resp.accounts?.[0].detail, "recorded 2026-01-01T09:00:00Z");
  // The "no evidence" row's windows sentinel travels verbatim too: a blank in
  // a usage table reads as zero remaining, which af does not know.
  assert.equal(resp.accounts?.[1].windows, "—");
  assert.equal(resp.accounts?.[1].observed, "no evidence");
});

test("quotaReport: accounts defaults to [] when the daemon sends none (older daemon)", async () => {
  stubFetch({ agents: [], warnings: [], mover: "" });
  const resp = await quotaReport("tok");
  assert.deepEqual(resp.accounts, []);
});

test("emptyQuotaState: the shell's starting point renders as 'nothing yet', not as an error", () => {
  const state = emptyQuotaState();
  assert.equal(state.loaded, false);
  assert.equal(state.error, "");
  assert.deepEqual(state.agents, []);
  assert.deepEqual(state.accounts, []);
  assert.equal(state.mover, "");
});
