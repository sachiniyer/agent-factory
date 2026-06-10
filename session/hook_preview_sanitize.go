package session

import (
	"strings"

	xansi "github.com/charmbracelet/x/ansi"
)

// sanitizeHookPreview strips every escape/control sequence from a raw remote
// preview stream except SGR color sequences and the \n/\t layout controls.
//
// HookBackend.Preview returns attach_cmd output that the preview pane embeds
// verbatim in its lipgloss frame. Any control sequence that survives — the
// documented preview pattern's own \x1b[2J\x1b[H, plus alt-screen
// (\x1b[?1049h), scroll-region (\x1b[r), and cursor-positioning sequences
// from the remote PTY — is executed by the real terminal when bubbletea
// flushes the frame, erasing the screen and repositioning the cursor
// mid-render (#810). SGR (\x1b[...m) is kept so remote output stays colored.
//
// Tokenize with xansi.DecodeSequence rather than a bespoke regex: the ErrBox
// sanitizer went through three rounds of regex escape-variant whack-a-mole
// (#525 → #552 → #565) before settling on the vetted xansi parser, and this
// follows the same rule. xansi.Strip alone is not enough here because it
// also removes SGR.
//
// Raw \r is dropped too — it would pull the cursor to column 0 and overwrite
// pane padding (#668) — as are all other C0 controls except \n and \t.
func sanitizeHookPreview(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	var state byte
	for len(s) > 0 {
		seq, _, n, newState := xansi.DecodeSequence(s, state, nil)
		if n == 0 {
			// Defensive: DecodeSequence always consumes at least one byte;
			// bail rather than spin if that invariant ever breaks.
			break
		}
		switch {
		case isSGRSequence(seq):
			out.WriteString(seq)
		case seq[0] == '\x1b' || (seq[0] >= 0x80 && seq[0] <= 0x9f):
			// Non-SGR escape sequence (CSI command, OSC/DCS string,
			// single-char escape) or a C1-introduced sequence: drop. An
			// incomplete sequence split at the end of the buffer lands here
			// too — DecodeSequence returns the unterminated tail as one
			// token, and it cannot match the complete-SGR check above.
		case len(seq) == 1 && (seq[0] < 0x20 || seq[0] == 0x7f):
			// C0 control / DEL: keep only the layout controls the pane
			// renderer understands.
			if seq[0] == '\n' || seq[0] == '\t' {
				out.WriteByte(seq[0])
			}
		default:
			out.WriteString(seq)
		}
		state = newState
		s = s[n:]
	}
	return out.String()
}

// isSGRSequence reports whether seq is exactly one 7-bit SGR sequence:
// ESC [ <numeric params> m. Private-prefix variants (\x1b[?4m) and sequences
// with intermediate bytes are not SGR and return false, as are C1 (8-bit
// CSI) forms — a UTF-8 terminal would not execute those bytes anyway, and
// passing them through would emit invalid UTF-8 into the pane.
func isSGRSequence(seq string) bool {
	if len(seq) < 3 || !strings.HasPrefix(seq, "\x1b[") || seq[len(seq)-1] != 'm' {
		return false
	}
	body := seq[2 : len(seq)-1]
	for i := 0; i < len(body); i++ {
		c := body[i]
		if (c < '0' || c > '9') && c != ';' && c != ':' {
			return false
		}
	}
	return true
}
