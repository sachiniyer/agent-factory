// Pins the spawn-failure copy to what the client actually observed (#3238). The
// daemon answers 503 BOTH when the assistant is absent from the build AND for its
// own lifecycle refusals (starting / upgrade probation / upgrade handoff —
// daemon/configassistant_routes.go, control_client.go). The status code cannot say
// which, so the pane must surface the daemon's own report — never convert a
// temporary "retry shortly" refusal into a permanent feature-absence claim.

import { test } from "node:test";
import assert from "node:assert/strict";
import { register } from "node:module";

// config_assistant.ts → terminal.ts → xterm's stylesheet + UMD bundle, neither of
// which plain node can load. Stub them (split_focus.test.ts precedent), then import
// dynamically so the hook is registered first.
register("./browser_stub_loader.mjs", import.meta.url);
const { spawnFailureCopy } = (await import("./config_assistant.js")) as {
  spawnFailureCopy: (e: unknown) => { status: string; error: string };
};
const { ApiError } = await import("./api.js");

test("a lifecycle 503 surfaces the daemon's retryable report, not a build claim (#3238)", () => {
  // The reachable case: the web UI stays open across a daemon restart/upgrade and
  // the POST lands during warm-up. The daemon's message says what happened and
  // what to do; the pane must not replace it with a mechanism it cannot know.
  const lifecycle = [
    "agent-factory daemon is starting (restoring sessions); retry shortly",
    "agent-factory daemon is validating an upgrade (transaction abc123); retry shortly",
    "agent-factory daemon is handing off to an upgrade; retry shortly",
  ];
  for (const msg of lifecycle) {
    const copy = spawnFailureCopy(new ApiError(503, msg));
    assert.equal(copy.status, "Unavailable");
    assert.equal(copy.error, msg, "the daemon's own report is the one thing the client observed");
    assert.ok(!copy.error.includes("daemon build"), `a temporary refusal must not become a feature-absence claim: ${copy.error}`);
  }
});

test("a genuine not-built 503 still reads as not built — in the daemon's words", () => {
  const copy = spawnFailureCopy(new ApiError(503, "config assistant is not available in this build"));
  assert.equal(copy.status, "Unavailable");
  assert.equal(copy.error, "config assistant is not available in this build");
});

test("a messageless 503 falls back to an observed, actionable line", () => {
  const copy = spawnFailureCopy(new ApiError(503, ""));
  assert.equal(copy.status, "Unavailable");
  assert.equal(copy.error, "The daemon reported the config assistant unavailable. Close and try again.");
});

test("a transport failure stays the Offline surface", () => {
  const copy = spawnFailureCopy(new ApiError(0, "cannot reach the daemon: Failed to fetch"));
  assert.equal(copy.status, "Offline");
  assert.equal(copy.error, "Could not reach the daemon. Close and try again.");
});

test("any other failure keeps its own message under Failed to start", () => {
  assert.deepEqual(spawnFailureCopy(new ApiError(500, "spawn failed: no tmux")), {
    status: "Failed to start",
    error: "spawn failed: no tmux",
  });
  assert.deepEqual(spawnFailureCopy(new Error("")), {
    status: "Failed to start",
    error: "Could not start the config assistant.",
  });
});
