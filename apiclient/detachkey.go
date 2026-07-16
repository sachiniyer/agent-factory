package apiclient

import (
	"bytes"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Detach-key recognition across keyboard-encoding modes (#1832).
//
// Full-screen attach is a RAW byte proxy: the pane program's output reaches the
// real terminal untouched (the same property #845 designs around for
// alt-screen/mouse modes). That includes the sequences a modern agent CLI emits
// at startup to negotiate a richer keyboard encoding — claude sends BOTH:
//
//	ESC [ > 1 u      kitty keyboard protocol, "disambiguate escape codes"
//	ESC [ > 4 ; 2 m  xterm modifyOtherKeys level 2
//
// A terminal that honors either one STOPS sending ctrl+<key> as a legacy C0
// control byte. Per the kitty spec, disambiguate mode reports "Esc, alt+key,
// ctrl+key, ctrl+alt+key, shift+alt+key ... using CSI u sequences instead of
// legacy ones" (only Enter/Tab/Backspace stay legacy). So on kitty the detach
// key arrives as ESC [ 119 ; 5 u, and under modifyOtherKeys as
// ESC [ 27 ; 5 ; 119 ~ — never as 0x17. Matching only the legacy byte silently
// forwarded it to the agent (where ctrl+w is a harmless word-erase) and the
// user could never leave the attach: #1832, reported on macOS + kitty + claude.
//
// This did not bite before #1592 Phase 2 PR7 (fc6a4d1): local attach ran through
// a real `tmux attach-session` client, and tmux — a terminal emulator in its own
// right — consumed the inner program's keyboard-mode requests instead of relaying
// them to the outer terminal, so the real terminal stayed in legacy mode. The
// clientless WS proxy has no such buffer.
//
// Rather than strip the pane program's negotiated mode (which would silently
// degrade its key handling — shift+enter and friends — for everyone), the detach
// key is recognized in every encoding a pane program can put the terminal into.
// Detection stays a SUFFIX test on the read chunk, preserving the #975 rule: a
// terminal delivers one keypress in one write, and batched leading bytes are
// forwarded as INPUT before the detach fires.
//
// The encodings are not one-to-one with the binding's character, so matching is a
// small set of (key code, modifier set) pairs per binding rather than a single
// codepoint — see detachKeyEncodingFor. Once the detach fires,
// tmux.NeutralTerminalRestore drops the negotiated modes back to legacy, so the
// terminal the user lands back in reports Ctrl keys normally again.

// kitty modifier bits (spec: the encoded param is bits+1). caps_lock and
// num_lock are *state* bits the terminal may OR in at any time, so they are
// masked off before comparing — a user with num-lock on must still be able to
// detach.
const (
	kbModShift   = 1
	kbModAlt     = 2
	kbModCtrl    = 4
	kbModCapsOn  = 64
	kbModNumOn   = 128
	kbModLockMsk = kbModCapsOn | kbModNumOn
)

// kitty event types, reported as a sub-param of the modifier param when the pane
// program turns on the "report event types" progressive-enhancement flag.
const (
	kbEventPress   = 1
	kbEventRepeat  = 2
	kbEventRelease = 3
)

// shiftRule says how the shift modifier relates to one key code. It is a
// property of the CODE, not of the binding: ctrl-_ is reported as '_' (shift
// irrelevant — some layouts have it as a base key) or as the unshifted '-' key,
// which is only the detach key WITH shift, because Ctrl+- unshifted is a
// different key the pane should receive.
type shiftRule int

const (
	// shiftForbidden: the character needs no shift, so a shift bit means the user
	// pressed something else. ctrl+shift+w is not the ctrl-w detach key.
	shiftForbidden shiftRule = iota
	// shiftOptional: the code IS the binding's character, however it was typed —
	// modifyOtherKeys reports '_' with shift, a layout with underscore as a base
	// key reports it without.
	shiftOptional
	// shiftRequired: the code is the unshifted physical key, which only produces
	// the binding's character when shifted. Ctrl+Shift+- is ctrl-_; Ctrl+- is not.
	shiftRequired
)

// detachKeyCode is one key code that can name the detach key, with the shift
// rule that makes it that key rather than a neighbouring one.
type detachKeyCode struct {
	code  int
	shift shiftRule
}

// detachKeyEncoding is how the configured detach key looks once the terminal has
// left legacy mode: the codes that identify it, each with its own shift rule.
//
// This exists because the C0 byte a binding parses to is NOT what an upgraded
// terminal reports. ctrl-w's byte 0x17 collapses key and modifier into one value;
// kitty reports them separately, as the key the user physically pressed plus a
// modifier set. For ctrl-^ and ctrl-_ they come apart entirely: on a US layout
// those characters are typed as Ctrl+Shift+6 and Ctrl+Shift+-, and kitty reports
// the UNSHIFTED key code with the shift bit set — never codepoint 94/95 with ctrl
// alone.
type detachKeyEncoding struct {
	codes []detachKeyCode
}

// matches reports whether code, pressed with mods, is this detach key.
func (e detachKeyEncoding) matches(code, mods int, maskLocks bool) bool {
	for _, c := range e.codes {
		if c.code == code && modsMatch(mods, c.shift, maskLocks) {
			return true
		}
	}
	return false
}

// detachKeyEncodingFor maps the detach key's C0 control byte to its encoded
// forms. config.ParseDetachKey only ever produces ctrl-<letter> (1..26),
// ctrl-[ (27), or ctrl-\ ] ^ _ (28..31).
//
// ctrl-[ is byte 27 — the same byte as Esc, which is why the LEGACY match cannot
// tell the two apart and a ctrl-[ user accepts that bare Esc detaches. The
// encoded forms have no such ambiguity: kitty reports the physical Ctrl+[ as
// CSI 91;5u and bare Esc as CSI 27u, so matching '[' here detaches on the real
// binding without ever hijacking the Esc the agent needs.
func detachKeyEncodingFor(b byte) (detachKeyEncoding, bool) {
	plain := func(code int) (detachKeyEncoding, bool) {
		return detachKeyEncoding{codes: []detachKeyCode{{code, shiftForbidden}}}, true
	}
	// shifted describes a binding whose character needs shift on a US layout:
	// the character itself (however typed) or the physical key it lives on, which
	// is only this binding when shifted.
	shifted := func(char, key int) (detachKeyEncoding, bool) {
		return detachKeyEncoding{codes: []detachKeyCode{
			{char, shiftOptional},
			{key, shiftRequired},
		}}, true
	}

	switch {
	case b >= 1 && b <= 26:
		return plain(int(b) + 'a' - 1) // ctrl-a..ctrl-z
	case b == 27:
		return plain('[')
	case b == 28:
		return plain('\\')
	case b == 29:
		return plain(']')
	case b == 30:
		return shifted('^', '6') // Ctrl+Shift+6 — Ctrl+6 alone is a different key
	case b == 31:
		return shifted('_', '-') // Ctrl+Shift+- — Ctrl+- alone is a different key
	default:
		return detachKeyEncoding{}, false
	}
}

// modsMatch reports whether an encoded modifier param is the modifier set for a
// key code with this shift rule: ctrl, shift exactly as the rule demands, and
// nothing else once lock state is masked away. Any other bit (alt, super, hyper,
// meta) makes it a different key the agent may want, so it must not detach.
func modsMatch(param int, rule shiftRule, maskLocks bool) bool {
	if param < 1 {
		return false
	}
	bits := param - 1
	if maskLocks {
		bits &^= kbModLockMsk
	}
	if bits&kbModCtrl == 0 || bits&^(kbModCtrl|kbModShift) != 0 {
		return false
	}
	hasShift := bits&kbModShift != 0
	switch rule {
	case shiftForbidden:
		return !hasShift
	case shiftRequired:
		return hasShift
	default: // shiftOptional
		return true
	}
}

// csiParams splits the parameter bytes of a CSI sequence into top-level params,
// each with its own sub-parameters. An empty field yields -1 ("default"),
// matching terminal convention; a non-numeric field makes the whole sequence
// unparseable and returns nil.
//
// Sub-params are kept rather than dropped because kitty puts load-bearing values
// there: the key param carries ":<shifted-key>:<base-layout-key>" when the
// report-alternate-keys flag is on, and the modifier param carries
// ":<event-type>" when event reporting is on.
func csiParams(b []byte) [][]int {
	if len(b) == 0 {
		return nil
	}
	var out [][]int
	for _, field := range bytes.Split(b, []byte{';'}) {
		var subs []int
		for _, s := range bytes.Split(field, []byte{':'}) {
			if len(s) == 0 {
				subs = append(subs, -1)
				continue
			}
			n := 0
			for _, c := range s {
				if c < '0' || c > '9' {
					return nil
				}
				n = n*10 + int(c-'0')
			}
			subs = append(subs, n)
		}
		out = append(out, subs)
	}
	return out
}

// csiSub returns param i's sub-param j, or -1 when either is absent.
func csiSub(params [][]int, i, j int) int {
	if i < 0 || i >= len(params) || j < 0 || j >= len(params[i]) {
		return -1
	}
	return params[i][j]
}

// matchesEncodedDetachKey reports whether seq — a complete escape sequence
// starting at ESC — is the detach key in either negotiated encoding.
func matchesEncodedDetachKey(seq []byte, enc detachKeyEncoding) bool {
	// Both encodings are CSI sequences: ESC [ <params> <final>.
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' {
		return false
	}
	final := seq[len(seq)-1]
	params := csiParams(seq[2 : len(seq)-1])

	switch final {
	case 'u':
		// kitty: CSI <key>[:<shifted>[:<base-layout>]] ; <mods>[:<event>] u
		if len(params) != 2 {
			return false
		}
		mods := csiSub(params, 1, 0)
		// Any slot naming the key counts. The base-layout slot is the point: it
		// reports the PC-101 key for the physical press, and it is the ONLY slot
		// carrying 'w' for a user typing ctrl+w on a Cyrillic layout, where the
		// primary slot holds that layout's own codepoint. The shifted slot is what
		// carries '^' when the user presses Ctrl+Shift+6.
		for _, code := range params[0] {
			if enc.matches(code, mods, true) {
				return true
			}
		}
		return false
	case '~':
		// xterm modifyOtherKeys=2: CSI 27 ; <mods> ; <codepoint> ~
		if len(params) != 3 {
			return false
		}
		return csiSub(params, 0, 0) == 27 &&
			enc.matches(csiSub(params, 2, 0), csiSub(params, 1, 0), false)
	default:
		return false
	}
}

// detachKeyEventType reports the kitty event type of an encoded key sequence. It
// defaults to press: the event-type sub-param is absent unless the pane program
// turned on kitty's event-reporting flag, and modifyOtherKeys has no such
// concept.
func detachKeyEventType(seq []byte) int {
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != 'u' {
		return kbEventPress
	}
	if ev := csiSub(csiParams(seq[2:len(seq)-1]), 1, 1); ev > 0 {
		return ev
	}
	return kbEventPress
}

// trailingDetachKeyLen reports how many bytes at the END of buf encode the
// detach key, or 0 if buf does not end with it. The legacy C0 byte is one byte;
// a negotiated encoding is a whole trailing escape sequence.
//
// Suffix-matching mirrors the long-standing #975 legacy rule and inherits its
// (accepted) trade-off: a paste whose final bytes happen to spell the detach
// key detaches. Terminals deliver a keypress as its own write, so this does not
// arise in practice.
//
// An encoded key is multi-byte, so unlike the legacy byte it could in principle
// straddle two reads (a paste that fills the caller's buffer to exactly
// mid-sequence) and be missed. This stays stateless rather than buffering a
// partial sequence: a keypress arrives as its own read, the window is a paste
// landing on an exact boundary, and the failure mode is one ignored detach —
// press again — not a wedged attach.
func trailingDetachKeyLen(buf []byte) int {
	if len(buf) == 0 {
		return 0
	}
	if buf[len(buf)-1] == tmux.DetachKeyByte {
		return 1
	}
	enc, ok := detachKeyEncodingFor(tmux.DetachKeyByte)
	if !ok {
		return 0
	}
	// A trailing escape sequence starts at the last ESC in the chunk.
	start := bytes.LastIndexByte(buf, 0x1b)
	if start < 0 || !matchesEncodedDetachKey(buf[start:], enc) {
		return 0
	}
	// With kitty's event-type flag on, ONE tap of the key reports twice — a press
	// (...;5:1u) then a release (...;5:3u) — and a single read can batch both. The
	// suffix test above sees only the release, so reporting just its length would
	// forward the press half of the very key being swallowed to the agent as
	// input, letting the detach key mutate the pane on its way out.
	//
	// Walk back over the earlier halves of THIS tap only, stopping at its press.
	// An earlier tap is a separate keypress, and #975's rule is that batched
	// leading input is forwarded rather than swallowed.
	if detachKeyEventType(buf[start:]) == kbEventRelease {
		for {
			prev := bytes.LastIndexByte(buf[:start], 0x1b)
			if prev < 0 || !matchesEncodedDetachKey(buf[prev:start], enc) {
				break
			}
			ev := detachKeyEventType(buf[prev:start])
			if ev != kbEventPress && ev != kbEventRepeat {
				break
			}
			start = prev
			if ev == kbEventPress {
				break // the press opens the tap; anything before it is earlier input
			}
			// A repeat is mid-tap (the key was held): the press is earlier still.
		}
	}
	return len(buf) - start
}
