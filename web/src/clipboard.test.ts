// Pins the web terminal's clipboard/interrupt decision (the web half of Sachin's
// "copying is not working the way I expect" report). It asserts on WHAT REACHES
// THE WIRE and the clipboard — not on the synthetic keydown — by feeding
// handleClipboardKeydown spies that encode input exactly as terminal.ts does
// (encode(inputFrame(...))) and record clipboard writes. So a regression that,
// say, sent \x03 while ALSO copying, or dropped the interrupt, fails here.
//
// clipboard.ts is pure and DOM-free (the browser-only copy/paste plumbing lives
// in terminal.ts), so this needs no xterm stub — it imports the real codec.

import { test } from "node:test";
import assert from "node:assert/strict";

import { type ClipboardDeps, type ClipboardKeyEvent, handleClipboardKeydown } from "./clipboard.js";
import { decode, inputFrame, Op, encode } from "./frame.js";

const ETX = "\x03";

/** A recording rig: captures every frame that would hit the WS (as the exact
 *  bytes terminal.ts sends), every clipboard write, and preventDefault calls. */
function rig(opts: { selection?: string; composerNewline?: boolean }) {
  const enc = new TextEncoder();
  const wire: Uint8Array[] = [];
  const clipboard: string[] = [];
  const userInput: string[] = [];
  let prevented = 0;
  let cleared = 0;
  // Mutable so clearSelection() genuinely drops it — a later hasSelection() then
  // reports false, exactly as xterm behaves after a real clear.
  let selection = opts.selection ?? "";
  const deps: ClipboardDeps = {
    composerNewline: opts.composerNewline ?? true,
    hasSelection: () => selection !== "",
    getSelection: () => selection,
    clearSelection: () => {
      selection = "";
      cleared++;
    },
    copy: (t) => clipboard.push(t),
    // Byte-identical to terminal.ts's input path, so `wire` holds real OpInput frames.
    sendInput: (t) => wire.push(encode(inputFrame(enc.encode(t)))),
    sendUserInput: (t) => {
      userInput.push(t);
      wire.push(encode(inputFrame(enc.encode(t))));
    },
  };
  return {
    deps,
    wire,
    clipboard,
    userInput,
    prevented: () => prevented,
    cleared: () => cleared,
    markPrevented: () => {
      prevented++;
    },
  };
}

/** Builds a keydown-shaped event with a preventDefault spy. */
function keyEvent(
  init: Partial<ClipboardKeyEvent> & { key: string },
  onPrevent: () => void,
): ClipboardKeyEvent {
  return {
    type: "keydown",
    ctrlKey: false,
    metaKey: false,
    shiftKey: false,
    altKey: false,
    preventDefault: onPrevent,
    ...init,
  };
}

/** Decodes the captured wire frames to the concatenated input bytes (as a string),
 *  proving what actually reached the OpInput channel. */
function wireInput(frames: Uint8Array[]): string {
  const dec = new TextDecoder();
  let out = "";
  for (const raw of frames) {
    const f = decode(raw);
    assert.equal(f.op, Op.Input, "only OpInput frames are expected on this channel");
    out += dec.decode(f.data);
  }
  return out;
}

// --- Modified Enter: newline in agent composers, plain Enter still submits -----

