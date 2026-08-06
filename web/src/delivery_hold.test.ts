import assert from "node:assert/strict";
import test from "node:test";

import { MidLineHold } from "./delivery_hold.js";

// A partial line takes the lease; committing it stops renewing. This is the whole
// user-visible contract: an automated delivery is held only while there is
// something of the user's to protect.
test("delivery_hold: a partial line holds, Enter ends the hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("g", 0), "pause");
  assert.equal(h.holding, true);
  assert.equal(h.noteInput("it status", 10), "none", "still mid-line, too soon to renew");
  assert.equal(h.noteInput("\r", 20), "none");
  assert.equal(h.holding, false, "committed: nothing left to protect");
});

// Nothing to protect, nothing to hold.
test("delivery_hold: Enter with no partial line does nothing", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\r", 0), "none");
  assert.equal(h.holding, false);
});

// #3025 review: Ctrl-C discards the line, so the hold ends.
test("delivery_hold: Ctrl-C ends the hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("half a command", 0), "pause");
  assert.equal(h.noteInput("\x03", 5), "none");
  assert.equal(h.holding, false);
});

// #3025 review finding: Ctrl-U and Ctrl-D are CONTEXT-DEPENDENT editing controls,
// not abandonments. Ctrl-U kills only back to the cursor; Ctrl-D deletes a
// character unless the buffer is empty. Releasing on either would let a delivery
// land in the text that is still there, so the hold must survive them.
test("delivery_hold: editing controls do NOT end the hold", () => {
  for (const edit of ["\x15", "\x04"]) {
    const h = new MidLineHold();
    assert.equal(h.noteInput("some text", 0), "pause");
    h.noteInput(edit, 5);
    assert.equal(h.holding, true, `${JSON.stringify(edit)} may leave text behind, so the hold must survive it`);
  }
});

// #3025 review finding: Shift+Enter emits a bare LF as a composer newline that
// does NOT submit (#2374). Treating it as a commit would drop the hold mid-draft —
// exactly when a delivery landing would submit the half-written draft.
test("delivery_hold: a composer newline (LF) keeps the hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("first paragraph", 0), "pause");
  h.noteInput("\n", 5);
  assert.equal(h.holding, true, "Shift+Enter is not a commit");
});

// #3025 review finding: xterm's onData also carries focus reports, mouse reports
// and query replies. Every one starts with ESC, and a hold created by merely
// focusing or scrolling would defer deliveries with no draft anywhere.
test("delivery_hold: terminal-generated sequences never create a hold", () => {
  const h = new MidLineHold();
  for (const seq of ["\x1b[I", "\x1b[O", "\x1b[<0;10;5M", "\x1b[?1;2c", "\x1b[A"]) {
    assert.equal(h.noteInput(seq, 0), "none", `${JSON.stringify(seq)} is not typing`);
    assert.equal(h.holding, false);
  }
});

// A bare control key on an empty prompt is not a draft either.
test("delivery_hold: a control key alone does not invent a draft", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\x7f", 0), "none", "backspace on an empty prompt");
  assert.equal(h.holding, false);
  assert.equal(h.noteInput("x", 5), "pause");
  assert.equal(h.noteInput("\x7f", 10), "none", "but it renews an existing hold rather than ending it");
  assert.equal(h.holding, true);
});

// The paste case, and why the tail after the last commit decides.
test("delivery_hold: a paste ending mid-line still holds", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("echo one\recho two\recho thr", 0), "pause", "text follows the last Enter");
  assert.equal(h.holding, true);
});

test("delivery_hold: a paste ending on Enter does not hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("x", 0), "pause");
  assert.equal(h.noteInput("echo one\recho two\r", 5), "none");
  assert.equal(h.holding, false);
});

// A user composing slowly must not lose protection between keystrokes.
test("delivery_hold: renews while the line stays uncommitted", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("a", 0), "pause");
  assert.equal(h.tick(500), "none", "too soon");
  assert.equal(h.tick(1_000), "pause");
  assert.equal(h.tick(1_500), "none");
  assert.equal(h.tick(2_000), "pause");
});

// The stated bound: a user who walks away mid-line does not block a queued
// delivery forever. The hold ends after the idle window; the daemon's own lease
// then expires and the delivery lands — delayed, never dropped.
test("delivery_hold: an abandoned-in-place line stops being renewed", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("half typed", 0), "pause");
  assert.equal(h.tick(14_999), "pause", "still within the bound, still protected");
  assert.equal(h.tick(15_000), "none", "bound reached: stop renewing and let the lease lapse");
  assert.equal(h.holding, false);
  assert.equal(h.tick(30_000), "none", "and it stays released");
});

// The bound is "untouched for N", not "held for N".
test("delivery_hold: continued typing pushes the idle bound out", () => {
  const h = new MidLineHold(1_000, 15_000);
  h.noteInput("a", 0);
  assert.equal(h.noteInput("b", 14_000), "pause", "input renews and resets the clock");
  assert.equal(h.tick(20_000), "pause", "not idle: 6s since the last keystroke");
  assert.equal(h.tick(29_000), "none", "now 15s since the last keystroke");
});

test("delivery_hold: release drops the hold without asking the daemon", () => {
  const h = new MidLineHold();
  h.noteInput("x", 0);
  assert.equal(h.holding, true);
  h.release();
  assert.equal(h.holding, false);
  assert.equal(h.tick(10_000), "none");
});

// #3025 review finding: with bracketed-paste mode on (agents enable it), xterm
// wraps a paste as ESC[200~ … ESC[201~ and sends it as ONE chunk. That starts with
// ESC, so a blanket ESC filter threw away a real pasted draft and took no lease,
// leaving a delivery free to append to it and submit.
test("delivery_hold: a bracketed paste is a draft, not a terminal report", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\x1b[200~deploy --to prod\x1b[201~", 0), "pause");
  assert.equal(h.holding, true);
});

// And the reason the payload is not scanned for a commit: inside a bracketed paste
// an embedded newline is LITERAL — the shell inserts it rather than executing — so
// a pasted block ending in one still leaves the user mid-draft.
test("delivery_hold: a bracketed paste ending in a newline still holds", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\x1b[200~line one\rline two\r\x1b[201~", 0), "pause");
  assert.equal(h.holding, true, "an embedded CR inside a paste is text, not Enter");
});

// An empty paste is not a draft.
test("delivery_hold: an empty bracketed paste creates no hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\x1b[200~\x1b[201~", 0), "none");
  assert.equal(h.holding, false);
});
