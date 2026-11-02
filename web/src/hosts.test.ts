// The web client's hosts-management contracts (#5571 W10.5). Pure logic + a
// stubbed fetch, matching the rest of web/src/*.test.ts (no DOM, no jsdom): the
// rendered section is proven in the Playwright selftest.
//
// What is worth pinning here is the wire: a hosts_edit must ride SetConfigValue
// exactly the way `af hosts` sends it — the daemon's writer performs the read,
// the exists/default checks and the write as ONE read-modify-write under the
// config file lock (config.EditGlobalHosts, #5855), so a body that shaped the
// edit differently would be a second writer, and the capability gate exists
// because an older daemon tolerates the field and writes an EMPTY `hosts` in
// its place (daemon/control_types.go).

import assert from "node:assert/strict";
import { afterEach, test } from "node:test";

import { checkHost, daemonHealth, editHosts } from "./api.js";
import {
  describeHost,
  effectiveHostKeyPosture,
  emptyHostsState,
  hostCheckLines,
  hostKeyPostures,
  hostReady,
  hostRows,
  hostsEditRefusal,
} from "./hosts.js";
import type { ConfigEntry, HostCheckResult } from "./types.js";

interface Captured {
  url: string;
  method: string;
  body: Record<string, unknown>;
  auth: string | undefined;
  calls: number;
}

