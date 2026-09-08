import assert from "node:assert/strict";
import { test } from "node:test";
import { sessionIdentity } from "./session-identity.js";

test("session identity uses the projected agent and account, including after handoff", () => {
  const s = { title: "Review", branch: "fix/review", current_agent: "claude", account: "work" };
  assert.equal(sessionIdentity(s), "claude · work");
  assert.equal(sessionIdentity({ ...s, current_agent: "codex", account: "personal" }), "codex · personal");
});
test("missing identity is explicit, never inferred from title or current viewer", () => {
  assert.equal(sessionIdentity({ title: "Claude · Sachin", branch: "main" }), "Agent not reported · Default account");
});
