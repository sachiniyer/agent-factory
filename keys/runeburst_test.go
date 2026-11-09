package keys

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
)

func runeKey(s string) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)} }

// TestSplitRuneBurstSplitsAPlainBurst: a coalesced burst splits into the
// single-rune keys that were pressed, each the exact message that key produces
// on its own, so each one matches the key map again (#7015).
func TestSplitRuneBurstSplitsAPlainBurst(t *testing.T) {
	got := SplitRuneBurst(runeKey("kkj"))

	assert.Equal(t, []tea.KeyMsg{runeKey("k"), runeKey("k"), runeKey("j")}, got)
	for _, k := range got {
		_, bound := GlobalKeyStringsMap[k.String()]
		assert.True(t, bound, "%q must resolve through the key map once split", k.String())
	}
}

// TestSplitRuneBurstLeavesOneKeyAlone: anything that is already one keypress,
// or one gesture, comes back nil so the caller handles it unchanged.
func TestSplitRuneBurstLeavesOneKeyAlone(t *testing.T) {
	cases := map[string]tea.KeyMsg{
		"single rune":            runeKey("k"),
		"empty runes":            {Type: tea.KeyRunes},
		"non-rune key":           {Type: tea.KeyEnter},
		"space key":              {Type: tea.KeySpace, Runes: []rune(" ")},
		"bracketed paste":        {Type: tea.KeyRunes, Runes: []rune("kk"), Paste: true},
		"alt single rune":        {Type: tea.KeyRunes, Runes: []rune("k"), Alt: true},
		"alt multi-rune (synth)": {Type: tea.KeyRunes, Runes: []rune("kk"), Alt: true},
	}
	for name, msg := range cases {
		assert.Nil(t, SplitRuneBurst(msg), name)
	}
}

// TestSplitRuneBurstNeverMovesAlt: on what the real parser delivers for
// ESC+"kkk", the Alt key stays exactly as it is and the plain burst splits into
// plain keys. No rune gains or loses Alt.
func TestSplitRuneBurstNeverMovesAlt(t *testing.T) {
	parsed := readKeys(t, "\x1bkkk")
	var keys []tea.KeyMsg
	for _, msg := range parsed {
		if burst := SplitRuneBurst(msg); burst != nil {
			keys = append(keys, burst...)
			continue
		}
		keys = append(keys, msg)
	}

	assert.Equal(t, []tea.KeyMsg{
		{Type: tea.KeyRunes, Runes: []rune("k"), Alt: true},
		runeKey("k"),
		runeKey("k"),
	}, keys)
}

// TestSplitRuneBurstMakesASpaceAKeySpace: a space in a synthesized burst becomes
// the KeySpace message Bubble Tea gives a lone space.
func TestSplitRuneBurstMakesASpaceAKeySpace(t *testing.T) {
	assert.Equal(t, []tea.KeyMsg{
		runeKey("a"),
		{Type: tea.KeySpace, Runes: []rune(" ")},
		runeKey("b"),
	}, SplitRuneBurst(runeKey("a b")))
}
