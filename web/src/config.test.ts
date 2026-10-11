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
  // unregistered and how to fix it. The TUI reads channel 2 then falls back to
  // channel 1 when it is empty (ui/config_target.go); this is the web's
  // equivalent.
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

test("saveNotice preserves independent channel-1 warnings alongside channel 2", () => {
  // The pending-rebind scenario the second review thread flagged: a
  // `default_accounts.claude = missing` write (channel 1: the
  // unregistered-account advisory) retries a pending listener rebind on apply
  // (channel 2: the rebind failure). The apply is DaemonApplyApplied (the rebind
  // failure is a warning, not a failed apply), so completeConfigSaveWarnings
  // returns ONLY the apply warnings — channel 2 is not the complete set here.
  // A channel-2-only read would lose the per-write advisory, which is the one
  // place that tells the operator the account they defaulted to is unregistered.
  // The advisory is independent of the rebind failure (no apply-side counterpart),
  // so it is preserved alongside channel 2.
  const writeWarning =
    'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const applyWarning = "apply network.listen_addr \"0.0.0.0:9443\": listen tcp 0.0.0.0:9443: bind: address already in use — daemon still serving on 127.0.0.1:8443";
  const restartNotice = "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).";
  const notice = saveNotice(
    setResp({
      result: { key: "default_accounts.claude", value: "missing", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (apply-time rebind failure) then channel 1 (per-write
  // unregistered-account advisory) — both are independent and both surface.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice prefers the authoritative apply warning when both channels describe the same hazard", () => {
  // The tokenless-network-listener exposure case the review flagged: the writer
  // (channel 1, exposureWarning) and the applier (channel 2,
  // ListenerExposureNotice) both describe the same hazard in different words.
  // The server's completeConfigSaveWarnings keeps the authoritative apply-time
  // copy on channel 2 and drops the writer's forecast from it on a successful
  // apply. The form suppresses the forecast on channel 1 when channel 2 already
  // carries the notice (both contain "... is reachable from the network ..."),
  // so the hazard is shown once in the authoritative wording.
  const writeWarning = 'network.listen_addr "0.0.0.0:9443" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run `af config set network.require_token true` to require a token (`af token show` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.';
  const applyWarning = 'network.listen_addr "0.0.0.0:9443" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (`af token show` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only';
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: "Applied — the running daemon is using the new value now.",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (authoritative) only; channel 1's differently-worded forecast
  // is suppressed because channel 2 already carries the exposure notice.
  assert.equal(notice, `Applied — the running daemon is using the new value now. · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast when channel 2 lacks the exposure notice", () => {
  // The exposure forecast (channel 1) is the SAME hazard as the apply-time
  // exposure notice (channel 2), so it is suppressed ONLY when channel 2
  // already carries the notice. When channel 2 has an unrelated apply warning
  // (e.g. a rebind failure on a network.listen_addr write whose rebind failed
  // so no exposure transition occurred) the forecast is the only copy of the
  // exposure warning and must surface — the write still moved the config into an
  // exposed posture on disk even if the listener did not rebind live.
  const writeWarning = 'network.listen_addr "0.0.0.0:9443" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run `af config set network.require_token true` to require a token (`af token show` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.';
  const applyWarning = "apply network.listen_addr \"0.0.0.0:9443\": listen tcp 0.0.0.0:9443: bind: address already in use — daemon still serving on 127.0.0.1:8443";
  const restartNotice = "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: "0.0.0.0:9443", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (rebind failure) then channel 1 (exposure forecast) — the
  // forecast is not a duplicate because channel 2 does not carry the exposure
  // notice (no transition occurred on the failed rebind).
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice preserves channel 2 order with multiple apply warnings and appends independent channel-1 notes", () => {
  // Channel 2 carries the authoritative set; the form reads it in the order the
  // daemon produced. Channel 1's independent per-write notes are appended after
  // channel 2 — they have no apply-side counterpart and must not be lost.
  const w2 = "apply network.listen_addr \"0.0.0.0:9443\": listen tcp 0.0.0.0:9443: bind: address already in use — daemon still serving on 127.0.0.1:8443";
  const w3 = "network.require_token is now false: revoked 2 sandbox callback credential(s), because a scoped credential enforces nothing against a listener that authenticates nobody.";
  const w1 = 'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const notice = saveNotice(
    setResp({
      result: { key: "default_accounts.claude", value: "missing", path: "/tmp/config.toml", requires_restart: false, warnings: [w1] },
      restart_notice: "Saved — could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).",
      warnings: [w2, w3],
    }),
  );
  // Channel 2 in authored order, then channel 1's independent advisory.
  assert.equal(notice, `Saved — could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason). · ${w2} · ${w3} · ${w1}`);
});

test("saveNotice does not duplicate a per-write advisory the server already merged into channel 2", () => {
  // On a failed or unconfirmed apply the server's completeConfigSaveWarnings
  // copies every result.warning into resp.warnings before returning, so a
  // non-exposure advisory (e.g. an unregistered-account warning on a
  // default_accounts write) that the write produced is already in channel 2
  // verbatim. The form must not append channel 1's copy on top of it — the
  // exposure-only filter would, rendering the advisory twice. A write warning
  // already present in channel 2 is dropped verbatim.
  const advisory =
    'no claude account named "missing" is registered on this machine, so a session using this default will be refused rather than started. Register it with `af accounts add claude missing` and log in';
  const applyError =
    "saved config, but live apply failed: reload config: expected a top-level item to end with a newline";
  const restartNotice =
    "Saved — the running daemon could not apply the new configuration and is still using its previous value. Resolve the warning, then retry the save or restart the daemon before relying on the saved value.";
  const notice = saveNotice(
    setResp({
      result: { key: "default_accounts.claude", value: "missing", path: "/tmp/config.toml", requires_restart: false, warnings: [advisory] },
      restart_notice: restartNotice,
      // completeConfigSaveWarnings merged the advisory into channel 2 on the
      // failed apply, so it appears here verbatim alongside the apply error.
      warnings: [advisory, applyError],
      apply_outcome: "failed",
    }),
  );
  assert.equal(notice, `${restartNotice} · ${advisory} · ${applyError}`);
});

test("saveNotice keeps a preview exposure forecast when channel 2 exposes a different listener", () => {
  // The exposure dedup matches the listener, not the phrase: channel 2 can carry
  // the control-plane exposure notice (network.listen_addr) while channel 1
  // carries a failed preview bind's on-disk exposure forecast
  // (network.preview_listen_addr). Both contain "is reachable from the
  // network", but a notice for one listener is not an authoritative copy of a
  // forecast for the other — the two listeners are independent — so the preview
  // forecast is kept.
  const previewForecast = 'network.preview_listen_addr "0.0.0.0:9444" is reachable from the network · it is the web-tab preview origin, kept separate from network.listen_addr so it never serves the daemon control API · it serves your previewed dev servers, each on its own hostname · a remote browser gains nothing from the bind: per-tab origins are *.localhost names, which a browser resolves to its own machine, so remote viewers keep the same-origin preview on network.listen_addr · but *.localhost is a browser convention, not a restriction — anything that reaches this address can send Host: <tab>.localhost itself, and a tab\'s hostname is the only credential this listener checks, so a hostname that leaks through a log, a screenshot, or browser history becomes readable from the network instead of only from this machine · editor tabs are withheld entirely while this is network-bound · set it to a loopback address such as 127.0.0.1:9444, or "" to disable it';
  const listenNotice = 'network.listen_addr "0.0.0.0:9443" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (`af token show` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only';
  const notice = saveNotice(
    setResp({
      result: { key: "network.preview_listen_addr", value: "0.0.0.0:9444", path: "/tmp/config.toml", requires_restart: false, warnings: [previewForecast] },
      restart_notice: "Applied — the running daemon is using the new value now.",
      warnings: [listenNotice],
    }),
  );
  assert.equal(notice, `Applied — the running daemon is using the new value now. · ${listenNotice} · ${previewForecast}`);
});

test("saveNotice keeps the exposure forecast when channel 2 names a different address for the same listener", () => {
  // A failed rebind can leave the serving address (channel 2,
  // ListenerExposureNotice — the bound address the daemon is actually on) different
  // from the configured address (channel 1, exposureWarning — the on-disk value that
  // will be exposed after restart). The daemon's apply notice deliberately names the
  // serving bound address, not the requested one (pinned by
  // TestApplyConfigExposureNoticePerKeyOrderNamesServingAddress). The two notices
  // share the same listener key but describe different addresses, so the dedup
  // identity includes the address: a channel-2 notice for the serving address is not
  // an authoritative copy of a channel-1 forecast for the configured address the
  // listener has not yet bound, and the future-state forecast is preserved.
  const servingAddr = "192.0.2.1:9443";
  const configuredAddr = "0.0.0.0:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (serving address) then channel 1 (configured address) — the
  // forecast names a different address the listener has not bound, so it is
  // not a duplicate and both surface.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a successful ephemeral rebind (configured :0 vs concrete serving port)", () => {
  // When a listener is saved with the supported address 0.0.0.0:0, a successful
  // rebind resolves the ephemeral port to a concrete kernel-assigned port. The
  // per-write exposureWarning (channel 1) names the configured address (:0) while
  // the apply-time ListenerExposureNotice (channel 2) names the concrete serving
  // address the kernel assigned (e.g. 0.0.0.0:54321) — the daemon test at
  // daemon/config_apply_preview_exposure_test.go:298-313 pins this wire shape.
  // The :0 is the ephemeral wildcard and the concrete port is what it resolved to
  // on the same rebind, so the two are the same exposure; the :0 forecast is
  // suppressed and only the authoritative channel-2 notice (with the address the
  // daemon is actually reachable on) surfaces. The rebind succeeded
  // (apply_outcome "applied"), so the resolved-port relaxation fires.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "0.0.0.0:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address) only; the :0 forecast is the same
  // exposure and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast for a :0 configured address when channel 2 names a different host", () => {
  // The :0 relaxation matches only the SAME listener and host: a failed rebind
  // can leave the serving address on a different host while config carries a :0
  // the listener never bound. In that case the forecast (:0 on the configured
  // host) describes a future state the serving notice (a different host) does not
  // cover, so both surface — the :0 wildcard does not collapse across hosts.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "127.0.0.1:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Both surface — the :0 forecast is for 0.0.0.0 and the serving notice is for
  // 127.0.0.1, so the host differs and the dedup does not fire.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a dual-stack ephemeral rebind (configured 0.0.0.0:0 vs serving [::]:<port>)", () => {
  // On a dual-stack host, net.Listen("tcp", "0.0.0.0:0") can report its bound
  // address as [::]:<port> (the kernel picks the IPv6 wildcard). Channel 1
  // (exposureWarning) names the configured address 0.0.0.0:0 while channel 2
  // (ListenerExposureNotice) names the serving bound address [::]:<port>. The two
  // describe the same all-interfaces bind, but the raw host comparison would not
  // match (0.0.0.0 vs ::). normalizeListenHost canonicalizes both wildcards so
  // the dedup fires and only the authoritative channel-2 notice surfaces. The
  // rebind succeeded (apply_outcome "applied"), so the resolved-port relaxation
  // (the :0 forecast matches the concrete serving port) fires as well.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "[::]:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address on [::], the IPv6 wildcard) only;
  // the 0.0.0.0:0 forecast is the same all-interfaces bind and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast when channel 2 names a different host for the same :0 port", () => {
  // The wildcard normalization matches 0.0.0.0 and [::] (both all-interfaces), but
  // NOT a concrete non-wildcard host: a forecast for 0.0.0.0:0 and a serving
  // notice for 127.0.0.1:54321 describe different listeners, so both surface.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "127.0.0.1:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a dual-stack non-ephemeral rebind (configured 0.0.0.0:8443 vs serving [::]:8443)", () => {
  // The same dual-stack normalization applies when the port is concrete: a
  // configured 0.0.0.0:8443 that binds as [::]:8443 on a dual-stack host is the
  // same all-interfaces bind at the same port, so the forecast is suppressed and
  // only the authoritative channel-2 notice surfaces. The cross-family wildcard
  // equivalence (:: -> 0.0.0.0) is gated on a successful rebind
  // (bindSucceeded): the saved key is network.listen_addr and apply_outcome is
  // "applied", so the daemon bound the configured 0.0.0.0 and the serving [::] is
  // the same bind on a dual-stack host. On a host with IPV6_V6ONLY the two
  // families expose different interfaces, so the gate is what keeps the
  // non-rebind case (the next test) from collapsing them.
  const configuredAddr = "0.0.0.0:8443";
  const servingAddr = "[::]:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the dual-stack exposure forecast when the rebind status is unknown (no apply_outcome, require_token flip)", () => {
  // The cross-family wildcard equivalence (:: -> 0.0.0.0) is gated on
  // bindSucceeded: on a host with IPV6_V6ONLY, [::]:8443 and 0.0.0.0:8443 expose
  // different address families. This response carries no apply_outcome (an older
  // daemon that predates the field), so the per-listener rebind status is unknown
  // and the relaxation does not fire. The browser cannot confirm the configured
  // 0.0.0.0 and the serving [::] are the same bind. The strict host comparison
  // keeps the IPv4 forecast (0.0.0.0:8443) and the IPv6 serving notice
  // ([::]:8443) distinct, so both surface.
  const configuredAddr = "0.0.0.0:8443";
  const servingAddr = "[::]:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a service-name configured port against a numeric serving port", () => {
  // The validator (config/listen_addr.go) accepts service-name ports via
  // net.LookupPort ("tcp", "http") → 80, so a configured 0.0.0.0:http binds port
  // 80. net.Listen resolves the service name the same way, so the serving bound
  // address channel 2 reports is 0.0.0.0:80 or [::]:80, while channel 1's
  // exposureWarning names the configured 0.0.0.0:http. Without service-name
  // resolution in normalizeListenPort the two would not compare equal and the
  // same exposure would render twice. normalizeListenPort resolves well-known
  // service names to their numeric ports (matching net.LookupPort) on a
  // successful rebind (bindSucceeded), so the forecast is suppressed and only
  // the authoritative channel-2 notice surfaces. The cross-family wildcard
  // equivalence (:: -> 0.0.0.0) is also gated on a successful rebind
  // (bindSucceeded), so the rebind must have landed (apply_outcome "applied")
  // for the configured 0.0.0.0 and the serving [::] to compare equal — on a host
  // with IPV6_V6ONLY they expose different families.
  const configuredAddr = "0.0.0.0:http";
  const servingAddr = "[::]:80";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address on port 80) only; the 0.0.0.0:http
  // forecast is the same exposure and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast when a service-name port resolves to a different number than the serving port", () => {
  // normalizeListenPort resolves service names only on a successful rebind
  // (bindSucceeded); on a non-listener save (network.require_token, not a
  // rebind) the table is not authoritative — Go's net.LookupPort augments its
  // built-in mappings from the daemon host's /etc/services, so the browser-side
  // table could resolve a name to a different port than the host. In this case
  // 0.0.0.0:https (port 443) and a serving address on port 8443 are different
  // binds, so both surface.
  const configuredAddr = "0.0.0.0:https";
  const servingAddr = "0.0.0.0:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a successful hostname rebind (configured hostname vs serving IP)", () => {
  // When the saved key is a listener (network.listen_addr) and the rebind
  // succeeded (apply_outcome "applied"), the configured hostname resolved to
  // the serving IP at bind time. Channel 1 (exposureWarning) names the configured
  // hostname while channel 2 (ListenerExposureNotice) names the kernel-resolved
  // IP from net.Addr().String(). The browser cannot reproduce DNS, so rather than
  // comparing the hostname against the IP textually, the dedup treats them as the
  // same bind when the rebind succeeded (the saved key is a listener key and
  // apply_outcome is "applied") and the configured host is a non-literal hostname
  // while the serving host is a literal IP — keyed by listener and port alone.
  // The same bind on the same listener and port is the one hazard, and the
  // authoritative channel-2 notice (with the dialable address) surfaces.
  const configuredAddr = "myhost:9443";
  const servingAddr = "192.0.2.1:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the serving IP) only; the hostname forecast is the same bind and
  // is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates a successful hostname rebind for the preview listener", () => {
  // The hostname relaxation applies to network.preview_listen_addr as well: a
  // successful rebind resolves the configured hostname to the serving IP, and the
  // preview listener's per-write forecast (PreviewListenerExposureNotice on
  // channel 1) names the configured hostname while the apply-time transition
  // notice (channel 2) names the resolved IP. The dedup matches them by listener
  // key and port when the rebind succeeded and the configured host is a
  // non-literal hostname.
  const configuredAddr = "myhost:9444";
  const servingAddr = "192.0.2.1:9444";
  const writeWarning = `network.preview_listen_addr "${configuredAddr}" is reachable from the network · it is the web-tab preview origin, kept separate from network.listen_addr so it never serves the daemon control API · it serves your previewed dev servers, each on its own hostname · a remote browser gains nothing from the bind: per-tab origins are *.localhost names, which a browser resolves to its own machine, so remote viewers keep the same-origin preview on network.listen_addr · but *.localhost is a browser convention, not a restriction — anything that reaches this address can send Host: <tab>.localhost itself, and a tab\'s hostname is the only credential this listener checks, so a hostname that leaks through a log, a screenshot, or browser history becomes readable from the network instead of only from this machine · editor tabs are withheld entirely while this is network-bound · set it to a loopback address such as 127.0.0.1:9444, or "" to disable it`;
  const applyWarning = `network.preview_listen_addr "${servingAddr}" is reachable from the network · it is the web-tab preview origin, kept separate from network.listen_addr so it never serves the daemon control API · it serves your previewed dev servers, each on its own hostname · a remote browser gains nothing from the bind: per-tab origins are *.localhost names, which a browser resolves to its own machine, so remote viewers keep the same-origin preview on network.listen_addr · but *.localhost is a browser convention, not a restriction — anything that reaches this address can send Host: <tab>.localhost itself, and a tab\'s hostname is the only credential this listener checks, so a hostname that leaks through a log, a screenshot, or browser history becomes readable from the network instead of only from this machine · editor tabs are withheld entirely while this is network-bound · set it to a loopback address such as 127.0.0.1:9444, or "" to disable it`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.preview_listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the serving IP) only; the hostname forecast is the same bind and
  // is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates a hostname forecast on a non-listener save with no rebind failure warning", () => {
  // A non-listener save (network.require_token) with apply_outcome "applied" and
  // no rebind failure warning in channel 2 means the listener is at the
  // configured address — either no rebind was needed (the listener was already at
  // the configured address) or ApplyConfig retried a pending rebind that
  // succeeded (listener_rebind.go reconciles every save, so a bind that failed
  // on the listener save can succeed on the require_token flip that retried it).
  // In both cases the serving address (channel 2) IS the resolved form of the
  // configured hostname (channel 1), so the hostname relaxation (a non-literal
  // configured host matched to the literal serving IP) is safe: the rebind
  // failure warning in channel 2 is absent, so listenerRebindFailedFor returns
  // false and the rebind is recognized as succeeded. The per-key apply_outcome is
  // "applied" for the saved key (network.require_token applies live) and does
  // not reflect the listener rebind, whose outcome the daemon reports only via a
  // rebind failure warning in channel 2; the absence of that warning is the
  // signal that the rebind did not fail.
  const configuredAddr = "myhost:9443";
  const servingAddr = "192.0.2.1:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the serving IP the hostname resolved to) only; the channel-1
  // forecast for the configured hostname is the same bind (the rebind either
  // succeeded or was not needed, so the serving address is the resolved form of
  // the configured hostname) and is suppressed by the hostname relaxation.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast for a hostname when a non-listener save's rebind failed", () => {
  // A non-listener save (network.require_token) can retry a pending listener
  // rebind that fails again: ApplyConfig reconciles every save, so a bind that
  // failed on the listener save is retried on the require_token flip. When the
  // rebind fails again, the rebind failure warning ("apply network.listen_addr
  // ... — daemon still serving on the previous address ...") is in channel 2, so
  // listenerRebindFailedFor returns true and the rebind is NOT recognized as
  // succeeded — the serving address (channel 2) is the OLD bound address, not
  // the resolved form of the configured hostname, so the hostname relaxation
  // (a non-literal configured host matched to the literal serving IP) is unsafe
  // and both the forecast and the notice surface. This preserves the
  // failed-rebind case (finding #5:
  // TestApplyConfigExposureNoticePerKeyOrderNamesServingAddress pins the
  // serving-vs-configured split) for a hostname the same way it does for a
  // configured IP.
  const configuredAddr = "myhost:9443";
  const servingAddr = "192.0.2.1:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const rebindFailure = `apply network.listen_addr "${configuredAddr}": listen tcp ${configuredAddr}: bind: address already in use — daemon still serving on the previous address "${servingAddr}"`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [rebindFailure, applyWarning],
    }),
  );
  // Both the rebind failure warning (channel 2) and the exposure notice (channel
  // 2) surface before the forecast (channel 1): the rebind failure warning
  // confirms the listener is still serving the old address, so the hostname
  // relaxation is not applied and the configured hostname forecast is distinct
  // from the serving IP notice.
  assert.equal(notice, `${restartNotice} · ${rebindFailure} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice keeps the exposure forecast for a hostname when the rebind failed (deferred)", () => {
  // A failed rebind is "deferred" (config/effect.go saveRules:
  // ListenerRebindFailed → ApplyStatusDeferred), not "applied", so the per-key
  // apply_outcome the dedup gates on is not "applied" and the hostname relaxation
  // does not fire. The configured hostname did not bind (the listener is still
  // serving the old address), so the serving address (channel 2) is not the
  // resolved form of the configured hostname — the two are different hazards, and
  // the strict host comparison is kept so the forecast for the configured address
  // the listener has not yet bound is preserved.
  const configuredAddr = "myhost:9443";
  const servingAddr = "192.0.2.1:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "deferred",
      warnings: [applyWarning],
    }),
  );
  // Both surface — the rebind failed (apply_outcome "deferred"), so the hostname
  // relaxation is gated off and the strict host comparison is kept.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice keeps the :0 exposure forecast when the rebind failed (deferred)", () => {
  // The :0 resolved-port relaxation is gated on a successful rebind
  // (bindSucceeded): a failed rebind (apply_outcome "deferred") leaves the
  // listener serving the OLD address while config carries the configured :0
  // that will bind a NEW ephemeral port after restart. Channel 2 warns about the
  // currently serving concrete port, while channel 1 forecasts the distinct
  // ephemeral address that will bind after restart — the two are different
  // hazards and both must surface. The :0 wildcard must not collapse the
  // future-state forecast onto the current-state notice when the rebind did not
  // land.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "0.0.0.0:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "deferred",
      warnings: [applyWarning],
    }),
  );
  // Both surface — the rebind failed (apply_outcome "deferred"), so the :0
  // forecast for the future ephemeral bind is not the same as the serving
  // notice for the current concrete port, and both reach the operator.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice keeps the :0 exposure forecast when the rebind status is unknown (no apply_outcome, require_token flip)", () => {
  // The :0 resolved-port relaxation is gated on bindSucceeded, which requires
  // apply_outcome "applied" and no rebind failure warning for the listener in
  // channel 2. This response carries no apply_outcome (an older daemon that
  // predates the field), so the per-listener rebind status is unknown and the
  // relaxation does not fire: a pending :0 configured address that a prior rebind
  // left unbound is not confirmed as the serving address, and the :0 forecast
  // (the future bind) is distinct from the serving notice (the current concrete
  // port) — both surface.
  const configuredAddr = "0.0.0.0:0";
  const servingAddr = "0.0.0.0:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Both surface — apply_outcome is absent (an older daemon that predates the
  // field), so the per-listener rebind status is unknown and the :0 relaxation
  // does not fire; the future :0 forecast and the current concrete serving
  // notice are different hazards.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates an uncompressed IPv6 wildcard (configured [0:0:0:0:0:0:0:0]:8443 vs serving [::]:8443)", () => {
  // A valid configured bind such as [0:0:0:0:0:0:0:0]:8443 is accepted by the
  // validator and net.Listen, but the bound address is reported as [::]:8443
  // (net.Addr().String() emits the compressed form). The old wildcard
  // normalization only recognized the already-compressed "::" spelling, so both
  // hosts were treated as different literals and even a successful rebind (same
  // all-interfaces bind, same port) rendered the channel-1 forecast and the
  // channel-2 notice twice. normalizeListenHost now canonicalizes IPv6 hosts
  // (canonicalizeIPv6Host) so 0:0:0:0:0:0:0:0 and :: both canonicalize to the
  // all-interfaces wildcard and compare equal, matching the IPv4 0.0.0.0.
  const configuredAddr = "[0:0:0:0:0:0:0:0]:8443";
  const servingAddr = "[::]:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the compressed [::]:8443 serving address) only; the uncompressed
  // [0:0:0:0:0:0:0:0]:8443 forecast is the same all-interfaces bind and is
  // suppressed (canonicalizeIPv6Host maps both to the IPv6 wildcard).
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates a successful rebind with a service-name port not in the well-known table (http-alt vs 8080)", () => {
  // net.LookupPort resolves service names from /etc/services, which is far
  // larger than the browser-side WELL_KNOWN_TCP_SERVICES fallback (http-alt,
  // mysql, postgresql, submission, imaps, pop3s all resolve but are absent from
  // the table). On a successful rebind, the configured service-name port IS the
  // serving port the kernel resolved it to, so the resolved-port relaxation
  // (gated on bindSucceeded) matches a non-numeric configured port (the service
  // name the table does not carry) against the numeric serving port — without
  // reproducing net.LookupPort in the browser. A failed rebind (or a non-listener
  // save) leaves the serving address as the OLD bound address, so the relaxation
  // does not fire and both surface (the service-name forecast is a future bind,
  // not the current serving notice).
  const configuredAddr = "0.0.0.0:http-alt";
  const servingAddr = "0.0.0.0:8080";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the numeric serving port 8080) only; the http-alt forecast is
  // the same exposure and is suppressed via the bindSucceeded-gated resolved-port
  // relaxation (the table does not carry http-alt, so the match relies on the
  // rebind-success signal, not the browser-side service table).
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the service-name exposure forecast when the rebind failed (deferred)", () => {
  // A service-name port the well-known table does not carry (http-alt) is
  // resolved to its numeric port (8080) only on a successful rebind — the
  // resolved-port relaxation is gated on bindSucceeded. When the rebind fails
  // (apply_outcome "deferred"), the serving address is the OLD bound port (not
  // the resolved service-name port) and the browser cannot confirm the
  // configured service name resolved to it, so both the forecast (the
  // service-name port that will bind after restart) and the serving notice (the
  // current concrete port) surface.
  const configuredAddr = "0.0.0.0:http-alt";
  const servingAddr = "0.0.0.0:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Saved — network.listen_addr could not be applied to the running daemon; it takes effect on the next daemon start (see the warning for the reason).";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "deferred",
      warnings: [applyWarning],
    }),
  );
  // Both surface — the rebind failed (apply_outcome "deferred"), so the
  // resolved-port relaxation does not fire and the service-name forecast (the
  // future bind on port 8080) is distinct from the current serving notice (port
  // 9443). The well-known table does not carry http-alt, and without the
  // rebind-success signal the browser cannot confirm the two are the same bind.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a dual-stack ephemeral rebind with an uncompressed IPv6 configured wildcard", () => {
  // The dual-stack ephemeral rebind (0.0.0.0:0 vs [::]:<port>) combined with
  // IPv6 canonicalization: a configured [0:0:0:0:0:0:0:0]:0 (the uncompressed
  // IPv6 unspecified address) is accepted by the validator and net.Listen, and
  // the bound address is reported as [::]:<port> (compressed). The rebind
  // succeeded (apply_outcome "applied"), so the resolved-port relaxation (the :0
  // forecast matches the concrete serving port) and the wildcard host
  // canonicalization (0:0:0:0:0:0:0:0 → ::, both the IPv6 wildcard) both fire.
  const configuredAddr = "[0:0:0:0:0:0:0:0]:0";
  const servingAddr = "[::]:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address on [::], the compressed IPv6
  // wildcard) only; the uncompressed [0:0:0:0:0:0:0:0]:0 forecast is the same
  // all-interfaces bind at the resolved ephemeral port and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates an IPv4-mapped IPv6 wildcard ([::ffff:0.0.0.0]:0 vs [::]:<port>)", () => {
  // ::ffff:0.0.0.0 is the IPv4-mapped form of the all-interfaces wildcard,
  // accepted by the validator (config/listen_addr.go via net.Listen) and bound
  // as the IPv6 wildcard on a dual-stack host, so net.Listen reports [::]:<port>.
  // canonicalizeIPv6Host collapses IPv4-mapped/compatible forms to the bare IPv4
  // (matching net.IP.String via IP.To4), so ::ffff:0.0.0.0 canonicalizes to
  // 0.0.0.0 — the same value :: normalizes to — and the resolved-port relaxation
  // (bindSucceeded) matches the :0 forecast to the concrete serving port.
  const configuredAddr = "[::ffff:0.0.0.0]:0";
  const servingAddr = "[::]:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address on [::], the IPv6 wildcard) only;
  // the IPv4-mapped [::ffff:0.0.0.0]:0 forecast is the same all-interfaces bind
  // at the resolved ephemeral port and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates an IPv4-mapped IPv6 non-wildcard ([::ffff:1.2.3.4]:8443 vs 1.2.3.4:8443)", () => {
  // ::ffff:1.2.3.4 is the IPv4-mapped form of 1.2.3.4. Go's net.IP.String()
  // emits the bare IPv4 for an IPv4-mapped address (IP.To4), so a successful
  // rebind reports the serving address as 1.2.3.4:<port>. canonicalizeIPv6Host
  // collapses ::ffff:1.2.3.4 to 1.2.3.4, matching the serving address.
  const configuredAddr = "[::ffff:1.2.3.4]:8443";
  const servingAddr = "1.2.3.4:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the bare IPv4 serving address) only; the IPv4-mapped configured
  // forecast is the same bind and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates a zoned IPv6 wildcard (configured [0:0:0:0:0:0:0:0%lo]:0 vs serving [::]:<port>)", () => {
  // A zoned IPv6 address like [0:0:0:0:0:0:0:0%lo]:0 is accepted by Go's
  // net.Listen, and the bound address is reported as [::]:<port> (the zone is
  // dropped from net.Addr().String()). canonicalizeIPv6Host strips the zone
  // (RFC 4007) so the configured host canonicalizes to ::, and the successful
  // rebind (apply_outcome "applied") maps :: to 0.0.0.0 (the cross-family
  // wildcard equivalence), matching the serving [::]. The resolved-port
  // relaxation (bindSucceeded) then matches the :0 forecast to the concrete
  // serving port. Without stripping the zone, canonicalizeIPv6Host could not
  // parse the %lo suffix and returned the host unchanged, so
  // isListenHostLiteral classified it as a literal and neither the hostname
  // relaxation nor the wildcard normalization could match the serving address.
  const configuredAddr = "[0:0:0:0:0:0:0:0%lo]:0";
  const servingAddr = "[::]:54321";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving address on [::], with the zone dropped)
  // only; the zoned [0:0:0:0:0:0:0:0%lo]:0 forecast is the same all-interfaces
  // bind at the resolved ephemeral port and is suppressed.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice deduplicates a zoned IPv6 non-ephemeral wildcard (configured [0:0:0:0:0:0:0:0%lo]:8443 vs serving [::]:8443)", () => {
  // The same zone stripping applies to a non-ephemeral port: the configured
  // [0:0:0:0:0:0:0:0%lo]:8443 (zoned IPv6 wildcard) binds on the same interface
  // as [::]:8443 (the zone is dropped from the serving address), and a
  // successful rebind (apply_outcome "applied") confirms the cross-family
  // wildcard equivalence (:: -> 0.0.0.0), so the forecast is suppressed.
  const configuredAddr = "[0:0:0:0:0:0:0:0%lo]:8443";
  const servingAddr = "[::]:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.listen_addr", value: configuredAddr, path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the exposure forecast when a service-name port is not resolved on a non-listener save", () => {
  // Go's net.LookupPort augments its built-in service mappings from the daemon
  // host's /etc/services, so the browser-side WELL_KNOWN_TCP_SERVICES table is
  // not authoritative: a host can map "http" to a different port (e.g. 8080).
  // This response carries no apply_outcome (an older daemon that predates the
  // field), so the per-listener rebind status is unknown and bindSucceeded is
  // false. The serving address is the OLD bound port while the configured service
  // name is a future state the kernel has not resolved, so the table is not used
  // and the service-name forecast does not collapse to the well-known port — both
  // the channel-2 notice (the serving port) and the channel-1 forecast (the
  // configured service name) surface. The configured 0.0.0.0:http and the serving
  // 0.0.0.0:80 are the same port on a host with the default /etc/services, but
  // the browser cannot know that without the rebind success signal, and a host
  // that maps "http" to 8080 would suppress a warning for a different restart
  // state.
  const configuredAddr = "0.0.0.0:http";
  const servingAddr = "0.0.0.0:80";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Both surface — apply_outcome is absent (an older daemon that predates the
  // field), so the per-listener rebind status is unknown and the service-name
  // table is not used; the configured 0.0.0.0:http forecast is not resolved to 80
  // and is distinct from the serving 0.0.0.0:80.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice keeps distinct scoped IPv6 zones on a non-listener save (require_token flip)", () => {
  // Go's net.TCPAddr.String() retains the zone for scoped non-wildcard
  // addresses (e.g. [fe80::1%eth0]:8443), so a failed rebind that leaves the
  // daemon serving [fe80::1%eth0]:8443 while config carries [fe80::1%eth1]:8443
  // is two different scoped interfaces, and a require_token flip must not
  // suppress the future-state forecast for the distinct %eth1 zone.
  // canonicalizeIPv6Host preserves the zone for scoped non-wildcard addresses
  // (dropping it only for the all-zeros wildcard ::, where net.Addr().String()
  // drops it from the bound address), so fe80::1%eth0 and fe80::1%eth1 do NOT
  // compare equal and both the channel-2 notice (%eth0) and the channel-1
  // forecast (%eth1) surface.
  const configuredAddr = "[fe80::1%eth1]:8443";
  const servingAddr = "[fe80::1%eth0]:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Both surface — the two scoped addresses have different zones, and
  // canonicalizeIPv6Host preserves the zone for non-wildcard addresses, so
  // fe80::1%eth0 and fe80::1%eth1 do not compare equal.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice keeps IPv4-compatible IPv6 distinct from IPv4 on a non-listener save (require_token flip)", () => {
  // ::1.2.3.4 is the IPv4-compatible form (the first 96 bits are zero, no
  // 0xffff prefix). Go's net.ParseIP("::1.2.3.4").String() is "::102:304" (not
  // "1.2.3.4") and To4() is nil, so it is a distinct IPv6 address, not IPv4.
  // Only the IPv4-MAPPED form (::ffff:a.b.c.d, the first 80 bits zero and bytes
  // 10-11 0xffff) is collapsed to IPv4 by Go's net.IP.To4(). A failed rebind
  // can leave an IPv4 1.2.3.4:8443 listener serving while [::1.2.3.4]:8443
  // remains configured, so a require_token flip must not suppress the
  // future-state forecast for the distinct IPv4-compatible address.
  // canonicalizeIPv6Host canonicalizes ::1.2.3.4 to ::102:304 (the IPv6 form),
  // not to 1.2.3.4 (the IPv4 form), so the two do not compare equal.
  const configuredAddr = "[::1.2.3.4]:8443";
  const servingAddr = "1.2.3.4:8443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      warnings: [applyWarning],
    }),
  );
  // Both surface — ::1.2.3.4 (IPv4-compatible) canonicalizes to ::102:304 (the
  // IPv6 form, not 1.2.3.4), so it does not compare equal to the serving 1.2.3.4.
  assert.equal(notice, `${restartNotice} · ${applyWarning} · ${writeWarning}`);
});

test("saveNotice deduplicates a service-name forecast when a non-listener save retried the rebind successfully", () => {
  // A non-listener save (network.require_token) can retry a pending listener
  // rebind that failed on the listener save (e.g. the port was occupied and is
  // now free). The per-key apply_outcome is "applied" for the saved key
  // (network.require_token applies live) even though the saved key is not the
  // listener, so the old gate (savedKey is a listener key AND apply_outcome is
  // "applied") left listenerRebindSucceeded false and the service-name
  // relaxation was disabled — the channel-1 forecast (0.0.0.0:http-alt, the
  // configured port the rebind just resolved) and the channel-2 notice
  // (0.0.0.0:8080, the resolved serving port) both surfaced as if the rebind
  // had failed. The rebind failure warning is absent from channel 2 when the
  // rebind succeeds, so the per-listener rebind status is derived from the
  // apply result: apply_outcome "applied" AND no rebind failure warning for the
  // listener in channel 2 means the rebind landed and the serving address is
  // the resolved form of the configured address. The resolved-port relaxation
  // (bindSucceeded) matches the service-name forecast to the concrete serving
  // port without the browser-side table (the rebind just resolved it).
  const configuredAddr = "0.0.0.0:http-alt";
  const servingAddr = "0.0.0.0:8080";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [applyWarning],
    }),
  );
  // Channel 2 (the concrete serving port 8080 the rebind just resolved) only;
  // the http-alt forecast is the same exposure and is suppressed via the
  // resolved-port relaxation (bindSucceeded), which is now gated on the
  // per-listener rebind status (no rebind failure warning in channel 2 for
  // network.listen_addr) rather than on the saved key being a listener key.
  assert.equal(notice, `${restartNotice} · ${applyWarning}`);
});

test("saveNotice keeps the service-name forecast when a non-listener save retried the rebind but it failed again", () => {
  // The other half of the non-listener rebind case: a pending listener rebind
  // that the non-listener save retried but FAILED again. The rebind failure
  // warning ("apply network.listen_addr ... — daemon still serving on the
  // previous address ...") is in channel 2, so the per-listener rebind status
  // is false — the serving address is the OLD bound port (not the resolved
  // configured service name), and the service-name relaxation must not collapse
  // them. The channel-1 forecast (the configured service name the rebind did not
  // land) and the channel-2 notice (the old serving port) both surface.
  const configuredAddr = "0.0.0.0:http-alt";
  const servingAddr = "0.0.0.0:9443";
  const writeWarning = `network.listen_addr "${configuredAddr}" is reachable from the network and network.require_token is false, which puts a plain-HTTP control plane with no authentication in front of anyone who can reach it — including DeliverPrompt, which runs instructions through your agents. The daemon will serve this on its next start. Run \`af config set network.require_token true\` to require a token (\`af token show\` prints it), or set network.listen_addr back to a loopback address such as 127.0.0.1:8443, or "" to turn the web server off.`;
  const applyWarning = `network.listen_addr "${servingAddr}" is reachable from the network and network.require_token is false, so af serves its full control API — including DeliverPrompt, which runs instructions through your agents — to anyone who can reach that address, with no authentication and no TLS · set network.require_token = true to require a bearer token (\`af token show\` prints it), or set network.listen_addr to 127.0.0.1:8443 to serve this machine only`;
  const rebindFailure = `apply network.listen_addr "${configuredAddr}": listen tcp ${configuredAddr}: bind: address already in use — daemon still serving on the previous address "${servingAddr}"`;
  const restartNotice = "Applied — the running daemon is using the new value now.";
  const notice = saveNotice(
    setResp({
      result: { key: "network.require_token", value: "false", path: "/tmp/config.toml", requires_restart: false, warnings: [writeWarning] },
      restart_notice: restartNotice,
      apply_outcome: "applied",
      warnings: [rebindFailure, applyWarning],
    }),
  );
  // Both the rebind failure warning (channel 2) and the exposure notice (channel
  // 2) surface before the forecast (channel 1): the rebind failure warning
  // confirms the listener is still serving the old address, so the per-listener
  // rebind status is false and the service-name relaxation does not collapse
  // the configured http-alt (a future state the rebind did not resolve) into the
  // serving 9443.
  assert.equal(notice, `${restartNotice} · ${rebindFailure} · ${applyWarning} · ${writeWarning}`);
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
