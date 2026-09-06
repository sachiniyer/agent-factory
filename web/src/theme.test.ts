import assert from "node:assert/strict";
import { test } from "node:test";
import { readFileSync } from "node:fs";
import { createLatestRequestGate } from "./refetch.js";
import { THEME_CHOICES, normalizeThemeChoice, connectionAttemptMayCommit, hasConnectedToken } from "./theme.js";

test("appearance has exactly Light, Dark and System; old Auto and invalid storage migrate to System", () => {
  assert.deepEqual(THEME_CHOICES, ["light", "dark", "system"]);
  for (const value of ["auto", "system", null, undefined, "zenburn", "#ffffff"]) assert.equal(normalizeThemeChoice(value), "system");
  assert.equal(normalizeThemeChoice("light"), "light");
  assert.equal(normalizeThemeChoice("dark"), "dark");
});

test("browser appearance has no daemon palette projection or color overrides", () => {
  const source = readFileSync(new URL("./theme.ts", import.meta.url), "utf8");
  assert.doesNotMatch(source, /DaemonTheme|deriveTheme|setProperty|#[0-9a-f]{3,8}\b/i);
  assert.match(source, /--af-surface/);
});

test("connection fences admit tokenless clients and reject invalidated attempts", () => {
  const gate = createLatestRequestGate();
  const attempt = gate.begin();

  assert.equal(hasConnectedToken(""), true, "the empty token is an authorized tokenless connection");
  assert.equal(hasConnectedToken(null), false);
  assert.equal(connectionAttemptMayCommit(attempt, "", ""), true);
  gate.invalidate();
  assert.equal(connectionAttemptMayCommit(attempt, "", ""), false);
});
