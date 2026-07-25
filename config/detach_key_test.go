package config

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseDetachKey(t *testing.T) {
	tests := []struct {
		input    string
		expected byte
		wantErr  bool
	}{
		{"ctrl-a", 1, false},
		{"ctrl-b", 2, false},
		{"ctrl-c", 3, false},
		{"ctrl-w", 23, false},
		{"ctrl-q", 17, false},
		{"ctrl-z", 26, false},
		{"Ctrl-W", 23, false},   // case insensitive
		{"CTRL-Q", 17, false},   // case insensitive
		{" ctrl-w ", 23, false}, // trimmed
		{"ctrl-[", 27, false},   // ESC
		{"ctrl-]", 29, false},
		{"ctrl-\\", 28, false},
		{"ctrl-^", 30, false},
		{"ctrl-_", 31, false},
		{"w", 0, true},       // missing ctrl- prefix
		{"ctrl-", 0, true},   // missing character
		{"ctrl-ab", 0, true}, // too many characters
		{"ctrl-1", 0, true},  // digit not supported
		{"alt-w", 0, true},   // wrong prefix
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			b, err := ParseDetachKey(tt.input)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.expected, b)
			}
		})
	}
}

// TestSanitizeDetachKeys pins the #2556 warn-and-default behavior: a valid value
// passes through, an invalid hand-edited value falls back to the default (never a
// crash — the old os.Exit(1)), and empty is left as-is ("use the default" at the
// attach site). This is the loader-side guarantee that lets the TUI trust the
// value instead of aborting on a config typo.
func TestSanitizeDetachKeys(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"valid passes through", "ctrl-q", "ctrl-q"},
		{"valid non-alpha passes through", "ctrl-]", "ctrl-]"},
		{"empty stays empty", "", ""},
		{"whitespace-only stays as-is", "   ", "   "},
		{"invalid falls back to default", "ctrl-1", defaultDetachKeys},
		{"wrong prefix falls back to default", "alt-w", defaultDetachKeys},
		{"garbage falls back to default", "not-a-key", defaultDetachKeys},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, sanitizeDetachKeys(tt.input, "test-config"))
		})
	}
	// The default we fall back to must itself parse, or the fallback is a lie.
	_, err := ParseDetachKey(DefaultDetachKeys())
	require.NoError(t, err, "DefaultDetachKeys() must parse")
}