/** Stubs fetch with a canned envelope and captures what was sent. */
function stubFetch(data: unknown, opts: { ok?: boolean; status?: number; error?: string } = {}): Captured {
  const cap: Captured = { url: "", method: "", body: {}, auth: undefined, calls: 0 };
  (globalThis as { fetch: unknown }).fetch = async (url: string, init: RequestInit): Promise<Response> => {
    cap.calls += 1;
    cap.url = url;
    cap.method = init?.method ?? "GET";
    cap.body = init?.body !== undefined ? JSON.parse(String(init.body)) : {};
    cap.auth = (init?.headers as Record<string, string> | undefined)?.Authorization;
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

const hostsEntry = (value: string): ConfigEntry =>
  entry({ key: "hosts", type: "table", value });

const probe = (over: Partial<HostCheckResult> = {}): HostCheckResult => ({
  name: "box1",
  probed: true,
  reachable: true,
  complete: true,
  readiness: "ready",
  ...over,
});

// --- the wire: the sends the writer expects --------------------------------

test("editHosts sends the atomic per-entry edit `af hosts` sends, under hosts_edit", async () => {
  const cap = stubFetch({ result: { key: "hosts", value: "{}", path: "/c.toml", requires_restart: false }, restart_notice: "" });
  await editHosts(
    {
      op: "add",
      name: "box1",
      host: { host: "box1.tailnet.ts.net", user: "ubuntu", port: 2222 },
    },
    "tok",
  );

  assert.equal(cap.url, "/v1/SetConfigValue");
  assert.equal(cap.auth, "Bearer tok");
  // The same `hosts` key the manifest serves — the writer routes hosts_edit
  // through its own arm, not the scalar value path.
  assert.equal(cap.body.key, "hosts");
  assert.deepEqual(cap.body.hosts_edit, {
    op: "add",
    name: "box1",
    host: { host: "box1.tailnet.ts.net", user: "ubuntu", port: 2222 },
  });
  // And critically: no `value` — a whole-table write is exactly the stale-copy
  // write hosts_edit exists to replace (#5855).
  assert.equal(cap.body.value, undefined);
});

test("editHosts remove carries clear_default only when asked", async () => {
  const cap = stubFetch({});
  await editHosts({ op: "remove", name: "box1", clear_default: true }, "tok");
  assert.deepEqual(cap.body.hosts_edit, { op: "remove", name: "box1", clear_default: true });
});

test("checkHost probes one host, or every saved one on the empty name", async () => {
  let cap = stubFetch({ hosts: [probe()] });
  await checkHost("box1", "tok");
  assert.equal(cap.url, "/v1/CheckHost");
  assert.deepEqual(cap.body, { host: "box1" });

  cap = stubFetch({ hosts: [probe(), probe({ name: "gpu" })] });
  await checkHost("", "tok");
  assert.deepEqual(cap.body, { host: "" }, "the flagless `af hosts check` form probes every saved host");
});

test("daemonHealth reads GET /v1/health and returns the capability bit", async () => {
  const cap = stubFetch({ ok: true, config_hosts_edit: true, version: "1.2.3" });
  const health = await daemonHealth("tok");
  assert.equal(cap.url, "/v1/health");
  assert.equal(cap.method, "GET", "health is a GET alias for Ping, not an af() POST");
  assert.equal(cap.auth, "Bearer tok");
  assert.equal(health?.config_hosts_edit, true);
});

test("daemonHealth answers null rather than claiming a capability it could not read", async () => {
  // A refusal, a transport failure and a missing field all mean the same thing
  // to the gate: not proven capable, so the edit send stays refused.
  stubFetch({}, { ok: false, status: 503 });
  assert.equal(await daemonHealth("tok"), null);

  stubFetch({ ok: true, version: "old" });
  const old = await daemonHealth("tok");
  assert.equal(old?.config_hosts_edit, undefined, "an older daemon omits the bit — absent, never true");

  (globalThis as { fetch: unknown }).fetch = async () => {
    throw new TypeError("fetch failed");
  };
  assert.equal(await daemonHealth("tok"), null);
});

// --- the read: the manifest values into rows --------------------------------

test("hostRows parses the served hosts table and marks the default", () => {
  const entries = [
    hostsEntry('{"gpu":{"host":"10.0.0.7","port":2222},"box1":{"host":"box1.tailnet.ts.net","user":"ubuntu"}}'),
    entry({ key: "default_host", value: "gpu" }),
  ];
  const { rows, defaultName, parseError } = hostRows(entries);
  assert.equal(parseError, "");
  assert.equal(defaultName, "gpu");
  // Sorted, like `af hosts list`.
  assert.deepEqual(rows.map((r) => r.name), ["box1", "gpu"]);
  assert.equal(rows[0].entry.user, "ubuntu");
  assert.equal(rows[1].isDefault, true);
  assert.equal(rows[0].isDefault, false);
});

test("hostRows treats an absent table as empty rather than an error", () => {
  const { rows, parseError } = hostRows([entry()]);
  assert.deepEqual(rows, []);
  assert.equal(parseError, "");
});

test("hostRows reports a corrupt table instead of rendering it as empty", () => {
  const { rows, parseError } = hostRows([hostsEntry("{not json")]);
  assert.deepEqual(rows, []);
  assert.notEqual(parseError, "", "a hand-edited hosts value must not read as 'no hosts saved'");

  const wrong = hostRows([hostsEntry('["a","b"]')]);
  assert.notEqual(wrong.parseError, "");
});

// --- the copy: renderHostCheck's arms --------------------------------------

test("describeHost renders the destination like the CLI's list", () => {
  assert.equal(describeHost({ host: "box1.tailnet.ts.net" }), "box1.tailnet.ts.net");
  assert.equal(describeHost({ host: "box1", user: "ubuntu", port: 2222 }), "ubuntu@box1:2222");
  assert.equal(describeHost({ host: "2001:db8::1" }), "2001:db8::1", "an IPv6 literal is already bracket-free");
});

test("hostReady mirrors the CLI: probed + reachable + complete + no problems", () => {
  assert.equal(hostReady(probe()), true);
  assert.equal(hostReady(probe({ problems: ["install tmux 3.2+"] })), false);
  assert.equal(hostReady(probe({ reachable: false })), false);
  // A reachable host whose probe af could not read is unknown, never ready
  // (#5841) — the same arm doctor renders.
  assert.equal(hostReady(probe({ complete: false })), false);
  assert.equal(hostReady(probe({ probed: false })), false);
});

test("hostCheckLines renders every verdict arm with its remedy", () => {
  assert.deepEqual(hostCheckLines(probe()), ["ready (git, tmux and gh answered)"]);

  const busy = hostCheckLines(probe({ probed: false, readiness: "blocked" }));
  assert.match(busy[0], /another probe is already running.*blocked/);

  const down = hostCheckLines(probe({ reachable: false, error: "connection refused" }));
  assert.match(down[0], /could not be probed: connection refused/);
  assert.match(down[1], /af opens no tunnel and edits no ssh config/);

  const blocked = hostCheckLines(probe({ problems: ["tmux 3.1 is too old: install tmux 3.2 or newer"] }));
  assert.match(blocked[0], /cannot run a session yet/);
  assert.match(blocked[1], /fix: tmux 3\.1 is too old/);

  const garbled = hostCheckLines(probe({ complete: false, readiness: "unprobed" }));
  assert.match(garbled[0], /unknown: the host answered but not with a recognisable/);
  assert.match(garbled[0], /cached state: unprobed/);

  const warned = hostCheckLines(probe({ warnings: ["gh is not authenticated; private repos will not clone"] }));
  assert.match(warned[1], /note: gh is not authenticated/);
});

test("hostKeyPostures comes from the manifest's own enum, never a local copy", () => {
  const entries = [
    entry({ key: "ssh.host_key_verification", enum: ["strict", "accept-new", "insecure"] }),
  ];
  assert.deepEqual(hostKeyPostures(entries), ["strict", "accept-new", "insecure"]);
  assert.deepEqual(hostKeyPostures([entry()]), [], "a manifest predating the enum falls back to a text field");
});

test("effectiveHostKeyPosture mirrors the daemon's hostPosture fallback", () => {
  const ssh = (value: string) => entry({ key: "ssh.host_key_verification", value });
  const bare = { host: "box" };

  // No override anywhere: the global as served, else the manifest's strict
  // default — config.LoadConfig composes the same "" -> strict.
  assert.equal(effectiveHostKeyPosture(bare, [ssh("insecure")]), "insecure");
  assert.equal(effectiveHostKeyPosture(bare, []), "strict", "an absent key is the manifest default");
  assert.equal(effectiveHostKeyPosture(bare, [ssh("")]), "strict", "an empty served value is too");

  // The entry's own override wins over any global, and an override that
  // matches the global is still the override — indistinguishable by design.
  const overridden = { host: "box", host_key_verification: "accept-new" };
  assert.equal(effectiveHostKeyPosture(overridden, [ssh("insecure")]), "accept-new");
  assert.equal(effectiveHostKeyPosture(overridden, [ssh("accept-new")]), "accept-new");

  // use_ssh_config composes no host-key options at all: posture is not an
  // input to that probe, so it answers "" — a global move must not retire
  // a verdict the global never informed (#6632).
  assert.equal(effectiveHostKeyPosture({ host: "box", use_ssh_config: true }, [ssh("insecure")]), "");
});

test("emptyHostsState starts unproven: the edit send stays refused until the bit lands", () => {
  const s = emptyHostsState();
  assert.equal(s.editable, null, "never read is not capable — the gate's third state");
  assert.deepEqual(s.checks, {});
  assert.equal(s.status, null);
  assert.deepEqual(s.busy, []);
});

test("hostsEditRefusal is the CLI's refusal, version substitution and all", () => {
  // daemon/config_save_client.go EditGlobalHosts: the message a refused
  // `af hosts add|remove` prints, carried to the web unchanged so the three
  // surfaces refuse in the daemon's own words.
  assert.equal(
    hostsEditRefusal("v1.2.3"),
    "the running daemon (v1.2.3) cannot apply a host-list edit atomically; run `af daemon restart` to upgrade it, then retry (nothing was written)",
  );
  assert.equal(
    hostsEditRefusal(undefined),
    "the running daemon cannot apply a host-list edit atomically; run `af daemon restart` to upgrade it, then retry (nothing was written)",
    "a Ping that never answered still refuses — the version slot just goes empty",
  );
  assert.equal(
    hostsEditRefusal(""),
    "the running daemon cannot apply a host-list edit atomically; run `af daemon restart` to upgrade it, then retry (nothing was written)",
  );
});
