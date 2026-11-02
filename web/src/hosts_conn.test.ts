// The hosts section's connection-lifecycle contract (#5571 W10.5): what a
// disconnect/reconnect may and may not carry over. These run index.ts's own
// function bodies — extracted like account_login_race.test.ts does — in a VM
// sandbox with stubbed collaborators, so they exercise the real fence rather
// than a copy of it.
//
// Two findings this pins:
//
//  1. disconnect() must retire the section's EARNED state, not only its
//     capability bit: a `checks` verdict and a `status` line were probed
//     against the OLD connection's host table, and hostRows indexes checks by
//     name alone — a stale "ready" would sit beside a new destination the
//     next daemon saved under the same name.
//  2. hostsHealthNow's `editable: false` write must take the same
//     generation+token fence its callers apply after it returns: a health
//     read sent before a disconnect and answered after the reconnect would
//     otherwise disarm a capable new connection.
//
// And two the re-review found on the fixed head:
//
//  3. doHostCheck must record the entry each verdict was probed against, so
//     a config edit under the same name retires the verdict rather than
//     displaying it beside a destination it never measured.
//  4. A successful retry must retire the check error its failed try left —
//     same name, or every check error after a check-all — without touching an
//     add/remove refusal that is not a check outcome.

import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { runInNewContext } from "node:vm";
import test from "node:test";
import ts from "typescript";

function topLevelFunctions(source: string, names: Set<string>): string {
  const ast = ts.createSourceFile("index.ts", source, ts.ScriptTarget.Latest, true);
  return ast.statements
    .filter((node) => ts.isFunctionDeclaration(node) && names.has(node.name?.text ?? ""))
    .map((node) => node.getText(ast))
    .join("\n");
}

interface HostsSnapshot {
  editable: boolean | null;
  checks: Record<string, unknown>;
  status: { name: string; message: string; error: boolean } | null;
  busy: string[];
}

interface SetCall {
  hosts?: HostsSnapshot;
  [key: string]: unknown;
}

/** The shared sandbox: a store that records every hosts write, and the
 *  connection knobs connect()/disconnect() read. */
function stage(names: string[]): {
  context: Record<string, unknown>;
  setCalls: SetCall[];
  hostsNow(): HostsSnapshot;
} {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handlers = topLevelFunctions(source, new Set(names));
  const setCalls: SetCall[] = [];
  let hosts: HostsSnapshot = {
    editable: true,
    checks: {
      box1: {
        entry: { host: "box1.tailnet.ts.net", user: "ubuntu" },
        result: { name: "box1", probed: true, reachable: true, complete: true, readiness: "ready" },
      },
    },
    status: { name: "box1", message: "ready (git, tmux and gh answered)", error: false },
    busy: ["box1"],
  };
  const context: Record<string, unknown> = {
    connectionGate: { invalidate() {} },
    pendingRestores: { reset() {} },
    optimisticSessions: { reset() {} },
    stopStream() {},
    closeOverlays() {},
    clearToken() {},
    store: {
      get: () => ({ authRequired: false, phase: "app", hosts, config: [] }),
      set(patch: SetCall) {
        setCalls.push(patch);
        if (patch.hosts !== undefined) hosts = patch.hosts;
      },
    },
  };
  const code = ts.transpileModule(
    `
    let token = "tok";
    let connectionGeneration = 0;
    let rebindInFlight = null;
    // Mirrors connect()'s commit and disconnect()'s bump without either's
    // dependency set: the generation is what the fences read.
    function simulateDisconnect() { connectionGeneration++; token = null; }
    function simulateConnect(candidate) { token = candidate; connectionGeneration++; }
    ${handlers}
    `,
    { compilerOptions: { target: ts.ScriptTarget.ES2022, module: ts.ModuleKind.None } },
  ).outputText;
  runInNewContext(code, context);
  return { context, setCalls, hostsNow: () => hosts };
}

