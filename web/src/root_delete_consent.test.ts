import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

test("root deletion receives the daemon-owned identity and guards form submission", () => {
  const handler = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  assert.match(handler, /isRoot: session\.is_root === true/);
  const modal = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  assert.match(modal, /acknowledgment && !acknowledgment.checked/);
  assert.match(modal, /scheduled and watch-task delivery/);
});
