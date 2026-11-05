// Package printsafety sanitizes durable-record field values for display.
// The strings the handoff surfaces print — a halt reason, an account name,
// a stable session key, a session title — are stored, and a hand-edited
// file (or a buggy writer) can put runes into them a terminal must never
// see: control codes, bidi overrides, or arbitrary length (#6776 review).
// CLI, TUI, and web all render the same vocabulary, so the strip happens
// once here rather than trusting every print site.
package printsafety

import "unicode"

// FieldCap bounds one printed record field in runes — long enough for any
// legitimate identity or reason, short enough that a hostile value cannot
// blow a status line out of the pane.
const FieldCap = 160

// Field strips the runes a stored string must never carry onto a terminal
// and caps the result at FieldCap runes, marked with a trailing ellipsis
// when it had to cut. unicode.IsPrint rejects control characters outright;
// the explicit Cf check names the class that matters most here: Unicode
// format characters — bidi embedding and override marks, isolates,
// joiners, byte-order marks — that reorder what a reader sees without
// printing anything of their own.
func Field(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		out = append(out, r)
	}
	if len(out) > FieldCap {
		return string(out[:FieldCap]) + "…"
	}
	return string(out)
}
