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

// #3025 review: xterm's onData carries focus reports, mouse reports and query
// replies. A hold created by merely focusing or scrolling would defer deliveries
// with no draft anywhere, so REPORTS are ignored outright.
test("delivery_hold: terminal reports never create a hold", () => {
  const h = new MidLineHold();
  for (const seq of ["\x1b[I", "\x1b[O", "\x1b[<0;10;5M", "\x1b[M abc", "\x1b[?1;2c", "\x1b[12;40R", "\x1b]11;rgb:0/0/0\x07"]) {
    assert.equal(h.noteInput(seq, 0), "none", `${JSON.stringify(seq)} is the terminal talking, not the user`);
    assert.equal(h.holding, false);
  }
});

// …but arrows, Delete and Home/End are the user EDITING, and they are the only
// input a draft receives while it is being revised. Ignoring them let a draft go
// idle and lose its lease while someone was actively working on it — so they can
// START a hold, not merely renew one.
test("delivery_hold: editing keys can start a hold", () => {
  for (const key of ["\x1b[A", "\x1b[3~", "\x1b[D", "\x1bOH"]) {
    const h = new MidLineHold();
    assert.equal(h.noteInput(key, 0), "pause", `${JSON.stringify(key)} is the user editing`);
    assert.equal(h.holding, true);
  }
});

// The case that motivated it: the idle bound releases a draft that is still in the
// PTY, the user comes back to it with cursor keys, and the lease must be re-taken.
test("delivery_hold: editing after the idle bound re-acquires the lease", () => {
  const h = new MidLineHold(1_000, 15_000);
  h.noteInput("half a thought", 0);
  assert.equal(h.tick(15_000), "none", "idle bound released it");
  assert.equal(h.holding, false);

  assert.equal(h.noteInput("\x1b[D", 20_000), "pause", "the draft is still in the PTY and is being edited again");
  assert.equal(h.holding, true);
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

test("editing an existing draft with cursor keys is activity, not idleness (#3025)", () => {
  const hold = new MidLineHold(1_000, 15_000);
  assert.equal(hold.noteInput("ls -la", 0), "pause");

  // Fifteen seconds of real editing: arrows and Delete, which xterm sends as
  // ESC-prefixed sequences. The earlier version returned before stamping the
  // activity clock, so this looked like a user who had walked away.
  for (let t = 1_000; t <= 15_000; t += 1_000) {
    hold.noteInput("\x1b[D", t);
    hold.noteInput("\x1b[3~", t + 100);
  }
  // The property is that the draft SURVIVES, not that any particular tick renews:
  // the last edit renewed at 15_100, so a tick 400ms later is simply not due yet.
  assert.equal(hold.holding, true, "a draft under active editing must not be released as idle");
  assert.equal(hold.tick(16_200), "pause", "and the lease keeps being extended while editing continues");
  assert.equal(hold.holding, true);

  // Only genuine idleness ends it: 15s after the LAST edit, not 15s after the
  // last printable character.
  assert.equal(hold.tick(31_000), "none");
  assert.equal(hold.holding, false, "a user who walks away mid-line is released by the idle bound");
});

test("ESC input renews a hold but never starts one (#3025)", () => {
  const hold = new MidLineHold(1_000, 15_000);
  // Focus reports, mouse reports and query replies arrive on the same callback.
  // With no draft anywhere they must not defer a cron delivery.
  assert.equal(hold.noteInput("\x1b[I", 0), "none");
  assert.equal(hold.noteInput("\x1b[<0;10;5M", 10), "none");
  assert.equal(hold.holding, false, "merely focusing or clicking must not create a hold");

  assert.equal(hold.noteInput("x", 20), "pause");
  assert.equal(hold.noteInput("\x1b[D", 1_100), "pause", "now the same bytes renew");
});

test("a composer newline on an empty prompt starts a hold (#3025)", () => {
  const hold = new MidLineHold(1_000, 15_000);
  // Shift+Enter sends a bare LF: it inserts a composer line without submitting,
  // so a draft now exists even though nothing printable was typed.
  assert.equal(hold.noteInput("\n", 0), "pause");
  assert.equal(hold.holding, true, "the composer holds an uncommitted line");

  // And it still does not COMMIT: Enter (CR) is the only thing that does.
  assert.equal(hold.noteInput("draft text\n", 100), "none");
  assert.equal(hold.holding, true, "LF mid-draft must not end the hold");
  hold.noteInput("\r", 200);
  assert.equal(hold.holding, false, "CR commits");
});

test("a commit that was only queued does not end the hold (#3025)", () => {
  const hold = new MidLineHold(1_000, 15_000);
  assert.equal(hold.noteInput("deploy prod", 0), "pause");

  // The stream drops and Enter is queued rather than delivered. The PTY still
  // holds "deploy prod"; nothing has committed there.
  assert.equal(hold.noteQueued("\r", 1_100), "pause");
  assert.equal(hold.holding, true, "the partial line is still in the PTY — keep protecting it");

  // Only a commit that actually reached the PTY ends it.
  hold.noteInput("\r", 2_200);
  assert.equal(hold.holding, false);
});

// #3025 review: the queued commit must be applied once the flush puts it on the
// wire. Until then the line is still sitting unsubmitted in the PTY and the hold is
// protecting it — but after, the draft is gone and continuing to renew defers an
// automated delivery to its next cron tick for no reason.
test("delivery_hold: a flushed queued commit ends the hold", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("deploy prod", 0), "pause");
  assert.equal(h.noteQueued("\r", 1_100), "pause", "queued Enter keeps protecting the line in the PTY");
  assert.equal(h.holding, true);

  h.noteFlushed(1_200);
  assert.equal(h.holding, false, "the commit has reached the PTY; there is no draft left to protect");
});

// …but a flush that carried no commit must NOT end the hold: the draft is still
// there, now actually in the PTY, which is exactly when it needs protecting.
test("delivery_hold: a flush with no commit keeps the hold", () => {
  const h = new MidLineHold(1_000, 15_000);
  assert.equal(h.noteInput("deploy", 0), "pause");
  assert.equal(h.noteQueued(" --to prod", 1_100), "pause", "renew interval elapsed");
  h.noteFlushed(1_200);
  assert.equal(h.holding, true, "the flushed bytes were a draft, not a commit");
});

// Two queued runs: the LAST one decides, the same rule live input follows.
test("delivery_hold: a later queued draft overrides an earlier queued commit", () => {
  const h = new MidLineHold(1_000, 15_000);
  h.noteInput("one", 0);
  h.noteQueued("\r", 1_100);
  h.noteQueued("two", 1_200);
  h.noteFlushed(1_300);
  assert.equal(h.holding, true, "the queue ends mid-line, so the flush leaves a draft in the PTY");
});

// #3025 review: a paste of only newlines into an empty composer is still content —
// xterm normalises them to CR before wrapping, and inside a bracketed paste a CR is
// literal text the application inserts rather than Enter.
test("delivery_hold: a bracketed paste of only newlines is a draft", () => {
  const h = new MidLineHold();
  assert.equal(h.noteInput("\x1b[200~\r\r\x1b[201~", 0), "pause");
  assert.equal(h.holding, true);
});

// #3025 review: a report arriving while the socket is down must not start a hold
// either — noteFlushed leaves a commit-less flush holding, so it would renew to the
// idle bound with nothing to protect.
test("delivery_hold: a queued terminal report starts no hold", () => {
  const h = new MidLineHold();
  assert.equal(h.noteQueued("\x1b[I", 0), "none");
  assert.equal(h.holding, false);
});
