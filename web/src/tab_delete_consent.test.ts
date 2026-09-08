import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

test("both web tab deletion entry points share target-bound confirmation", () => {
  const source = readFileSync(new URL("./index.ts", import.meta.url), "utf8");
  const handler = source.slice(source.indexOf("function closeSessionTab("), source.indexOf("function renameSessionTab("));
  assert.match(handler, /confirmDeleteTabModal/);
  assert.match(handler, /captureTabDeleteTarget\(target\)/);
});
