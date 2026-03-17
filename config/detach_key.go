package config

import (
	"fmt"
	"strings"
)

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
