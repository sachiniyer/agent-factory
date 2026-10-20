package daemon

import "testing"

func TestIsTypedInputIgnoresTerminalReplies(t *testing.T) {
	cases := map[string]bool{
		"a":          true,
		"\r":         true,
		"\x03":       true,
		"\x1b[I":     false,
		"\x1b[O":     false,
		"\x1b[12;1R": false,
		"":           false,
	}
	for in, want := range cases {
		if got := isTypedInput([]byte(in)); got != want {
			t.Errorf("isTypedInput(%q) = %v, want %v", in, got, want)
		}
	}
}
