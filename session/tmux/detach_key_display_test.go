package tmux

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestDetachKeyDisplayUsesOverlaySpelling pins #4176: the config file's
// "ctrl-w" spelling must arrive at the help surfaces as "ctrl+w", matching
// every other ctrl binding the overlay lists. The normaliser is where the two
// spellings meet, so a configured value and the built-in default render
// identically.
func TestDetachKeyDisplayUsesOverlaySpelling(t *testing.T) {
	prevByte, prevDisplay := DetachKeyByte, DetachKeyDisplay
	t.Cleanup(func() { SetDetachKey(prevByte, prevDisplay) })

	for _, tc := range []struct {
		in   string
		want string
	}{
		{"ctrl-w", "ctrl+w"},
		{"ctrl-]", "ctrl+]"},
		{"Ctrl-Q", "ctrl+q"},   // case/spacing the config parser accepts
		{" ctrl-a ", "ctrl+a"}, // hand-edited whitespace
		{"ctrl+w", "ctrl+w"},   // already-normalised input is idempotent
	} {
		SetDetachKey(23, tc.in)
		assert.Equal(t, tc.want, DetachKeyDisplay, "display form for %q", tc.in)
	}
}
