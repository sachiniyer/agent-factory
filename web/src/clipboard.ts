// Clipboard key handling for the web attach terminal — the web half of Sachin's
// "copying is not working the way I expect" report. His decision, verbatim:
//
//   "Copy on selection; Ctrl+C interrupts only when nothing is selected."
//
// Read as the terminal convention (NOT auto-copy-on-drag): the copy happens on
// the Ctrl+C GESTURE when a selection is present, so drag-selecting text never
// clobbers the clipboard on its own. Concretely:
//
//   - Ctrl+C with a selection → COPY the selection, do NOT send \x03.
//   - Ctrl+C with no selection → interrupt (\x03), exactly as before — the
//     runaway-agent reflex is preserved.
//   - Ctrl+Shift+C → an EXPLICIT always-copy: copies the selection if any, a
//     no-op otherwise, and NEVER interrupts.
//   - Ctrl+V / Ctrl+Shift+V → paste, by deferring to xterm's native browser
//     paste (see below).
//   - Shift+Enter in the AGENT tab → LF (Ctrl+J), which Codex and Claude bind to
//     composer newline; non-agent tabs and plain Enter stay on xterm's CR path.
//
// This is the DECISION half, kept pure and DOM-free so it unit-tests against
// what reaches the wire/clipboard rather than against a synthetic keydown.
// terminal.ts binds it to xterm via attachCustomKeyEventHandler and supplies the
// real capabilities (xterm selection, clipboard write, OpInput send).

/** The subset of a DOM KeyboardEvent this decision reads. A real KeyboardEvent
 *  satisfies it structurally; tests construct plain objects. */
export interface ClipboardKeyEvent {
  /** xterm's custom handler fires for keydown/keypress/keyup alike — we act only
   *  on "keydown" so a gesture is handled once, not three times. */
  type: string;
  key: string;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
  preventDefault(): void;
}

/** The side-effecting capabilities the decision drives. terminal.ts supplies real
 *  ones; tests supply spies so an assertion can read exactly what would reach the
 *  clipboard and the input frame. */
export interface ClipboardDeps {
  /** Whether this PTY is the agent composer rather than a shell/process tab. */
  composerNewline: boolean;
  /** Whether the terminal currently has a text selection (xterm.hasSelection). */
  hasSelection(): boolean;
  /** The selected text (xterm.getSelection). Only read when hasSelection() is true. */
  getSelection(): string;
  /** Clear the terminal's selection (xterm.clearSelection). Called after a Ctrl+C
   *  copy so the NEXT Ctrl+C interrupts (see the Ctrl+C branch). */
  clearSelection(): void;
  /** Copy text to the system clipboard, with a graceful, never-silent fallback. */
  copy(text: string): void;
  /** Send text to the PTY as OpInput — the same path onData uses for typed keys. */
  sendInput(text: string): void;
  /** Feed genuine typed input back through xterm so it runs onUserInput effects
   *  (scroll-to-bottom and selection clearing) before onData sends the bytes. */
  sendUserInput(text: string): void;
}

/** The Ctrl+C interrupt byte (End-of-Text), sent verbatim on the no-selection path. */
const ETX = "\x03";
/** Line Feed / Ctrl+J: the cross-agent composer-newline input (#2337). */
const LF = "\n";

/**
 * Decide what a key event does for clipboard vs. interrupt. Returns whether xterm
 * should keep its DEFAULT handling of the event:
 *
 *   - `true`  → not our gesture; let xterm/the browser process it as usual.
 *   - `false` → we fully handled it; xterm skips its own processing so it does not
 *     ALSO emit bytes for the key.
 *
 * Only Ctrl+* clipboard gestures and bare Shift+Enter in an agent composer are
 * claimed. Cmd+* (metaKey), Alt+*, and every Enter in a shell/process tab are left
 * untouched so their browser/xterm/application bindings keep working as before.
 *
 * Paste note: for Ctrl+V (and Ctrl+Shift+V) we return `false` WITHOUT calling
 * preventDefault. In xterm, a custom handler that returns false makes _keyDown
 * bail before its keymap runs — so xterm's own Ctrl+V→\x16 mapping is suppressed
 * — but it does NOT cancel the DOM event, so the browser still fires its trusted
 * `paste` event, which xterm's native paste handler forwards to the PTY. That is
 * a permission-free paste with no double input; navigator.clipboard.readText is
 * deliberately NOT used (it prompts on Chrome and is blocked on Firefox).
 */
export function handleClipboardKeydown(ev: ClipboardKeyEvent, deps: ClipboardDeps): boolean {
  // Act once per gesture: xterm's handler also fires for keypress/keyup, and
  // returning false there would suppress xterm's own keyup/keypress bookkeeping.
  if (ev.type !== "keydown") {
    return true;
  }
  // xterm maps Enter and Shift+Enter to the same CR, so the agent cannot otherwise
  // distinguish "newline" from "submit". Both shipped agent composers recognize
  // LF / Ctrl+J as newline without terminal-protocol negotiation. Claim ONLY the
  // bare Shift variant in the one agent tab: plain Enter and every non-agent tab
  // keep xterm's CR path, while Ctrl/Alt/Meta combinations remain available to the
  // application. Terminal.input(..., true) is deliberate: unlike a direct socket
  // write it fires xterm's onUserInput path, which scrolls to the prompt and clears
  // an active selection before onData sends the LF.
  if (
    deps.composerNewline &&
    ev.key === "Enter" &&
    ev.shiftKey &&
    !ev.ctrlKey &&
    !ev.metaKey &&
    !ev.altKey
  ) {
    ev.preventDefault();
    deps.sendUserInput(LF);
    return false;
  }
  // Leave Cmd+* (macOS) and Alt+* to the browser/xterm untouched.
  if (ev.metaKey || ev.altKey || !ev.ctrlKey) {
    return true;
  }

  const key = ev.key.toLowerCase();

  if (key === "v") {
    // Defer to xterm's native (browser-trusted) paste for both Ctrl+V and
    // Ctrl+Shift+V. Returning false suppresses xterm's Ctrl+V→\x16 keymap; NOT
    // calling preventDefault lets the browser's `paste` event flow to xterm.
    return false;
  }

  if (key === "c") {
    if (ev.shiftKey) {
      // Ctrl+Shift+C — explicit, unambiguous copy. Copies a selection if present,
      // a no-op otherwise; never interrupts. The selection is deliberately LEFT
      // intact: this is the "just copy, I want to keep selecting" gesture.
      ev.preventDefault();
      if (deps.hasSelection()) {
        deps.copy(deps.getSelection());
      }
      return false;
    }
    // Ctrl+C — a present selection means "copy it" (the terminal convention), so
    // copy and do NOT interrupt.
    if (deps.hasSelection()) {
      ev.preventDefault();
      deps.copy(deps.getSelection());
      // Clear the selection so the NEXT Ctrl+C interrupts. Without this, a user who
      // copies runaway output and then reaches for Ctrl+C to STOP the agent keeps
      // re-copying instead — the interrupt reflex the no-selection path exists for
      // would be unreachable until they manually deselect. xterm only auto-clears
      // the selection on its own onUserInput, which this path bypasses (we send via
      // our WS and return false), so we clear it explicitly.
      deps.clearSelection();
      return false;
    }
    // No selection: interrupt. Send \x03 ourselves and suppress xterm's own
    // Ctrl+C handling so the interrupt is emitted exactly once.
    ev.preventDefault();
    deps.sendInput(ETX);
    return false;
  }

  return true; // not our gesture
}
