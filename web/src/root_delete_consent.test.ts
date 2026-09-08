import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

test("root deletion receives the daemon-owned identity and guards form submission", () => {
  const handler = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  assert.match(handler, /hasRootAcknowledgment = action === "kill" && session\.is_root === true/);
  assert.match(handler, /isRoot: hasRootAcknowledgment/);
  const modal = readFileSync(new URL("./modals.ts", import.meta.url), "utf8");
  assert.match(modal, /acknowledgment && !acknowledgment.checked/);
  assert.match(modal, /scheduled and watch-task delivery/);
});