/** Resolves a promise the test controls, like account_login_race's startPromise. */
function gate<T>(): { promise: Promise<T>; resolve(v: T): void } {
  let resolve!: (v: T) => void;
  const promise = new Promise<T>((r) => { resolve = r; });
  return { promise, resolve };
}

/** One macrotask — flushes the microtask chain a voided checkHost promise
 *  leaves behind (the sandbox has no timers; the outer realm's do). */
function tick(): Promise<void> {
  return new Promise((r) => setImmediate(r));
}

/** doHostCheck's collaborators as the shell would wire them: hostRows reads
 *  the last-known table (doHostCheck snapshots it at request time), checkHost
 *  is the POST, errorText the message extractor. */
function stageCheck(table: { name: string; entry: unknown }[]): {
  s: ReturnType<typeof stage>;
  doHostCheck(name: string): void;
} {
  const s = stage(["doHostCheck", "setHostBusy", "setHostStatus"]);
  s.context.hostRows = () => ({ rows: table.map((r) => ({ name: r.name, entry: r.entry, isDefault: false })), defaultName: "", parseError: "" });
  s.context.errorText = (e: unknown) => (e instanceof Error ? e.message : String(e));
  const doHostCheck = s.context.doHostCheck as (name: string) => void;
  return { s, doHostCheck };
}

const readyVerdict = { name: "box1", probed: true, reachable: true, complete: true, readiness: "ready" };
const box1Entry = { host: "box1.tailnet.ts.net", user: "ubuntu" };

test("disconnect retires the hosts checks and status the old connection earned", () => {
  const s = stage(["disconnect"]);
  const disconnect = s.context.disconnect as () => void;

  disconnect();

  const last = s.setCalls.at(-1)?.hosts;
  assert.ok(last !== undefined, "disconnect writes a hosts reset");
  assert.equal(last.editable, null, "the capability bit belongs to the dead connection");
  assert.equal(last.busy.length, 0, "an in-flight edit's finally is fenced off");
  assert.equal(
    Object.keys(last.checks).length,
    0,
    "a verdict was probed against the OLD connection's host table — it must not sit beside the new one",
  );
  assert.equal(last.status, null, "the old connection's outcome line goes with it");
});

test("hostsHealthNow does not disarm a new connection with the old one's answer", async () => {
  const s = stage(["hostsHealthNow"]);
  const health = gate<unknown>();
  s.context.daemonHealth = () => health.promise;
  const hostsHealthNow = s.context.hostsHealthNow as (tok: string) => Promise<unknown>;
  const simulateDisconnect = s.context.simulateDisconnect as () => void;

  const pending = hostsHealthNow("tok");
  simulateDisconnect();
  health.resolve({ ok: true, config_hosts_edit: false, version: "old" });
  await pending;

  assert.equal(
    s.hostsNow().editable,
    true,
    "the dead connection's health answer must not write editable:false onto the reconnect's state",
  );
});

// Soundness control: on the SAME connection the refusal write still lands —
// the fence must not swallow the live case it exists to serve.
test("hostsHealthNow writes editable:false when the live connection lacks the bit", async () => {
  const s = stage(["hostsHealthNow"]);
  s.context.daemonHealth = async () => ({ ok: true, version: "old" });
  const hostsHealthNow = s.context.hostsHealthNow as (tok: string) => Promise<unknown>;

  await hostsHealthNow("tok");

  assert.equal(s.hostsNow().editable, false, "a live health read without the bit disarms the section");
});

test("a health answer arriving after disconnect and same-token reconnect is still fenced", async () => {
  const s = stage(["hostsHealthNow"]);
  const health = gate<unknown>();
  s.context.daemonHealth = () => health.promise;
  const hostsHealthNow = s.context.hostsHealthNow as (tok: string) => Promise<unknown>;
  const simulateDisconnect = s.context.simulateDisconnect as () => void;
  const simulateConnect = s.context.simulateConnect as (candidate: string) => void;

  const pending = hostsHealthNow("tok");
  simulateDisconnect();
  simulateConnect("tok"); // the SAME credential — only the generation tells the connections apart
  health.resolve({ ok: true, version: "old" });
  await pending;

  assert.equal(
    s.hostsNow().editable,
    true,
    "a same-token reconnect is still a new generation; the stale answer must not disarm it",
  );
});

