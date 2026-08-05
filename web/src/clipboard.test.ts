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

import {
  type ClipboardDeps,
  type ClipboardKeyEvent,
  handleClipboardKeydown,
  handleTerminalCopy,
  type TerminalCopyDeps,
  type TerminalCopyEvent,
} from "./clipboard.js";
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

test("Ctrl+Shift+C is left entirely to the browser's devtools (#2831)", () => {
  // It used to be an explicit always-copy with a preventDefault. But Ctrl+Shift+C is
  // inspect-element on Linux and Windows, exactly as Cmd+Shift+C is on macOS — which
  // the Cmd branch already refused to claim for that reason. Claiming one and sparing
  // the other protected devtools for macOS users only.
  //
  // `true` is safe here, and that was measured rather than assumed: against real
  // xterm in Chromium, Ctrl+Shift+C emits nothing on onData (the ctrl+letter mapping
  // does not apply with shift held) and xterm leaves the event uncancelled. So there
  // is no keymap to suppress and the browser still gets its shortcut.
  for (const selection of ["sel", ""]) {
    const r = rig({ selection });
    const ret = handleClipboardKeydown(
      keyEvent({ key: "C", code: "KeyC", ctrlKey: true, shiftKey: true }, r.markPrevented),
      r.deps,
    );

    assert.equal(ret, true, `selection=${JSON.stringify(selection)}`);
    assert.equal(r.prevented(), 0, "the browser must still see the event to open devtools");
    assert.equal(wireInput(r.wire), "", "nothing may reach the PTY on a devtools keypress");
    assert.deepEqual(r.clipboard, [], "copying belongs to Ctrl+C, Cmd+C and the copy event now");
  }
});

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

// --- macOS Cmd+C: copy a selection, otherwise stay out of the way -------------
//
// #2831 asked whether the copy EVENT could replace this chord. Measured in Chromium:
// it cannot. The event fires for the PLATFORM copy chord only, so on Linux
// `Control+c` dispatches one and `Meta+c` does not — dropping this branch would
// regress Cmd+C wherever Meta is not the copy modifier, and leave the remaining path
// untested on the platform CI runs. handleTerminalCopy is an addition, not a
// replacement, and these stay.

test("Cmd+C WITH a selection copies it through the same never-silent ladder as Ctrl+C", () => {
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", metaKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, false, "we handled the copy, so xterm must not also process the key");
  assert.deepEqual(r.clipboard, ["sel"], "the selection must reach deps.copy — the browser cannot copy it for us");
  assert.equal(wireInput(r.wire), "", "Cmd+C is not an interrupt: nothing may reach the wire");
  assert.equal(r.prevented(), 1, "preventDefault stops the browser's own (empty) copy from racing ours");
  assert.equal(r.cleared(), 0, "Cmd+C keeps the selection — it has no interrupt to fall through to");
});

test("Cmd+C copies on a non-Latin layout too (#2831)", () => {
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "с", code: "KeyC", metaKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.deepEqual(r.clipboard, ["sel"], "a Cyrillic layout must copy on Cmd+C as well as Ctrl+C");
});

test("Cmd+C with NO selection is left untouched and NEVER interrupts", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", metaKey: true }, r.markPrevented), r.deps);

  assert.equal(ret, true, "nothing to copy — leave the gesture to the browser");
  assert.deepEqual(r.clipboard, []);
  assert.equal(wireInput(r.wire), "", "on macOS Cmd+C is not an interrupt: \\x03 here would kill the agent");
  assert.equal(r.prevented(), 0);
});

