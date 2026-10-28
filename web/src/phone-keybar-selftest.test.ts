import assert from "node:assert/strict";
import { test } from "node:test";
import { leavePageAndCleanup } from "../selftest/phone-keybar.js";

test("phone keybar cleanup runs even when page navigation fails", async () => {
  let cleaned = false;
  await assert.rejects(leavePageAndCleanup({
    async goto() { throw new Error("page is closed"); },
  }, async () => { cleaned = true; }), /page is closed/);
  assert.equal(cleaned, true);
});
