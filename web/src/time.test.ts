import assert from "node:assert/strict";
import { test } from "node:test";
import { formatTime, formatDuration } from "./time.js";
test("near timestamps share the rail's duration vocabulary in both directions", () => {
  const now = new Date("2026-09-03T03:00:00Z");
  assert.equal(formatTime("2026-09-03T03:00:30Z", now), "in <1m");
  assert.equal(formatTime("2026-09-03T02:45:00Z", now), "15m ago");
  assert.equal(formatTime("2026-09-03T05:00:00Z", now), "in 2h");
  assert.equal(formatDuration(-1), "<1m");
});
test("distant timestamps use local calendar time and invalid timestamps stay readable", () => {
  const value = "2026-09-06T14:00:00Z";
  const local = new Date(value).toLocaleString(undefined, { year: "numeric", month: "short", day: "numeric", hour: "numeric", minute: "2-digit" });
  assert.equal(formatTime(value, new Date("2026-09-03T03:00:00Z")), local);
  assert.equal(formatTime("invalid"), "Unknown time");
});
