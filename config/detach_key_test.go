package config

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
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

// withDefaultDetachKey snapshots the process-global detach key for one test and
// restores it on cleanup, so ApplyConfiguredDetachKey's mutation of the global
// cannot leak into a sibling test (apiclient's matcher reads it live).
func withDefaultDetachKey(t *testing.T) {
	t.Helper()
	prevByte, prevDisplay := tmux.DetachKeyByte, tmux.DetachKeyDisplay
	t.Cleanup(func() { tmux.SetDetachKey(prevByte, prevDisplay) })
	// Start every case from the built-in default so a prior test's mutation is
	// not the baseline.
	tmux.SetDetachKey(23, "ctrl+w")
}

// TestApplyConfiguredDetachKey pins the shared wiring site both attach paths
// (the TUI's newHome and `af sessions attach`) call: it parses the configured
// value and arms the global, warn-and-defaults an unparseable value instead of
// crashing, and leaves the default in place when the value is empty (the path a
// caller takes when config is unset or failed to load).
func TestApplyConfiguredDetachKey(t *testing.T) {
	t.Run("valid key arms the global with its byte and display form", func(t *testing.T) {
		withDefaultDetachKey(t)
		ApplyConfiguredDetachKey("ctrl-a")
		assert.Equal(t, byte(1), tmux.DetachKeyByte, "ctrl-a parses to byte 1")
		assert.Equal(t, "ctrl+a", tmux.DetachKeyDisplay, "SetDetachKey normalizes ctrl-a to ctrl+a for the help overlay (#4176)")
	})

	t.Run("a punctuation binding arms its byte and preserves the config spelling", func(t *testing.T) {
		withDefaultDetachKey(t)
		ApplyConfiguredDetachKey("ctrl-]")
		assert.Equal(t, byte(29), tmux.DetachKeyByte)
		assert.Equal(t, "ctrl+]", tmux.DetachKeyDisplay)
	})

	t.Run("invalid value warns and falls back to the default ctrl+w", func(t *testing.T) {
		withDefaultDetachKey(t)
		// First arm a non-default key, so a fallback that left the global
		// untouched would be caught: the contract is that it RESETS to the
		// default, not that it ignores the input.
		ApplyConfiguredDetachKey("ctrl-a")
		require.Equal(t, byte(1), tmux.DetachKeyByte, "fixture: ctrl-a armed first")

		ApplyConfiguredDetachKey("not-a-key")
		assert.Equal(t, byte(23), tmux.DetachKeyByte, "an unparseable value must reset to the default byte")
		assert.Equal(t, "ctrl+w", tmux.DetachKeyDisplay, "an unparseable value must reset to the default display")
	})

	t.Run("empty value is a no-op and keeps whatever is armed", func(t *testing.T) {
		withDefaultDetachKey(t)
		// The default itself stays put — the documented behavior for an unset
		// detach_keys (the CLI path with no config, or a load failure).
		ApplyConfiguredDetachKey("")
		assert.Equal(t, byte(23), tmux.DetachKeyByte, "empty must leave the built-in default armed")
		assert.Equal(t, "ctrl+w", tmux.DetachKeyDisplay)
	})

	t.Run("empty value does not reset a previously-armed non-default key", func(t *testing.T) {
		withDefaultDetachKey(t)
		ApplyConfiguredDetachKey("ctrl-q")
		require.Equal(t, byte(17), tmux.DetachKeyByte, "fixture: ctrl-q armed first")
		// A caller that loads config but finds detach_keys empty (e.g. a load
		// failure) passes "" and must not clobber a key armed earlier in the
		// same process. The global is the caller's to manage; "" is "do
		// nothing", not "reset to default".
		ApplyConfiguredDetachKey("")
		assert.Equal(t, byte(17), tmux.DetachKeyByte, "empty must not undo a prior arm")
		assert.Equal(t, "ctrl+q", tmux.DetachKeyDisplay)
	})
}
