import assert from "node:assert/strict";
import { register } from "node:module";
import test from "node:test";

register("./browser_stub_loader.mjs", import.meta.url);
const { AttachTerminal } = await import("./terminal.js");

// Exercise the real initial-fit/connection boundary without constructing xterm.
// Browser coverage below separately checks the actual surface and input wiring.
function stage() {
  const calls: string[] = [];
  const host = { clientWidth: 900, clientHeight: 600 };
  const term = { rows: 24, cols: 80 };
  const fit = {
    proposeDimensions: (): { rows: number; cols: number } | undefined => ({ rows: 40, cols: 120 }),
    fit: () => { Object.assign(term, { rows: 40, cols: 120 }); calls.push("fit"); },
  };
  const terminal = Object.assign(Object.create(AttachTerminal.prototype), {
    container: host, term, fit, stopped: false, initialConnectStarted: false,
    pendingViewport: null,
    sendResize: () => calls.push("resize"),
    connect: () => calls.push("connect"),
  }) as {
    fitVisibleHost(): void;
    stopped: boolean;
    connect(): void;
  };
  return { terminal, host, fit, calls };
}

test("measured attach fits immediately and connects once after the owner can assign it", async () => {
  const { terminal, calls } = stage();
  terminal.fitVisibleHost();
  assert.deepEqual(calls, ["fit", "resize"]);
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize", "connect"]);
});

test("hidden or unresolved geometry waits for a later successful measurement", async () => {
  const { terminal, host, fit, calls } = stage();
  host.clientWidth = 0;
  terminal.fitVisibleHost();
  host.clientWidth = 900;
  const propose = fit.proposeDimensions;
  fit.proposeDimensions = () => undefined;
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, []);
  fit.proposeDimensions = propose;
  terminal.fitVisibleHost();
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize", "connect"]);
});

test("disposal before the queued attach prevents socket construction and status callbacks", async () => {
  const { terminal, calls } = stage();
  terminal.connect = (AttachTerminal.prototype as unknown as { connect(): void }).connect;
  terminal.fitVisibleHost();
  terminal.stopped = true;
  // Real connect() must return before touching any absent browser dependencies.
  await Promise.resolve();
  assert.deepEqual(calls, ["fit", "resize"]);
});