test("agent Shift+Enter sends LF through xterm's user-input path and suppresses its default CR", () => {
  const r = rig({});
  const ret = handleClipboardKeydown(keyEvent({ key: "Enter", shiftKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, false, "xterm must not also turn Shift+Enter into a submitting CR");
  assert.deepEqual(r.userInput, ["\n"], "LF must traverse xterm's genuine-user-input side effects");
  assert.equal(wireInput(r.wire), "\n", "Codex and Claude both bind LF / Ctrl+J to composer newline");
  assert.equal(r.prevented(), 1, "the handled key must not retain browser-default behavior");
});

test("Shift+Enter stays xterm-owned outside the agent composer", () => {
  const r = rig({ composerNewline: false });
  const ret = handleClipboardKeydown(keyEvent({ key: "Enter", shiftKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, true, "shell/process tabs must retain xterm's existing CR mapping");
  assert.deepEqual(r.userInput, []);
  assert.equal(wireInput(r.wire), "", "the custom handler sends no replacement byte outside the agent tab");
  assert.equal(r.prevented(), 0);
});

test("plain Enter remains xterm-owned so it still submits as CR", () => {
  const r = rig({});
  const ret = handleClipboardKeydown(keyEvent({ key: "Enter" }, r.markPrevented), r.deps);

  assert.equal(ret, true, "plain Enter must keep xterm's existing CR path");
  assert.equal(wireInput(r.wire), "", "the custom handler must not duplicate plain Enter");
  assert.equal(r.prevented(), 0);
});

test("Ctrl/Alt/Meta combinations do not get mistaken for bare Shift+Enter", () => {
  for (const modifiers of [{ ctrlKey: true }, { altKey: true }, { metaKey: true }]) {
    const r = rig({});
    const ret = handleClipboardKeydown(
      keyEvent({ key: "Enter", shiftKey: true, ...modifiers }, r.markPrevented),
      r.deps,
    );

    assert.equal(ret, true, JSON.stringify(modifiers));
    assert.equal(wireInput(r.wire), "", JSON.stringify(modifiers));
    assert.equal(r.prevented(), 0, JSON.stringify(modifiers));
  }
});

// --- Ctrl+C: copy when there's a selection, interrupt when there isn't --------

test("Ctrl+C WITH a selection copies it and sends NO \\x03", () => {
  const r = rig({ selection: "hello world" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", ctrlKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, false, "must suppress xterm's own Ctrl+C so it does not also emit \\x03");
  assert.deepEqual(r.clipboard, ["hello world"], "the selection must reach the clipboard");
  assert.equal(wireInput(r.wire), "", "no interrupt on the wire when copying");
  assert.equal(r.prevented(), 1, "preventDefault stops the browser's own copy");
  assert.equal(r.cleared(), 1, "the selection is cleared so the NEXT Ctrl+C can interrupt");
});

test("a SECOND Ctrl+C after a copy interrupts — the runaway-agent reflex", () => {
  // The scenario the interrupt half exists for: copy some runaway output, then the
  // agent keeps going and the user reaches for Ctrl+C to STOP it. Because the first
  // Ctrl+C cleared the selection, the second one falls through to the interrupt.
  const r = rig({ selection: "runaway output" });

  const first = handleClipboardKeydown(keyEvent({ key: "c", ctrlKey: true }, r.markPrevented), r.deps);
  assert.equal(first, false);
  assert.deepEqual(r.clipboard, ["runaway output"], "first Ctrl+C copies");
  assert.equal(wireInput(r.wire), "", "first Ctrl+C does NOT interrupt");

  const second = handleClipboardKeydown(keyEvent({ key: "c", ctrlKey: true }, r.markPrevented), r.deps);
  assert.equal(second, false);
  assert.equal(wireInput(r.wire), ETX, "second Ctrl+C interrupts — exactly one \\x03");
  assert.deepEqual(r.clipboard, ["runaway output"], "the second press does NOT re-copy");
});

test("Ctrl+C with NO selection sends \\x03 on the wire and copies nothing", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", ctrlKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, false, "we emit the interrupt ourselves, so xterm must be suppressed");
  assert.equal(wireInput(r.wire), ETX, "the interrupt reflex is preserved — exactly one \\x03");
  assert.deepEqual(r.clipboard, [], "nothing to copy");
});

// --- Ctrl+Shift+C: explicit always-copy, never interrupts ---------------------

test("Ctrl+Shift+C copies the selection and never sends \\x03", () => {
  const r = rig({ selection: "abc" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "C", ctrlKey: true, shiftKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.deepEqual(r.clipboard, ["abc"]);
  assert.equal(wireInput(r.wire), "", "an explicit copy must never interrupt");
  assert.equal(r.prevented(), 1);
  assert.equal(r.cleared(), 0, "Ctrl+Shift+C keeps the selection — it is the 'keep selecting' gesture");
});

test("Ctrl+Shift+C with NO selection is a no-op (no copy, no interrupt)", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "C", ctrlKey: true, shiftKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false, "still claims the key so it never falls through to \\x03");
  assert.deepEqual(r.clipboard, []);
  assert.equal(wireInput(r.wire), "", "explicit copy is never an interrupt, even with nothing selected");
});

// --- Paste: defer to xterm's native (browser-trusted) paste -------------------

test("Ctrl+V defers to native paste: suppresses xterm's \\x16, sends nothing itself, no preventDefault", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(keyEvent({ key: "v", ctrlKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, false, "false suppresses xterm's Ctrl+V→\\x16 keymap");
  assert.equal(wireInput(r.wire), "", "we send no input ourselves — the native paste event does");
  assert.deepEqual(r.clipboard, [], "paste must not touch the clipboard write path");
  assert.equal(r.prevented(), 0, "must NOT preventDefault, or the browser's paste event never fires");
});

test("Ctrl+Shift+V also defers to native paste without preventDefault", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "V", ctrlKey: true, shiftKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.equal(wireInput(r.wire), "");
  assert.equal(r.prevented(), 0);
});

// --- macOS Cmd+* and Alt+* are left entirely to the browser -------------------

test("Cmd+C is NOT claimed (macOS browser copies before xterm)", () => {
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", metaKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, true, "leave Cmd+C to the browser");
  assert.deepEqual(r.clipboard, [], "our handler must not double-copy on macOS");
  assert.equal(wireInput(r.wire), "");
  assert.equal(r.prevented(), 0);
});

test("Cmd+V is NOT claimed (native paste path untouched)", () => {
  const r = rig({});
  const ret = handleClipboardKeydown(keyEvent({ key: "v", metaKey: true }, r.markPrevented), r.deps);
  assert.equal(ret, true);
  assert.equal(r.prevented(), 0);
});

// --- The keydown-only guard: a gesture is handled once, not per key phase ------

test("keyup for Ctrl+C is ignored (returns true, no side effects)", () => {
  const r = rig({ selection: "x" });
  const ev = keyEvent({ key: "c", ctrlKey: true }, r.markPrevented);
  ev.type = "keyup";
  const ret = handleClipboardKeydown(ev, r.deps);

  assert.equal(ret, true, "only keydown acts; keyup/keypress must fall through to xterm");
  assert.deepEqual(r.clipboard, [], "no duplicate copy on the keyup half of the gesture");
  assert.equal(wireInput(r.wire), "");
});

// --- Unrelated keys are never disturbed ---------------------------------------

test("a plain key and other Ctrl combos fall through untouched", () => {
  for (const ev of [
    keyEvent({ key: "a" }, () => {}),
    keyEvent({ key: "a", ctrlKey: true }, () => {}),
    keyEvent({ key: "d", ctrlKey: true }, () => {}), // Ctrl+D must still reach the PTY
  ]) {
    const r = rig({ selection: "sel" });
    ev.preventDefault = r.markPrevented;
    assert.equal(handleClipboardKeydown(ev, r.deps), true, `${ev.key} must fall through`);
    assert.deepEqual(r.clipboard, []);
    assert.equal(wireInput(r.wire), "");
    assert.equal(r.prevented(), 0);
  }
});