test("Cmd+Shift+C, Cmd+Alt+C and Cmd+V stay with the browser", () => {
  for (const init of [
    { key: "c", shiftKey: true },
    { key: "c", altKey: true },
    { key: "v" },
  ]) {
    const r = rig({ selection: "sel" });
    const ret = handleClipboardKeydown(keyEvent({ ...init, metaKey: true }, r.markPrevented), r.deps);

    assert.equal(ret, true, JSON.stringify(init));
    assert.deepEqual(r.clipboard, [], `only the BARE Cmd+C is ours: ${JSON.stringify(init)}`);
    assert.equal(wireInput(r.wire), "", JSON.stringify(init));
    assert.equal(r.prevented(), 0, JSON.stringify(init));
  }
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

// --- Layout independence (#2831) ----------------------------------------------
//
// `ev.key` is the layout-mapped character, `ev.code` the physical key. Matching on
// `key` alone reproduced the original silent failure for anyone on a non-Latin
// layout; matching on `code` alone would break Dvorak, where the physical C
// position types "j" and Ctrl+J is a real control code. So: `key` when it is a
// Latin letter, `code` only when it cannot be.

test("Ctrl+C copies on a Cyrillic layout, where key is 'с' (U+0441) not 'c'", () => {
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "с", code: "KeyC", ctrlKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.deepEqual(r.clipboard, ["sel"], "a Cyrillic layout must copy, not fall through silently");
  assert.equal(wireInput(r.wire), "");
});

test("Ctrl+C still INTERRUPTS on a non-Latin layout when nothing is selected", () => {
  const r = rig({ selection: "" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "ψ", code: "KeyC", ctrlKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.equal(wireInput(r.wire), ETX, "the runaway-agent reflex must survive the layout too");
});

test("Ctrl+V defers to native paste on a non-Latin layout", () => {
  const r = rig({});
  const ret = handleClipboardKeydown(
    keyEvent({ key: "м", code: "KeyV", ctrlKey: true }, r.markPrevented),
    r.deps,
  );
  assert.equal(ret, false, "xterm's \\x16 must still be suppressed");
  assert.equal(r.prevented(), 0, "and the browser's trusted paste must still fire");
});

test("a Latin layout is matched by the TYPED letter, so Dvorak's Ctrl+J stays Ctrl+J", () => {
  // On Dvorak the key at the physical C position types "j". Ctrl+J is LF — a real
  // terminal control code — so `code === "KeyC"` must NOT win over a Latin `key`.
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "j", code: "KeyC", ctrlKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, true, "Ctrl+J is the application's, not ours");
  assert.deepEqual(r.clipboard, [], "typing J must never copy");
  assert.equal(r.prevented(), 0);
});

test("the letter the user TYPED wins: Dvorak Ctrl+C copies though its code is KeyI", () => {
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(
    keyEvent({ key: "c", code: "KeyI", ctrlKey: true }, r.markPrevented),
    r.deps,
  );

  assert.equal(ret, false);
  assert.deepEqual(r.clipboard, ["sel"]);
});

test("an event with no code at all still works on a Latin layout", () => {
  // `code` is optional: a synthetic event may omit it entirely.
  const r = rig({ selection: "sel" });
  const ret = handleClipboardKeydown(keyEvent({ key: "c", ctrlKey: true }, r.markPrevented), r.deps);
  assert.equal(ret, false);
  assert.deepEqual(r.clipboard, ["sel"]);
});

// --- The copy EVENT: every route, not just the chord (#2831) ------------------

/** A copy-event rig: records what would be written into the system clipboard via
 *  clipboardData, and what fell back to the never-silent ladder. */
function copyRig(opts: { selection?: string; withClipboardData?: boolean }) {
  const written: Array<[string, string]> = [];
  const ladder: string[] = [];
  let prevented = 0;
  const selection = opts.selection ?? "";
  const deps: TerminalCopyDeps = {
    hasSelection: () => selection !== "",
    getSelection: () => selection,
    copy: (t) => ladder.push(t),
  };
  const ev: TerminalCopyEvent = {
    clipboardData:
      opts.withClipboardData === false
        ? null
        : {
            setData: (format, data) => {
              written.push([format, data]);
            },
          },
    preventDefault: () => {
      prevented++;
    },
  };
  return { ev, deps, written, ladder, prevented: () => prevented };
}

test("a copy over a terminal selection writes it into clipboardData", () => {
  // This is the whole point of #2831: the browser fires `copy` for the chord AND
  // for macOS Edit -> Copy, right-click -> Copy and assistive tech, so one handler
  // covers routes no key chord ever saw.
  const r = copyRig({ selection: "hello world" });
  const handled = handleTerminalCopy(r.ev, r.deps);

  assert.equal(handled, true);
  assert.deepEqual(r.written, [["text/plain", "hello world"]]);
  assert.equal(r.prevented(), 1, "without preventDefault the browser overwrites us with its empty selection");
  assert.deepEqual(r.ladder, [], "clipboardData is the permission-free path; no ladder needed");
});

test("a copy with NO terminal selection is left entirely alone", () => {
  // Ordinary page text (an error message, a session title) must still copy.
  const r = copyRig({ selection: "" });
  const handled = handleTerminalCopy(r.ev, r.deps);

  assert.equal(handled, false);
  assert.deepEqual(r.written, []);
  assert.equal(r.prevented(), 0, "preventDefault here would break copying the rest of the page");
});

test("a copy event with no clipboardData falls back to the never-silent ladder", () => {
  const r = copyRig({ selection: "sel", withClipboardData: false });
  const handled = handleTerminalCopy(r.ev, r.deps);

  assert.equal(handled, true);
  assert.deepEqual(r.ladder, ["sel"], "the copy must not be silently dropped");
  assert.equal(r.prevented(), 1);
});
