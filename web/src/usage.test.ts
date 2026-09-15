// The web client's usage-report contracts (#4361). Same coverage shape as
// accounts.test.ts: the WIRE is pinned here with a stubbed fetch, and the
// section's rendered states are pinned against a stubbed document — because the
// failure this feature exists to prevent is a surface that reads "no limits
// anywhere" when the honest answer was "af could not look" or "af last saw the
// wall six days ago".

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { quotaReport } from "./api.js";
import { emptyUsageState, renderUsageSection, type UsageState } from "./usage.js";
import type { QuotaRow } from "./types.js";

interface Captured {
  url: string;
  body: Record<string, unknown>;
  auth: string | undefined;
  calls: number;
}

function stubFetch(data: unknown, opts: { ok?: boolean; status?: number; error?: string } = {}): Captured {
  const cap: Captured = { url: "", body: {}, auth: undefined, calls: 0 };
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    cap.calls += 1;
    cap.url = url;
    cap.body = JSON.parse(String(init.body));
    cap.auth = (init.headers as Record<string, string>).Authorization;
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

test("quotaReport posts to /v1/QuotaReport and carries the daemon's rows verbatim", async () => {
  const cap = stubFetch({
    rows: [
      { agent: "codex", quota: "not reported", observed: "limit reached", detail: "observed 2026-09-14T00:00:00Z (6d ago)" },
    ],
    note: "QUOTA is what the provider reports.",
    caveats: ["1 project record file(s) could not be parsed"],
  });
  const resp = await quotaReport("T0KEN");

  assert.equal(cap.url, "/v1/QuotaReport");
  assert.equal(cap.auth, "Bearer T0KEN");
  assert.deepEqual(cap.body, {}, "a read takes no parameters");
  assert.equal(resp.rows?.length, 1);
  assert.equal(resp.rows?.[0]?.observed, "limit reached");
  assert.equal(resp.rows?.[0]?.detail, "observed 2026-09-14T00:00:00Z (6d ago)");
  assert.equal(resp.note, "QUOTA is what the provider reports.");
  assert.deepEqual(resp.caveats, ["1 project record file(s) could not be parsed"]);
});

test("quotaReport: an older daemon that omits the fields yields empty ones, not undefined", async () => {
  // A client that renders `undefined.map` on a field an older daemon does not
  // send is a blank section, which reads as "no limits anywhere" — the exact
  // confusion the section's error line exists to avoid.
  stubFetch({});
  const resp = await quotaReport("T");
  assert.deepEqual(resp.rows, []);
  assert.equal(resp.note, "");
  assert.deepEqual(resp.caveats, []);
});

// --- the rendered section (stubbed document, recovery.test.ts precedent) ---

class ElementStub {
  className = "";
  children: (ElementStub | string)[] = [];
  append(child: ElementStub | string): void { this.children.push(child); }
  setAttribute(): void {}
  prepend(child: ElementStub | string): void { this.children.unshift(child); }
  get textContent(): string {
    return this.children.map((child) => (typeof child === "string" ? child : child.textContent)).join(" ");
  }
}

function stubDocument(t: { after: (fn: () => void) => void }): void {
  const previous = Object.getOwnPropertyDescriptor(globalThis, "document");
  Object.defineProperty(globalThis, "document", {
    configurable: true,
    value: { createElement: () => new ElementStub() },
  });
  t.after(() => {
    if (previous) Object.defineProperty(globalThis, "document", previous);
    else Reflect.deleteProperty(globalThis, "document");
  });
}

function usageState(over: Partial<UsageState> = {}): UsageState {
  return { loaded: true, rows: [], note: "", caveats: [], error: "", ...over };
}

const parkedRow: QuotaRow = {
  agent: "codex",
  quota: "not reported",
  observed: "limit reached",
  detail: "1 of 2 session(s) parked at a usage limit; observed 2026-09-14T00:00:00Z (6d ago)",
};

test("the section renders the daemon's rows verbatim, including when the wall was observed", (t) => {
  stubDocument(t);
  const el = renderUsageSection(usageState({ rows: [parkedRow], note: "QUOTA is what the provider reports." })) as unknown as ElementStub;
  const text = el.textContent;
  assert.match(text, /Usage/);
  assert.match(text, /codex/);
  assert.match(text, /not reported · limit reached/);
  // The staleness is the feature: WHEN af saw it must be on screen.
  assert.match(text, /6d ago/);
  assert.match(text, /QUOTA is what the provider reports\./);
});

test("a caveat reaches the operator verbatim", (t) => {
  stubDocument(t);
  const el = renderUsageSection(usageState({
    rows: [parkedRow],
    caveats: ["1 project record file(s) could not be parsed and were skipped, so this report is INCOMPLETE"],
  })) as unknown as ElementStub;
  assert.match(el.textContent, /INCOMPLETE/);
});

test("a failed read is a failure line, not an empty section", (t) => {
  stubDocument(t);
  const el = renderUsageSection(usageState({ error: "cannot reach the daemon" })) as unknown as ElementStub;
  const text = el.textContent;
  assert.match(text, /could not be read/);
  assert.match(text, /cannot reach the daemon/);
  assert.doesNotMatch(text, /no limit seen|not reported/, "a failed read must not render rows");
});

test("before the first answer the section says it is loading", (t) => {
  stubDocument(t);
  const el = renderUsageSection(emptyUsageState()) as unknown as ElementStub;
  assert.match(el.textContent, /Loading usage/);
});

test("an empty report says so plainly rather than rendering nothing", (t) => {
  stubDocument(t);
  const el = renderUsageSection(usageState()) as unknown as ElementStub;
  assert.match(el.textContent, /nothing to report/);
});