test("a landed verdict is stored with the entry it was probed against", async () => {
  const { s, doHostCheck } = stageCheck([{ name: "box1", entry: box1Entry }]);
  s.context.checkHost = async () => ({ hosts: [readyVerdict] });
  const store = s.context.store as { set(p: { hosts: HostsSnapshot }): void };
  store.set({ hosts: { editable: true, checks: {}, status: null, busy: [] } });

  doHostCheck("box1");
  await tick();

  const check = s.hostsNow().checks.box1 as { entry: unknown; result: unknown };
  assert.ok(check !== undefined, "the verdict lands under the host's name");
  assert.deepEqual(
    check.entry,
    box1Entry,
    "it carries the entry snapshot from request time — the render retires it when the saved entry changes",
  );
});

test("a successful retry retires the check error its failed try left", async () => {
  const { s, doHostCheck } = stageCheck([{ name: "box1", entry: box1Entry }]);
  const store = s.context.store as { set(p: { hosts: HostsSnapshot }): void };
  const seed = (status: unknown) =>
    store.set({ hosts: { editable: true, checks: {}, status: status as HostsSnapshot["status"], busy: [] } });

  // First try fails: the row keeps the request-level error…
  s.context.checkHost = async () => { throw new Error("POST /v1/CheckHost: daemon went away"); };
  doHostCheck("box1");
  await tick();
  const failed = s.hostsNow().status;
  assert.equal(failed?.name, "box1");
  assert.equal(failed?.error, true);
  assert.equal((failed as { check?: boolean } | null)?.check, true, "check outcomes are marked so a retry can retire exactly them");

  // …then the retry answers: the old error goes with the verdict arriving.
  s.context.checkHost = async () => ({ hosts: [readyVerdict] });
  seed({ name: "box1", message: "POST /v1/CheckHost: daemon went away", error: true, check: true });
  doHostCheck("box1");
  await tick();
  assert.equal(s.hostsNow().status, null, "a successful retry retires the error its failed try left");
});

test("a check-all success retires every check error; a per-host success keeps others'", async () => {
  const { s, doHostCheck } = stageCheck([
    { name: "box1", entry: box1Entry },
    { name: "box2", entry: { host: "box2.tailnet.ts.net" } },
  ]);
  s.context.checkHost = async (name: string) => ({ hosts: [{ ...readyVerdict, name: name === "" ? "box1" : name }] });
  const store = s.context.store as { set(p: { hosts: HostsSnapshot }): void };
  const seed = (status: unknown) =>
    store.set({ hosts: { editable: true, checks: {}, status: status as HostsSnapshot["status"], busy: [] } });

  // Check-all supersedes every per-host verdict — its success clears box2's
  // old check error too.
  seed({ name: "box2", message: "POST /v1/CheckHost: refused", error: true, check: true });
  doHostCheck("");
  await tick();
  assert.equal(s.hostsNow().status, null, "check-all's success retires every check error");

  // A single-host success leaves another host's check error where it is — it
  // was never retried.
  seed({ name: "box2", message: "POST /v1/CheckHost: refused", error: true, check: true });
  doHostCheck("box1");
  await tick();
  assert.equal(s.hostsNow().status?.name, "box2", "box1's retry does not clear box2's check error");

  // And neither clears an add/remove refusal: a probe's success does not
  // resolve a write refusal — the flag is what scopes the retirement.
  seed({ name: "", message: "the running daemon cannot apply a host-list edit", error: true });
  doHostCheck("");
  await tick();
  assert.equal(s.hostsNow().status?.message, "the running daemon cannot apply a host-list edit");
});
