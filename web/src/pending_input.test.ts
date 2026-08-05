// Pins the type-ahead queue behind the attach terminal's input path (#2811).
//
// The defect this replaces was a silent drop, so these tests assert the two
// properties whose absence made it silent AND destructive: nothing typed is lost
// while the socket is down, and what comes back comes back IN ORDER. A queue that
// preserved bytes but reordered them would corrupt a command line just as badly
// as the drop did.

import { test } from "node:test";
import assert from "node:assert/strict";

import { DEFAULT_MAX_PENDING_INPUT_BYTES, PendingInput } from "./pending_input.js";

const enc = new TextEncoder();
const dec = new TextDecoder();

/** Concatenates a drain back into the byte stream the PTY would have received. */
function stream(frames: Uint8Array[]): string {
  return frames.map((f) => dec.decode(f)).join("");
}

test("holds what was typed while the socket is down and replays it IN ORDER", () => {
  const pending = new PendingInput();
  // One frame per keystroke, exactly as xterm's onData delivers them.
  for (const ch of "for i in $(seq 1 80); do") {
    assert.equal(pending.push(enc.encode(ch)), true);
  }

  assert.equal(pending.length, 24);
  const drained = stream(pending.drain());
  assert.equal(
    drained,
    "for i in $(seq 1 80); do",
    "the command line must survive byte-for-byte — a lost PREFIX is what left the shell at a continuation prompt",
  );
  assert.equal(pending.length, 0, "a drain empties the queue so the next flush cannot double-send");
  assert.equal(pending.bytes, 0);
});

test("a drain returns nothing when nothing was held", () => {
  const pending = new PendingInput();
  assert.deepEqual(pending.drain(), []);
});

test("an empty frame is not held and is not a refusal", () => {
  const pending = new PendingInput();
  assert.equal(pending.push(new Uint8Array()), true);
  assert.equal(pending.length, 0);
});

test("the cap refuses the NEWEST frame and keeps every earlier byte intact", () => {
  const pending = new PendingInput(8);
  assert.equal(pending.push(enc.encode("12345")), true);
  assert.equal(pending.push(enc.encode("678")), true, "exactly at the cap still fits");
  assert.equal(pending.bytes, 8);

  assert.equal(pending.push(enc.encode("9")), false, "over the cap must be refused, not evict the front");
  assert.equal(stream(pending.drain()), "12345678", "the refusal must not disturb what is already queued");
});

test("clear drops everything — the PTY it was typed at is gone", () => {
  const pending = new PendingInput();
  pending.push(enc.encode("rm -rf /tmp/x"));
  pending.clear();
  assert.equal(pending.length, 0);
  assert.deepEqual(pending.drain(), [], "cleared input must never reach a later PTY");
  assert.equal(pending.bytes, 0);
});

test("the default cap is far above any real type-ahead", () => {
  // Not a magic-number echo: the point is that ordinary typing and an ordinary
  // paste can never hit the ceiling, so the refusal path stays reserved for a
  // socket that is never coming back.
  assert.ok(DEFAULT_MAX_PENDING_INPUT_BYTES >= 16 * 1024);
  const pending = new PendingInput();
  assert.equal(pending.push(enc.encode("x".repeat(8 * 1024))), true, "an 8 KiB paste must be held, not refused");
});
