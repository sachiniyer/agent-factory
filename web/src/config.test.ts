import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { getConfig, setConfigValue } from "./api.js";
import { controlKind } from "./config.js";
import type { ConfigEntry } from "./types.js";

// These are the web client's config-editor contracts. They are pure logic +
// stubbed fetch, matching the rest of web/src/*.test.ts (no DOM, no jsdom): the
// rendered form is proven in the Playwright selftest instead.

interface Captured {
  url: string;
  body: Record<string, unknown>;
  auth: string | undefined;
  calls: number;
}

/** Stubs fetch with a canned envelope and captures what was sent. */
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

const entry = (over: Partial<ConfigEntry> = {}): ConfigEntry => ({
  key: "default_program",
  type: "string",
  default: "claude",
  purpose: "The agent a new session runs.",
  tier: 1,
  tier_name: "core",
  settable: true,
  value: "claude",
  requires_restart: true,
  ...over,
});

test("getConfig reads the manifest from the daemon, not from a local key list", async () => {
  const cap = stubFetch({ entries: [entry()], path: "/home/u/.agent-factory/config.toml" });
  const resp = await getConfig("tok");

  assert.equal(cap.url, "/v1/GetConfig");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(resp.entries.length, 1);
  assert.equal(resp.entries[0].key, "default_program");
  // The path is carried so the UI can name the file it is editing rather than
  // leaving an AF_HOME user guessing.
  assert.equal(resp.path, "/home/u/.agent-factory/config.toml");
});

test("getConfig reports no entries as an empty list, never null", async () => {
  stubFetch({ entries: null, path: "" });
  const resp = await getConfig("tok");
  assert.deepEqual(resp.entries, [], "callers iterate this; null would throw at render");
});

test("setConfigValue posts the key and the RAW value for the daemon to validate", async () => {
  const cap = stubFetch({
    result: { key: "update_channel", value: "preview", path: "/tmp/config.toml", requires_restart: true },
    restart_notice: "af and the daemon read config.toml at startup · run `af daemon restart` and restart af to apply",
  });
  const resp = await setConfigValue("update_channel", "preview", "tok");

  assert.equal(cap.url, "/v1/SetConfigValue");
  assert.deepEqual(cap.body, { key: "update_channel", value: "preview" });
  // The echo is the CANONICAL value the writer reported — the UI shows this
  // rather than what it sent, which is the same contract `af config set` has.
  assert.equal(resp.result.value, "preview");
  assert.equal(resp.result.requires_restart, true);
  assert.match(resp.restart_notice, /af daemon restart/, "the notice must name the command to run");
});

test("setConfigValue surfaces the validator's own message on a rejected value", async () => {
  // The daemon returns the validator's error verbatim; a 500 is how this API
  // reports a handler error (daemon/httpserver.go). The browser must not rewrite
  // it: a second copy of the rules in the UI is how a form comes to accept a
  // value the loader rejects at startup.
  stubFetch(null, { ok: false, status: 500, error: 'update_channel must be one of [stable, preview], got "nightly"' });

  await assert.rejects(
    () => setConfigValue("update_channel", "nightly", "tok"),
    (err: Error) => {
      assert.match(err.message, /update_channel must be one of/);
      return true;
    },
    "an invalid value must reject with the validator's message, not a generic failure",
  );
});

test("setConfigValue sends no Authorization header for the tokenless credential", async () => {
  // "" is the authorized-tokenless sentinel (#1696); null-vs-"" matters here.
  const cap = stubFetch({
    result: { key: "auto_yes", value: "true", path: "/tmp/config.toml", requires_restart: true },
    restart_notice: "",
  });
  await setConfigValue("auto_yes", "true", "");
  assert.equal(cap.auth, undefined);
});

// The web half of the anti-drift guarantee.
//
// The Go side pins that every config_types.go key has a manifest entry
// (TestManifestCoversEveryConfigKey) and that the TUI renders every entry
// (TestConfigPaneRendersEveryManifestKey). This is the third link: the web form's
// control choice is driven ENTIRELY by the manifest's own description of a key —
// its `type`, `enum`, and `settable` — so an unfamiliar key still renders a
// sensible control instead of being dropped or crashing the view.
//
// That is what lets a key added to config_types.go reach this surface with no
// edit to config.ts. A hand-written form would need a new branch per key, which
// is exactly the drift the manifest exists to kill.
test("the control for a key is decided by the manifest, so an unknown key still renders", () => {
  // controlKind is the REAL function renderControl dispatches on — imported, not
  // reimplemented here. A local copy of the rules would drift from the pane and
  // pass while the form was broken, which is the exact failure mode this whole
  // feature is designed against.
  const controlFor = controlKind;

  assert.equal(controlFor(entry({ type: "bool", value: "true" })), "checkbox");
  assert.equal(controlFor(entry({ type: "string", enum: ["stable", "preview"] })), "select");
  assert.equal(controlFor(entry({ type: "string" })), "text");
  assert.equal(controlFor(entry({ type: "int" })), "text");
  // A hand-edited key is never offered as a field: the write would be refused,
  // so an editable-looking control would be a dead end.
  assert.equal(controlFor(entry({ settable: false, type: "table" })), "readonly");
  // For a table the enum constrains entry NAMES, not the value — offering it as
  // a value picker would be a small lie about what the key takes.
  assert.equal(controlFor(entry({ type: "table", enum: ["claude", "codex"], settable: true })), "text");
  // A key type this bundle has never heard of still gets a usable field rather
  // than disappearing from the form.
  assert.equal(controlFor(entry({ type: "duration-we-have-never-seen" })), "text");
});
