package termpane

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	uv "github.com/charmbracelet/ultraviolet"
)

// This file is the bubbletea-key → PTY-bytes boundary for interactive mode
// (#1089 PR 2, RFC §2.3/§2.4). Every focused tea.KeyMsg is translated into
// exactly one of:
//
//   - emulator key events (vt's SendKey encoder) — the default path, because
//     the emulator knows the terminal's current modes: DECCKM flips arrows
//     between CSI and SS3, bracketed paste wraps pastes, and so on. No
//     hand-written escape sequences where a mode could change the answer.
//   - a pre-encoded xterm sequence — ONLY for the modifier+navigation
//     family (ctrl/alt/shift arrows, Home/End, PgUp/PgDn, Insert/Delete),
//     which the pinned x/vt SendKey does not encode (its modifier support is
//     an upstream TODO) and whose xterm encoding (`CSI 1;{mod}X` /
//     `CSI {n};{mod}~`) is mode-INDEPENDENT, so pre-encoding cannot go stale
//     against the emulator's state.
//   - nothing — the input contract of #1089: a key this table does not cover
//     is IGNORED, never guessed into a wrong byte sequence. The known
//     ignored set (F13-F20, ctrl+? ) is listed on translateKey.
//
// The table is pinned byte-for-byte by TestKeyTranslationTable.

// translated is the encoding decision for one key message: exactly one of
// events/raw is set. events go through emu.SendKey (mode-aware); raw is
// written to the emulator's input pipe verbatim.
type translated struct {
	events []uv.KeyEvent
	raw    string
}

// modNavKey describes one member of the modifier+navigation family in xterm
// terms: cursor-class keys encode as `CSI 1;{mod}{final}`, tilde-class keys
// as `CSI {num};{mod}~`, where mod = 1 + shift(1) + alt(2) + ctrl(4).
type modNavKey struct {
	final byte // 'A'..'D', 'H', 'F' — cursor class; 0 for tilde class
	num   int  // tilde class: 2 ins, 3 del, 5 pgup, 6 pgdn
	shift bool
	ctrl  bool
}

// modNavKeys maps every bubbletea modified-navigation KeyType (plus the
// unmodified ones, for the msg.Alt variants) to its xterm encoding parts.
// bubbletea v1 has no alt+nav KeyTypes — alt arrives as the Alt flag on the
// base type — so alt contributes via the flag only, never the table.
var modNavKeys = map[tea.KeyType]modNavKey{
	tea.KeyUp:    {final: 'A'},
	tea.KeyDown:  {final: 'B'},
	tea.KeyRight: {final: 'C'},
	tea.KeyLeft:  {final: 'D'},
	tea.KeyHome:  {final: 'H'},
	tea.KeyEnd:   {final: 'F'},

	tea.KeyShiftUp:    {final: 'A', shift: true},
	tea.KeyShiftDown:  {final: 'B', shift: true},
	tea.KeyShiftRight: {final: 'C', shift: true},
	tea.KeyShiftLeft:  {final: 'D', shift: true},
	tea.KeyShiftHome:  {final: 'H', shift: true},
	tea.KeyShiftEnd:   {final: 'F', shift: true},

	tea.KeyCtrlUp:    {final: 'A', ctrl: true},
	tea.KeyCtrlDown:  {final: 'B', ctrl: true},
	tea.KeyCtrlRight: {final: 'C', ctrl: true},
	tea.KeyCtrlLeft:  {final: 'D', ctrl: true},
	tea.KeyCtrlHome:  {final: 'H', ctrl: true},
	tea.KeyCtrlEnd:   {final: 'F', ctrl: true},

	tea.KeyCtrlShiftUp:    {final: 'A', shift: true, ctrl: true},
	tea.KeyCtrlShiftDown:  {final: 'B', shift: true, ctrl: true},
	tea.KeyCtrlShiftRight: {final: 'C', shift: true, ctrl: true},
	tea.KeyCtrlShiftLeft:  {final: 'D', shift: true, ctrl: true},
	tea.KeyCtrlShiftHome:  {final: 'H', shift: true, ctrl: true},
	tea.KeyCtrlShiftEnd:   {final: 'F', shift: true, ctrl: true},

	tea.KeyInsert:     {num: 2},
	tea.KeyDelete:     {num: 3},
	tea.KeyPgUp:       {num: 5},
	tea.KeyPgDown:     {num: 6},
	tea.KeyCtrlPgUp:   {num: 5, ctrl: true},
	tea.KeyCtrlPgDown: {num: 6, ctrl: true},
}

// xtermSeq renders the modNavKey as its xterm escape sequence, with alt from
// the message's Alt flag folded into the modifier parameter.
func (k modNavKey) xtermSeq(alt bool) string {
	mod := 1
	if k.shift {
		mod++
	}
	if alt {
		mod += 2
	}
	if k.ctrl {
		mod += 4
	}
	if k.final != 0 {
		return fmt.Sprintf("\x1b[1;%d%c", mod, k.final)
	}
	return fmt.Sprintf("\x1b[%d;%d~", k.num, mod)
}

