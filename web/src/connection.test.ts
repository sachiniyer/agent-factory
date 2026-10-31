import { test } from "node:test";
import assert from "node:assert/strict";

import { connectionNotice, connectionWords, isDown, nextRetryAt, RECONNECT_GRACE_MS } from "./connection.js";

const up = { live: "open", termStatus: "open" };

test("a connected client shows nothing (#6430)", () => {
  assert.equal(isDown(up), false);
  assert.equal(connectionNotice(up, null, 10_000), null);
});

test("the first connect and an exited PTY are not an outage (#6430)", () => {
  assert.equal(isDown({ live: "connecting", termStatus: "connecting" }), false);
  assert.equal(isDown({ live: "open", termStatus: "exited" }), false);
});

test("a drop is silent until it has lasted the grace period (#6430)", () => {
  const input = { live: "reconnecting", termStatus: "open" };
  assert.equal(connectionNotice(input, 1_000, 1_000 + RECONNECT_GRACE_MS - 1), null);
  assert.deepEqual(connectionNotice(input, 1_000, 1_000 + RECONNECT_GRACE_MS), { down: true, retryInS: null });
});

test("either socket down is enough, and the soonest retry wins (#6430)", () => {
  const input = { live: "reconnecting", termStatus: "reconnecting", liveRetryAt: 20_000, termRetryAt: 14_500 };
  assert.equal(nextRetryAt(input), 14_500);
  assert.deepEqual(connectionNotice(input, 0, 10_000), { down: true, retryInS: 5 });
  assert.deepEqual(connectionNotice({ live: "open", termStatus: "reconnecting", termRetryAt: 13_000 }, 0, 10_000), { down: true, retryInS: 3 });
});

test("a retry time for a socket that is up is ignored (#6430)", () => {
  const input = { live: "open", termStatus: "reconnecting", liveRetryAt: 12_000, termRetryAt: 16_000 };
  assert.equal(nextRetryAt(input), 16_000);
});

test("the words name the retry time only when one is scheduled (#6430)", () => {
  assert.equal(connectionWords({ down: true, retryInS: 4 }).full, "Reconnecting… retry in 4s");
  assert.equal(connectionWords({ down: true, retryInS: null }).full, "Reconnecting…");
  assert.equal(connectionWords({ down: true, retryInS: 4 }).retry, "retry in 4s");
});
