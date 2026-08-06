import assert from "node:assert/strict";
import test from "node:test";

import { MidLineHold } from "./delivery_hold.js";

// A partial line takes the lease; committing it gives the lease back. This is the
// whole user-visible contract: an automated delivery is held only while there is
// something of the user's to protect.
test("delivery_hold: a partial line holds, Enter releases", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("g", 0), "pause");
  assert.equal(h.holding, true);
  assert.equal(h.noteInput("it status", 10), "none", "still mid-line, and too soon to renew");
  assert.equal(h.noteInput("\r", 20), "resume");
  assert.equal(h.holding, false);
});

// Nothing to protect, nothing to hold: a bare Enter on an empty prompt must not
// take a lease, or every keystroke-free Enter would briefly blind the daemon.
test("delivery_hold: Enter with no partial line does nothing", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\r", 0), "none");
  assert.equal(h.holding, false);
});

// Ctrl-C/Ctrl-U/Ctrl-D discard the line. Holding afterwards would delay a
// delivery to protect text that no longer exists.
test("delivery_hold: abandoning the line releases the hold", () => {
  for (const abandon of ["\x03", "\x15", "\x04"]) {
    const h = new MidLineHold();
    assert.equal(h.noteInput("half a command", 0), "pause");
    assert.equal(h.noteInput(abandon, 5), "resume", `${JSON.stringify(abandon)} must release`);
    assert.equal(h.holding, false);
  }
});

// The paste case, and the reason the last byte decides rather than "contains a
// newline". A pasted block that ends mid-line leaves the user mid-line.
test("delivery_hold: a paste ending mid-line still holds", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("echo one\necho two\necho thr", 0), "pause", "text follows the last newline");
  assert.equal(h.holding, true);
});

// …and the same paste ending ON a newline commits, because nothing of the user's
// is left unsent.
test("delivery_hold: a paste ending on a newline releases", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("x", 0), "pause");
  assert.equal(h.noteInput("echo one\necho two\n", 5), "resume");
  assert.equal(h.holding, false);
});

// A user composing slowly must not lose protection between keystrokes, so the
// lease is renewed rather than left to lapse.
test("delivery_hold: renews while the line stays uncommitted", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("a", 0), "pause");
  assert.equal(h.tick(500), "none", "too soon");
  assert.equal(h.tick(1_000), "renew");
  assert.equal(h.tick(1_500), "none");
  assert.equal(h.tick(2_000), "renew");
});

// The stated bound: a user who walks away mid-line does not block a queued
// delivery forever. The hold ends after the idle window even though the line was
// never committed — the delivery then lands, rather than being dropped.
test("delivery_hold: an abandoned-in-place line releases after the idle bound", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("half typed", 0), "pause");
  assert.equal(h.tick(14_999), "renew", "still within the bound, still protected");
  assert.equal(h.tick(15_000), "resume", "bound reached: stop holding");
  assert.equal(h.holding, false);
  assert.equal(h.tick(30_000), "none", "and it stays released");
});

// Typing resets the idle clock — the bound is "untouched for N", not "held for N".
test("delivery_hold: continued typing pushes the idle bound out", () => {
  const h = new MidLineHold(1_000, 15_000);
  h.noteInput("a", 0);
  assert.equal(h.noteInput("b", 14_000), "renew", "input renews as well as resetting the clock");
  assert.equal(h.tick(20_000), "renew", "not idle: 6s since the last keystroke");
  assert.equal(h.tick(29_000), "resume", "now 15s since the last keystroke");
});

// release() reports whether there was a lease, so a teardown only sends the
// resume RPC when one is actually held.
test("delivery_hold: release reports whether a lease was held", () => {
  const h = new MidLineHold();
  assert.equal(h.release(), false, "nothing held");
  h.noteInput("x", 0);
  assert.equal(h.release(), true);
  assert.equal(h.holding, false);
  assert.equal(h.release(), false, "idempotent");
});
