package config

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// sanitizeDetachKeys puts detach_keys on the same warn-and-default footing as the
// other defaultable values in validateConfig (#2556): an unparseable hand-edited
// value warns and falls back to the default ctrl-w rather than crashing the TUI.
// Typed input stays strict — `af config set detach_keys` validates eagerly and
// rejects a bad value up front (the deliberate hand-edit-lenient / typed-strict
// asymmetry). An empty value is left as-is; the attach path treats "" as "use the
// built-in default".
func sanitizeDetachKeys(value, prettyConfigPath string) string {
	if strings.TrimSpace(value) == "" {
		return value
	}
	if _, err := ParseDetachKey(value); err != nil {
		log.WarningLog.Printf("Config issue in %s: detach_keys=%q is invalid (%v); using default %q",
			prettyConfigPath, value, err, defaultDetachKeys)
		return defaultDetachKeys
	}
	return value
}

// DefaultDetachKeys is the built-in default attach-detach key ("ctrl-w"), the
// value a bad configured key falls back to.
func DefaultDetachKeys() string { return defaultDetachKeys }

// ParseDetachKey parses a human-readable key name like "ctrl-w" into its ASCII byte value.
// Supported formats:
//   - "ctrl-a" through "ctrl-z" (case-insensitive)
//   - "ctrl-[", "ctrl-]", "ctrl-\", "ctrl-^", "ctrl-_"
//
// Returns the ASCII byte and an error if the key string is not recognized.
func ParseDetachKey(key string) (byte, error) {
	key = strings.TrimSpace(strings.ToLower(key))

	if !strings.HasPrefix(key, "ctrl-") {
		return 0, fmt.Errorf("unsupported key format %q: must start with \"ctrl-\"", key)
	}

	suffix := key[len("ctrl-"):]
	if len(suffix) != 1 {
		return 0, fmt.Errorf("unsupported key format %q: expected single character after \"ctrl-\"", key)
	}

	ch := suffix[0]
	switch {
	case ch >= 'a' && ch <= 'z':
		return ch - 'a' + 1, nil
	case ch == '[':
		return 27, nil // ESC
	case ch == '\\':
		return 28, nil
	case ch == ']':
		return 29, nil
	case ch == '^':
		return 30, nil
	case ch == '_':
		return 31, nil
	default:
		return 0, fmt.Errorf("unsupported key %q: character %q is not a valid ctrl- combination", key, ch)
	}
}
