import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { getConfig, setConfigValue } from "./api.js";
import { type ConfigStatus, canCommit, controlKind, createKeyedQueue, saveNotice, shouldCloseSavedField } from "./config.js";
import type { ConfigEntry, ConfigSetResponse } from "./types.js";

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
    // The per-key effect notice (#2480): update_channel is client-side, so the
    // daemon returns the "next af launch" sentence — never one canned banner.
    restart_notice: "Saved — this setting takes effect the next time you launch af.",
  });
  const resp = await setConfigValue("update_channel", "preview", "tok");

  assert.equal(cap.url, "/v1/SetConfigValue");
  assert.deepEqual(cap.body, { key: "update_channel", value: "preview" });
  // The echo is the CANONICAL value the writer reported — the UI shows this
  // rather than what it sent, which is the same contract `af config set` has.
  assert.equal(resp.result.value, "preview");
  assert.equal(resp.result.requires_restart, true);
  // Since #2480 the daemon applies the write in place and returns a per-key notice,
  // so it must NOT drop the user to a shell to run a command (#2479); the client
  // passes it through verbatim from the daemon.
  assert.doesNotMatch(resp.restart_notice, /daemon restart|run `/, "the notice must not tell the user to run a command (#2479)");
});

// --- saveNotice: what the form says after a write (#2480 notice, #3722 address) ---

const setResp = (over: Partial<ConfigSetResponse> = {}): ConfigSetResponse => ({
  result: { key: "network.listen_addr", value: "127.0.0.1:9443", path: "/tmp/config.toml", requires_restart: false },
  restart_notice: "Applied — the running daemon is using the new value now.",
  ...over,
});

test("saveNotice names where the daemon is listening after a listener move", () => {
  // The whole point of #3722 on this surface: saving network.listen_addr moves the
  // listener this page is talking over, so the address it moved TO has to reach the
  // operator — the next request from this page goes nowhere.
  const notice = saveNotice(setResp({ listener_addr: "127.0.0.1:9443" }));
  assert.match(notice, /Daemon now listening at 127\.0\.0\.1:9443/);
});

test("saveNotice reports the address the DAEMON named, not the value that was sent", () => {
  // A rebind that failed leaves the daemon on its previous address and says so.
  // Echoing the typed value here would name an address nothing answers, in the one
  // case where the operator most needs the real one.
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: "127.0.0.1:9443", path: "/tmp/config.toml", requires_restart: false },
      restart_notice: "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).",
      listener_addr: "127.0.0.1:8443",
    }),
  );
  assert.match(notice, /Daemon now listening at 127\.0\.0\.1:8443/);
  assert.doesNotMatch(notice, /9443/, "the form must not echo the requested address as if it were bound");
});

test("saveNotice says nothing for an ordinary key that needs no restart", () => {
  assert.equal(
    saveNotice(
      setResp({
        result: { key: "default_program", value: "codex", path: "/tmp/config.toml", requires_restart: false },
      }),
    ),
    "",
    "an \"Applied\" line under every field is noise — that gate is unchanged",
  );
});

test("saveNotice keeps the restart notice for a key that requires one", () => {
  const notice = saveNotice(
    setResp({
      result: { key: "update_channel", value: "preview", path: "/tmp/config.toml", requires_restart: true },
      restart_notice: "Saved — this setting takes effect the next time you launch af.",
    }),
  );
  assert.equal(notice, "Saved — this setting takes effect the next time you launch af.");
});

test("saveNotice surfaces a failed live apply and its error", () => {
  const restartNotice =
    "Saved — the running daemon could not apply the new configuration and is still using its previous value. Resolve the warning, then retry the save or restart the daemon before relying on the saved value.";
  const warning =
    "saved config, but live apply failed: reload config: expected a top-level item to end with a newline";
  const writeWarning = "saved value exposes a tokenless network listener";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false },
      restart_notice: restartNotice,
      warnings: [writeWarning, warning],
      apply_outcome: "failed",
    }),
  );
  assert.equal(notice, `${restartNotice} · ${writeWarning} · ${warning}`);
});

test("saveNotice surfaces a write-only warning the daemon applied silently (default_accounts)", () => {
  // The two-channel contract: a `default_accounts.<agent>` write is
  // EffectAppliedLive, so the daemon applies it without an apply-side warning —
  // channel 2 (`resp.warnings`) is empty/absent — while channel 1
  // (`resp.result.warnings`) carries the unregistered-account advisory the
  // config writer authored at write time. The form MUST read channel 1 or the
  // operator is never told, at save time, that the account they defaulted to is
  // unregistered and how to fix it. The CLI reads channel 1 unconditionally; the
  // TUI falls back to it when channel 2 is empty; this is the web's equivalent.
  const warning =
    'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const notice = saveNotice(
    setResp({
      result: { key: "default_accounts.claude", value: "missing", path: "/tmp/config.toml", requires_restart: false, warnings: [warning] },
      restart_notice: "Applied — the running daemon is using the new value now.",
      // channel 2 absent: apply is silent for an applied default_accounts write
    }),
  );
  assert.equal(notice, `Applied — the running daemon is using the new value now. · ${warning}`);
});

test("saveNotice surfaces a write-only warning for an ordinarily-live key with no restart notice", () => {
  // The same write-only warning, but with a daemon that omits the restart notice
  // (e.g. a pre-#2480-era reply shape, or an agent that does not emit one). The
  // warning still renders on its own — losing it here would be the silent
  // regression that prompted reading channel 1 in the first place.
  const warning =
    'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const notice = saveNotice(
    setResp({
      result: { key: "default_accounts.claude", value: "missing", path: "/tmp/config.toml", requires_restart: false, warnings: [warning] },
      restart_notice: "",
    }),
  );
  // No restart notice is emitted, but the per-write warning still shows.
  assert.equal(notice, warning);
});

test("saveNotice reads the deduped union when both channels are populated", () => {
  // Sub-case (c) from the report: channel 2 is NON-empty (a rebind-failure
  // warning) and channel 1 additionally carries a per-write advisory. A
  // `??`-style fallback would lose the channel-1 notice here because channel 2
  // is non-empty; only the deduped union (channel 1 first, then channel 2
  // minus overlaps) surfaces both, matching the CLI which reads channel 1
  // unconditionally then channel 2 deduped against it.
  const writeWarning =
    'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const applyWarning = "rebind failed: address already in use";
  const restartNotice = "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: "0.0.0.0:9443", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
      apply_outcome: "failed",
    }),
  );
  // Channel 1 (per-write) first, then channel 2 (apply-time) — order matches the CLI.
  assert.equal(notice, `${restartNotice} · ${writeWarning} · ${applyWarning}`);
});

test("saveNotice de-dupes a warning that rides both channels", () => {
  // The tokenless-network-listener exposure case: the writer and the applier
  // both describe the same hazard in different words; the daemon's
  // completeConfigSaveWarnings keeps the authoritative apply-time copy on
  // channel 2, and the writer's copy rides channel 1. A naive concat would
  // show the hazard twice; the union's filter drops the overlap so it prints
  // once. The dedup is by exact string match, so when the two surfaces phrase
  // the warning identically it is collapsed; when they diverge both show.
  const warning = "saved value exposes a tokenless network listener";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [warning] },
      restart_notice: "Applied — the running daemon is using the new value now.",
      warnings: [warning],
    }),
  );
  assert.equal(notice, `Applied — the running daemon is using the new value now. · ${warning}`);
});

test("saveNotice preserves channel order with multiple per-write and multiple apply warnings", () => {
  // More than one warning per channel: channel 1 in its authored order, then
  // channel 2 in its authored order, minus overlaps. Pins the ordering the
  // union uses so a future edit cannot silently reverse the two channels.
  const w1 = 'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const w2 = "saved value exposes a tokenless network listener";
  const w3 = "rebind failed: address already in use";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: "0.0.0.0:9443", path: "/tmp/config.toml", requires_restart: false, warnings: [w1, w2] },
      restart_notice: "Saved — could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).",
      warnings: [w2, w3],
    }),
  );
  // w2 appears on both channels; printed once (from channel 1). w3 is channel-2-only.
  assert.equal(notice, `Saved — could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason). · ${w1} · ${w2} · ${w3}`);
});

test("saveNotice tolerates an older daemon that sends no address", () => {
  // listener_addr is additive: a daemon predating #3722 omits it entirely, and the
  // form must not render "undefined" at the user.
  const notice = saveNotice(setResp({ result: { key: "update_channel", value: "preview", path: "/tmp/config.toml", requires_restart: true } }));
  assert.doesNotMatch(notice, /undefined/);
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
    result: { key: "auto_update", value: "true", path: "/tmp/config.toml", requires_restart: true },
    restart_notice: "",
  });
  await setConfigValue("auto_update", "true", "");
  assert.equal(cap.auth, undefined);
});

// The web half of the anti-drift guarantee.
//
// The Go side pins that every config_types.go key has a manifest entry
// (TestManifestCoversEveryConfigKey) and that the TUI renders every entry
// (TestConfigPaneRendersEveryManifestKey). This is the third link: the web form's
// control choice is driven ENTIRELY by the manifest's own description of a key —
// its `type` and `enum` — so an unfamiliar key still renders a
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
  // Structured keys use a text field for the compact JSON value supplied by the
  // manifest and accepted by the same config-set writer.
  assert.equal(controlFor(entry({ type: "table" })), "text");
  assert.equal(
    controlFor({ ...entry({ type: "table" }), editable: false } as ConfigEntry & { editable: boolean }),
    "text",
    "the retired wire marker must not recreate a read-only control during version skew",
  );
  // A dynamic family keeps the same whole-value JSON control even though its
  // entry-name enum is metadata rather than a picker value list.
  assert.equal(
    controlFor(entry({ key: "program_overrides", type: "table", enum: ["claude", "codex"] })),
    "text",
    "a dynamic table is editable as one validated JSON value",
  );
  // A key type this bundle has never heard of still gets a usable field rather
  // than disappearing from the form.
  assert.equal(controlFor(entry({ type: "duration-we-have-never-seen" })), "text");
});

// Both ways to commit a field — the Save button and the Enter key — gate on this
// one predicate. They used to disagree: the button honored `disabled`, Enter did
// not, so pressing Enter on an untouched field wrote it anyway and printed an
// echo plus a restart notice for a no-op.
test("an unchanged value is not worth writing — the gate both Save and Enter use", () => {
  assert.equal(canCommit("claude", "claude"), false, "an untouched field must not write");
  assert.equal(canCommit("codex", "claude"), true);
  // An emptied field IS a change: "" is a real value (an unset vscode binary,
  // listen_addr = "" to turn the web server off), not a no-op.
  assert.equal(canCommit("", "claude"), true, "clearing a value is an edit, not a no-op");
  assert.equal(canCommit("", ""), false, "but an already-empty field is still untouched");
});

// The write race. Two saves of the same key are ordered by the network, not by
// the user — so a checkbox toggled on→off→on can land "off". Every write
// succeeds and none is clobbered; the answer is simply not what the user chose
// last. The daemon's file lock cannot help: it serializes ACCESS, not INTENT.
test("saves for one key run in the order the user made them, however slow the network", async () => {
  const started: string[] = [];
  const finished: string[] = [];
  const queue = createKeyedQueue();

  // The first save is slow, the second fast — unqueued, the second would finish
  // first and lose.
  const delays: Record<string, number> = { off: 30, on: 0 };
  const save = (v: string) => async () => {
    started.push(v);
    await new Promise((r) => setTimeout(r, delays[v]));
    finished.push(v);
  };

  queue("auto_update", save("off"));
  const done = queue("auto_update", save("on"));
  await done;

  assert.deepEqual(started, ["off", "on"], "the second save must not start until the first settles");
  assert.deepEqual(finished, ["off", "on"], "the LAST value the user chose must be the last one written");
});

test("a rejected save does not wedge the queue for that key", async () => {
  const finished: string[] = [];
  const queue = createKeyedQueue();

  queue("update_channel", async () => {
    throw new Error('update_channel must be one of [stable, preview], got "nightly"');
  });
  const done = queue("update_channel", async () => {
    finished.push("preview");
  });
  await done;

  assert.deepEqual(finished, ["preview"], "a refused value must not block the user's next attempt on that key");
});

test("different keys are not queued behind each other", async () => {
  const finished: string[] = [];
  const queue = createKeyedQueue();

  // A slow save on one key must not hold up an unrelated one.
  const slow = queue("listen_addr", async () => {
    await new Promise((r) => setTimeout(r, 40));
    finished.push("listen_addr");
  });
  const fast = queue("auto_update", async () => {
    finished.push("auto_update");
  });
  await Promise.all([slow, fast]);

  assert.deepEqual(finished, ["auto_update", "listen_addr"], "keys have no ordering relationship; they must not block each other");
});

// shouldCloseSavedField (#4383). The close-on-save rule reads a status that the
// store holds as a LEVEL, and the post-save refetch re-delivers it. These pin the
// edge requirement, because the cost of getting it wrong is the user's typing.
const okStatus = (key: string): ConfigStatus => ({ key, value: "v", notice: "", error: "" });

test("a save's success status closes the field it came from", () => {
  assert.equal(
    shouldCloseSavedField(okStatus("network.listen_addr"), "network.listen_addr", true),
    true,
    "the edge that commits a value must close the field, or the user is invited to write it twice",
  );
});

test("the post-save refetch re-delivers the same status and must not close a newer edit", () => {
  const status = okStatus("network.listen_addr");
  // Delivery 1 is the save landing; delivery 2 is refreshConfig() committing fresh
  // entries while configStatus is untouched. The user has typed again by then, so
  // `editing` names the same key — the shape that silently discarded their text.
  assert.equal(shouldCloseSavedField(status, "network.listen_addr", true), true);
  assert.equal(
    shouldCloseSavedField(status, "network.listen_addr", false),
    false,
    "a re-delivered status must not close an edit the user opened after the save",
  );
});

test("a refused value leaves its field open to be corrected", () => {
  const refused: ConfigStatus = { key: "update_channel", value: "", notice: "", error: "must be one of [stable, preview]" };
  assert.equal(
    shouldCloseSavedField(refused, "update_channel", true),
    false,
    "closing on an error would discard the text the validator is complaining about",
  );
});

test("a save for another key never closes the field being edited", () => {
  assert.equal(
    shouldCloseSavedField(okStatus("auto_update"), "network.listen_addr", true),
    false,
    "saves are queued per key and can land in any order; only the field's OWN save closes it",
  );
  assert.equal(shouldCloseSavedField(okStatus("auto_update"), null, true), false);
  assert.equal(shouldCloseSavedField(null, "network.listen_addr", true), false);
});
