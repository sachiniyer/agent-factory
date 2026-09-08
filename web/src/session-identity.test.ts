import assert from "node:assert/strict";
import { test } from "node:test";
import { sessionIdentity, sessionOperatorState } from "./session-identity.js";
import { Liveness } from "./types.js";

test("session identity uses the projected agent and account, including after handoff", () => {
  const s = { title: "Review", branch: "fix/review", current_agent: "claude", account: "work" };
  assert.equal(sessionIdentity(s), "claude · work");
  assert.equal(sessionIdentity({ ...s, current_agent: "codex", account: "personal" }), "codex · personal");
});
test("missing identity is explicit, never inferred from title or current viewer", () => {
  assert.equal(sessionIdentity({ title: "Claude · Sachin", branch: "main" }), "Agent not reported · Default account");
});

test("focused identity colors the operator state that its label displays", () => {
  const session = (liveness: number) => ({ title: "Review", branch: "main", liveness });
  assert.equal(sessionOperatorState(session(Liveness.Ready)), "needs-you");
  assert.equal(sessionOperatorState(session(Liveness.Running)), "working");
  assert.equal(sessionOperatorState(session(Liveness.Archived)), "archived");
  assert.equal(sessionOperatorState(session(Liveness.LimitReached)), "waiting-limit");
  assert.equal(
    sessionOperatorState({ ...session(Liveness.Ready), idle_reason: "prompt-not-delivered" }),
    "broken",
    "Broken must not inherit Ready's green color",
  );
});