// simpleKeys are the unmodified specials vt's SendKey encodes natively
// (mode-aware where it matters: DECCKM arrows, keypad). The unmodified nav
// keys appear BOTH here and in modNavKeys: without Alt they take this
// mode-aware path, with Alt they take the pre-encoded modifier path.
var simpleKeys = map[tea.KeyType]rune{
	tea.KeyEnter:     uv.KeyEnter,
	tea.KeyTab:       uv.KeyTab,
	tea.KeyBackspace: uv.KeyBackspace,
	tea.KeyEscape:    uv.KeyEscape,
	tea.KeyUp:        uv.KeyUp,
	tea.KeyDown:      uv.KeyDown,
	tea.KeyRight:     uv.KeyRight,
	tea.KeyLeft:      uv.KeyLeft,
	tea.KeyHome:      uv.KeyHome,
	tea.KeyEnd:       uv.KeyEnd,
	tea.KeyPgUp:      uv.KeyPgUp,
	tea.KeyPgDown:    uv.KeyPgDown,
	tea.KeyInsert:    uv.KeyInsert,
	tea.KeyDelete:    uv.KeyDelete,
}

// ctrlPunct maps the non-letter control KeyTypes vt's SendKey encodes from a
// (rune, ModCtrl) event. ctrl+] (0x1d) is deliberately ABSENT: it is the
// host's only reserved key while interactive (RFC §2.3) and the app router
// never forwards it, so encoding it here would only mask a routing bug.
// (ctrl+[ needs no entry — its KeyType IS tea.KeyEscape, covered above; same
// for ctrl+? which IS tea.KeyBackspace.)
var ctrlPunct = map[tea.KeyType]rune{
	tea.KeyCtrlAt:         uv.KeySpace, // ctrl+@ / ctrl+space → NUL
	tea.KeyCtrlBackslash:  '\\',
	tea.KeyCtrlCaret:      '^',
	tea.KeyCtrlUnderscore: '_',
}

// translateKey maps one bubbletea key message to its PTY encoding. ok=false
// means the key has no safe encoding and must be ignored — the #1089 input
// contract ("key ignored", never a wrong byte sequence). Known-ignored today:
// F13-F20 (no portable xterm encoding for shifted F-keys across terminals),
// ctrl+? (DEL collides with backspace handling), ctrl+] (host-reserved,
// filtered by the app router before this is ever consulted), and pastes,
// which the caller routes through the emulator's bracketed-paste-aware Paste
// before translation.
func translateKey(msg tea.KeyMsg) (translated, bool) {
	mod := uv.KeyMod(0)
	if msg.Alt {
		mod |= uv.ModAlt
	}

	// Literal text: one event per rune. vt's SendKey emits the rune as-is,
	// with the legacy ESC prefix when Alt is held.
	if msg.Type == tea.KeyRunes {
		events := make([]uv.KeyEvent, 0, len(msg.Runes))
		for _, r := range msg.Runes {
			events = append(events, uv.KeyPressEvent{Code: r, Mod: mod})
		}
		return translated{events: events}, true
	}
	if msg.Type == tea.KeySpace {
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: uv.KeySpace, Mod: mod}}}, true
	}

	// Modifier+navigation family (including Alt on a plain nav key):
	// pre-encoded xterm sequences, because the pinned vt SendKey drops
	// modified nav keys (upstream TODO) and their encoding is mode-free.
	if nav, isNav := modNavKeys[msg.Type]; isNav && (msg.Alt || nav.shift || nav.ctrl) {
		return translated{raw: nav.xtermSeq(msg.Alt)}, true
	}

	// Unmodified specials: the emulator's mode-aware encoder.
	if code, isSimple := simpleKeys[msg.Type]; isSimple {
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: code, Mod: mod}}}, true
	}

	switch {
	case msg.Type == tea.KeyShiftTab:
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: uv.KeyTab, Mod: mod | uv.ModShift}}}, true

	// bubbletea's special KeyTypes are DESCENDING negative constants, so F12
	// compares smaller than F1.
	case msg.Type <= tea.KeyF1 && msg.Type >= tea.KeyF12:
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: uv.KeyF1 + rune(tea.KeyF1-msg.Type), Mod: mod}}}, true

	// ctrl+letter: the tea v1 KeyType for these IS the ASCII control code
	// (ctrl+a == 1 … ctrl+z == 26). Tab (ctrl+i), enter (ctrl+m) and escape
	// (ctrl+[) were consumed by the cases above, so this range is exactly
	// the remaining control letters.
	case msg.Type >= tea.KeyCtrlA && msg.Type <= tea.KeyCtrlZ:
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: 'a' + rune(msg.Type-tea.KeyCtrlA), Mod: mod | uv.ModCtrl}}}, true
	}

	if code, isCtrl := ctrlPunct[msg.Type]; isCtrl {
		return translated{events: []uv.KeyEvent{uv.KeyPressEvent{Code: code, Mod: mod | uv.ModCtrl}}}, true
	}

	return translated{}, false
}
