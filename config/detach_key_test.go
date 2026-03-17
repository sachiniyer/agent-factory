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
		{"w", 0, true},           // missing ctrl- prefix
		{"ctrl-", 0, true},       // missing character
		{"ctrl-ab", 0, true},     // too many characters
		{"ctrl-1", 0, true},      // digit not supported
		{"alt-w", 0, true},       // wrong prefix
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
