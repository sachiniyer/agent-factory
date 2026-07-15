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

// detachKeyCodepoint maps the detach key's C0 control byte to the Unicode
// codepoint of the key the user physically presses — what the CSI u and
// modifyOtherKeys encodings report. config.ParseDetachKey only ever produces
// ctrl-<letter> (1..26) or ctrl-\ ] ^ _ (28..31).
//
// 27 (ctrl-[) is deliberately unmapped: it IS Esc, which the kitty protocol
// reports specially, and treating a bare Esc as "detach" would hijack a key the
// agent needs. A ctrl-[ detach key keeps legacy-only matching.
func detachKeyCodepoint(b byte) (int, bool) {
	switch {
	case b >= 1 && b <= 26:
		return int(b) + 'a' - 1, true // ctrl-a..ctrl-z -> 'a'..'z'
	case b >= 28 && b <= 31:
		return int(b) + 64, true // ctrl-\ ] ^ _ -> '\' ']' '^' '_'
	default:
		return 0, false
	}
}

// ctrlOnly reports whether an encoded modifier param means "ctrl, and nothing
// else" once lock state is masked away.
func ctrlOnly(param int, maskLocks bool) bool {
	if param < 1 {
		return false
	}
	bits := param - 1
	if maskLocks {
		bits &^= kbModLockMsk
	}
	return bits == kbModCtrl
}

// csiParams splits the parameter bytes of a CSI sequence into top-level params,
// keeping only each param's FIRST sub-parameter (kitty appends ":<event-type>"
// / ":<alternate-key>" sub-params when those progressive-enhancement flags are
// on). An empty param yields -1 ("default"), matching terminal convention.
func csiParams(b []byte) []int {
	if len(b) == 0 {
		return nil
	}
	var out []int
	for _, field := range bytes.Split(b, []byte{';'}) {
		if i := bytes.IndexByte(field, ':'); i >= 0 {
			field = field[:i]
		}
		if len(field) == 0 {
			out = append(out, -1)
			continue
		}
		n := 0
		ok := true
		for _, c := range field {
			if c < '0' || c > '9' {
				ok = false
				break
			}
			n = n*10 + int(c-'0')
		}
		if !ok {
			return nil
		}
		out = append(out, n)
	}
	return out
}

// matchesEncodedDetachKey reports whether seq — a complete escape sequence
// starting at ESC — is the detach key in either negotiated encoding.
func matchesEncodedDetachKey(seq []byte, cp int) bool {
	// Both encodings are CSI sequences: ESC [ <params> <final>.
	if len(seq) < 4 || seq[0] != 0x1b || seq[1] != '[' {
		return false
	}
	final := seq[len(seq)-1]
	params := csiParams(seq[2 : len(seq)-1])

	switch final {
	case 'u':
		// kitty: CSI <codepoint> ; <modifiers> u
		if len(params) != 2 {
			return false
		}
		return params[0] == cp && ctrlOnly(params[1], true)
	case '~':
		// xterm modifyOtherKeys=2: CSI 27 ; <modifiers> ; <codepoint> ~
		if len(params) != 3 {
			return false
		}
		return params[0] == 27 && params[2] == cp && ctrlOnly(params[1], false)
	default:
		return false
	}
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
	cp, ok := detachKeyCodepoint(tmux.DetachKeyByte)
	if !ok {
		return 0
	}
	// A trailing escape sequence starts at the last ESC in the chunk.
	i := bytes.LastIndexByte(buf, 0x1b)
	if i < 0 {
		return 0
	}
	if matchesEncodedDetachKey(buf[i:], cp) {
		return len(buf) - i
	}
	return 0
}
