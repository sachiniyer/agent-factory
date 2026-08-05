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
//   - Cmd+C (macOS) → copy the selection when there IS one; otherwise untouched.
//     Cmd+C is not an interrupt anywhere, so it never sends \x03 (#2787).
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
  /** The PHYSICAL key, layout-independent by definition ("KeyC" wherever the C key
   *  sits). Optional because a synthetic event may omit it; see chordIsLetter. */
  code?: string;
  ctrlKey: boolean;
  metaKey: boolean;
  shiftKey: boolean;
  altKey: boolean;
  preventDefault(): void;
}

/**
 * Does this event carry the given Latin letter as a CHORD key, on any layout?
 *
 * `ev.key` is the layout-mapped character and `ev.code` is the physical key, and
 * neither alone is right (#2831):
 *
 *   - `key` alone breaks every non-Latin layout. On Cyrillic the C key reports
 *     `key: "с"` (U+0441) and on Greek `"ψ"`; neither equals "c", so the gesture
 *     went unclaimed and the user got the original silent failure.
 *   - `code` alone breaks re-arranged Latin layouts. On Dvorak the key at the
 *     physical C position types "j", and Ctrl+J is a real terminal control code
 *     (LF) — claiming it as copy would break input to fix a layout nobody was on.
 *
 * So: trust `key` whenever it IS a Latin letter (every Latin layout, including
 * Dvorak and Colemak, where the letter the user typed is the letter they meant),
 * and fall back to the physical `code` only when it is not (the non-Latin layouts,
 * where `key` cannot express the chord at all).
 */
