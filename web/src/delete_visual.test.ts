import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

test("session deletion uses the shared destructive confirmation style", () => {
  const source = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  const kill = source.split("kill: {")[1].split("archive: {")[0];
  assert.match(kill, /confirmClass: "af-danger"/);
});
