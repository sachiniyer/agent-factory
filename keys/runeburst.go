package keys

import tea "github.com/charmbracelet/bubbletea"

// SplitRuneBurst returns the separate keypresses inside a burst of printable
// runes that Bubble Tea delivered as one message. It returns nil when msg is not
// such a burst.
//
// Bubble Tea reports "the longest sequence of runes that are not control
// characters" found in one terminal read as a SINGLE KeyRunes message (v1
// key.go, detectOneMsg). So two `k` presses that land in the same read arrive as
// one message whose String() is "kk". That happens with key repeat, fast
// typing, or an event loop that fell behind a slow frame. Every binding lookup
// in this repo matches String() exactly (GlobalKeyStringsMap, key.Matches), so
// the burst matches no binding. On the tree it was silently dropped. On a
// focused pane it read as an unbound printable key, which switches into
// keyboard mode and types (#4722 Option 3), so the `k` a user pressed to move
// the cursor was typed into the agent (#7015). Splitting restores the property
// a user expects: a key sequence behaves the same however the terminal chunked
// it into reads.
//
// These are deliberately NOT split:
//   - A single rune, or any non-rune key. Each is already one keypress.
//   - A bracketed paste (Paste). It is one gesture, and its runes must never
//     fire shortcuts. That is why Bubble Tea brackets its String() as "[…]".
//   - An Alt-modified key. Bubble Tea attaches Alt to exactly ONE rune, the one
//     right after the ESC (detectOneMsg stops collecting there), and any runes
//     after it arrive as their own plain message. So the parser never produces
//     a multi-rune Alt message, and splitting one would spread Alt to runes
//     that were never modified. keys/bubbletea_input_test.go pins the parser
//     side of this.
//
// A space inside a burst becomes KeySpace, the type Bubble Tea gives a lone
// space, so each piece is exactly the message that key produces when it
// arrives alone. (The parser itself ends a burst at a space. Only a
// synthesized message can contain one.)
func SplitRuneBurst(msg tea.KeyMsg) []tea.KeyMsg {
	if msg.Type != tea.KeyRunes || msg.Paste || msg.Alt || len(msg.Runes) < 2 {
		return nil
	}
	out := make([]tea.KeyMsg, 0, len(msg.Runes))
	for _, r := range msg.Runes {
		k := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}
		if r == ' ' {
			k.Type = tea.KeySpace
		}
		out = append(out, k)
	}
	return out
}