function chordIsLetter(ev: ClipboardKeyEvent, letter: string, code: string): boolean {
  const typed = ev.key.toLowerCase();
  if (typed.length === 1 && typed >= "a" && typed <= "z") {
    return typed === letter;
  }
  return ev.code === code;
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
 * Only Ctrl+* clipboard gestures, bare Cmd+C over a selection, and bare Shift+Enter
 * in an agent composer are claimed. Every other Cmd+* (metaKey), Alt+*, and every
 * Enter in a shell/process tab are left untouched so their browser/xterm/application
 * bindings keep working as before.
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
  // macOS copies with Cmd+C, and that gesture CANNOT be left to the browser (#2787).
  // xterm keeps its own selection model and paints an overlay — it never makes a DOM
  // selection — so the browser's native copy finds document.getSelection() empty and
  // writes NOTHING, silently, without ever reaching the never-silent copy ladder in
  // terminal.ts. (Cmd+V is the asymmetry that hid this: paste works because the
  // browser fires a trusted `paste` event xterm forwards to the PTY; copy has no
  // counterpart.) So claim the BARE gesture, and only when there is something to
  // copy:
  //
  //   - a selection → preventDefault + copy, through the same deps.copy ladder as
  //     Ctrl+C, so a failed write still surfaces a visible hint;
  //   - no selection → untouched. Cmd+C is not an interrupt on macOS, so this path
  //     must never emit \x03.
  //
  // #2831 asked whether handleTerminalCopy below could REPLACE this chord, since the
  // browser fires `copy` for the chord as well as for the menu routes. Measured in
  // Chromium, it cannot: the copy event fires for the PLATFORM copy chord only, so on
  // Linux `Control+c` dispatches one and `Meta+c` does not. Dropping this branch would
  // therefore trade a path verified on every platform for one that is unverifiable on
  // the platform CI runs, and would regress Cmd+C wherever Meta is not the copy
  // modifier. handleTerminalCopy is an ADDITION that covers the routes no chord
  // reaches; the chords below stay.
  //
  // The selection is deliberately KEPT. Clearing exists on Ctrl+C so a second press
  // falls through to the interrupt; Cmd+C has no interrupt to fall through to, so
  // clearing would just be a surprising selection loss.
  //
  // Bare only: Cmd+Shift+C is the browser's inspect-element toggle, and Cmd+Alt+C
  // devtools too — claiming those would trade one broken shortcut for another.
  // Cmd+V stays untouched; its trusted-paste path already works.
  if (
    ev.metaKey &&
    !ev.ctrlKey &&
    !ev.altKey &&
    !ev.shiftKey &&
    chordIsLetter(ev, "c", "KeyC")
  ) {
    if (!deps.hasSelection()) {
      return true;
    }
    ev.preventDefault();
    deps.copy(deps.getSelection());
    return false;
  }
  // Leave every other Cmd+* (macOS) and Alt+* to the browser/xterm untouched.
  if (ev.metaKey || ev.altKey || !ev.ctrlKey) {
    return true;
  }

  if (chordIsLetter(ev, "v", "KeyV")) {
    // Defer to xterm's native (browser-trusted) paste for both Ctrl+V and
    // Ctrl+Shift+V. Returning false suppresses xterm's Ctrl+V→\x16 keymap; NOT
    // calling preventDefault lets the browser's `paste` event flow to xterm.
    return false;
  }

  if (chordIsLetter(ev, "c", "KeyC")) {
    if (ev.shiftKey) {
      // Ctrl+Shift+C is the INSPECT-ELEMENT shortcut on Linux and Windows, exactly
      // as Cmd+Shift+C is on macOS — and the Cmd branch above already refuses to
      // claim it, on the grounds that doing so "would trade one broken shortcut for
      // another". This branch used to claim it anyway with a preventDefault, so the
      // #2787 fix protected devtools for macOS users while keeping it broken for
      // everyone else (#2831). The two branches now agree, and this returns `true`
      // exactly as that one does.
      //
      // `true` (not the `false` Ctrl+V uses) because xterm needs no suppressing
      // here. Measured against real xterm in Chromium: Ctrl+Shift+C emits NOTHING
      // on onData — the ctrl+letter→control-byte mapping does not apply with shift
      // held, unlike bare Ctrl+C which emits \x03 — and xterm leaves the event
      // uncancelled, so the browser still gets its shortcut. There is nothing to
      // suppress and nothing to preventDefault; the honest answer is "not our
      // gesture".
      //
      // Copy is not lost with it: Ctrl+C over a selection still copies, Cmd+C still
      // copies on macOS, and handleTerminalCopy covers the routes that reach the
      // terminal as a `copy` event instead of a chord.
      return true;
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

/** The subset of a DOM ClipboardEvent the copy decision reads. A real
 *  ClipboardEvent satisfies it structurally; tests construct plain objects. */
export interface TerminalCopyEvent {
  /** null when the platform gives no synchronous write surface — see the fallback. */
  clipboardData: { setData(format: string, data: string): void } | null;
  preventDefault(): void;
}

/** What the copy decision needs from the terminal. A strict subset of ClipboardDeps
 *  — no wire access at all, because copying can never send input. */
export interface TerminalCopyDeps {
  hasSelection(): boolean;
  getSelection(): string;
  /** The never-silent ladder in terminal.ts, used only when the event carries no
   *  clipboardData to write into. */
  copy(text: string): void;
}

/**
 * Put xterm's selection on the clipboard for ANY copy the browser initiates (#2831).
 *
 * This is the single copy path, and it is deliberately event-shaped rather than
 * chord-shaped. The browser fires `copy` for Cmd+C, for Ctrl+C on Linux/Windows,
 * for macOS Edit → Copy, for right-click → Copy, and for assistive technology that
 * copies without synthesizing a keydown. Before this, only the chords worked, so
 * every menu route failed the same silent way the original bug did: the browser
 * copied `document.getSelection()`, xterm had no DOM selection, nothing landed on
 * the clipboard, and no handler of ours ran to say so.
 *
 * Writing into `event.clipboardData` is what makes this work without a permission
 * prompt: inside a trusted `copy` event the write is synchronous and always allowed,
 * unlike navigator.clipboard.writeText.
 *
 * Returns whether the selection was written, so a caller (and a test) can tell the
 * handled case from the deliberate pass-through.
 */
export function handleTerminalCopy(ev: TerminalCopyEvent, deps: TerminalCopyDeps): boolean {
  // No terminal selection ⇒ not ours. Leave the event completely alone so a copy
  // aimed at ordinary page text (an error message, a session title) still works.
  if (!deps.hasSelection()) {
    return false;
  }
  const text = deps.getSelection();
  if (text === "") {
    return false;
  }
  // Ours either way: preventDefault stops the browser writing its own EMPTY document
  // selection over what we are about to put there.
  ev.preventDefault();
  if (ev.clipboardData) {
    ev.clipboardData.setData("text/plain", text);
    return true;
  }
  // No clipboardData (older/unusual embedders): fall back to the never-silent ladder
  // rather than dropping the copy, so a failure still surfaces a visible hint.
  deps.copy(text);
  return true;
}
