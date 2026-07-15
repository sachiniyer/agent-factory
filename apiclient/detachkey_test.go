package apiclient

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// withDetachKey pins the process-global detach key for one test and restores it.
func withDetachKey(t *testing.T, b byte, display string) {
	t.Helper()
	prevByte, prevDisplay := tmux.DetachKeyByte, tmux.DetachKeyDisplay
	tmux.SetDetachKey(b, display)
	t.Cleanup(func() { tmux.SetDetachKey(prevByte, prevDisplay) })
}

// TestTrailingDetachKeyLen pins the #1832 contract: the detach key must be
// recognized in EVERY encoding a pane program can negotiate onto the real
// terminal, and must NOT be confused with a different key or a different
// modifier combination. Lengths are computed with len() rather than hand-counted
// so a test never encodes an off-by-one of its own.
func TestTrailingDetachKeyLen(t *testing.T) {
	withDetachKey(t, 23, "ctrl-w") // the default: ctrl-w, codepoint 'w' = 119

	const (
		legacy  = "\x17"
		kitty   = "\x1b[119;5u"    // CSI 119 ; 5 u  — kitty disambiguate
		modOthr = "\x1b[27;5;119~" // CSI 27 ; 5 ; 119 ~ — xterm modifyOtherKeys=2
	)

	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		// The three encodings that mean "the user pressed the detach key".
		{"legacy C0 byte", legacy, len(legacy)},
		{"kitty CSI u", kitty, len(kitty)},
		{"xterm modifyOtherKeys", modOthr, len(modOthr)},

		// Lock state is OR-ed in by the terminal at will and must not defeat
		// detach: caps_lock=64, num_lock=128 (encoded as bits+1).
		{"kitty with num-lock on", "\x1b[119;133u", len("\x1b[119;133u")},
		{"kitty with caps-lock on", "\x1b[119;69u", len("\x1b[119;69u")},
		{"kitty with both locks on", "\x1b[119;197u", len("\x1b[119;197u")},

		// kitty appends sub-params (event type / alternate key) when those
		// progressive-enhancement flags are on; only the first sub-param counts.
		{"kitty with event-type sub-param", "\x1b[119;5:1u", len("\x1b[119;5:1u")},

		// #975: a read that batches typed bytes with the detach key detaches,
		// and reports only the key's own length so the rest is forwarded.
		{"batched legacy", "abc" + legacy, len(legacy)},
		{"batched kitty", "abc" + kitty, len(kitty)},
		{"batched modifyOtherKeys", "abc" + modOthr, len(modOthr)},

		// Not the detach key: a real modifier difference must forward, never detach.
		{"ctrl+shift+w is not the detach key", "\x1b[119;6u", 0},
		{"alt+w is not the detach key", "\x1b[119;3u", 0},
		{"unmodified w is not the detach key", "\x1b[119u", 0},
		{"ctrl+x is a different key", "\x1b[120;5u", 0},
		{"modifyOtherKeys ctrl+x is a different key", "\x1b[27;5;120~", 0},
		{"modifyOtherKeys with no ctrl", "\x1b[27;1;119~", 0},

		// Plain text and partial input must never be mistaken for the key.
		{"plain w", "w", 0},
		{"empty", "", 0},
		{"bare escape", "\x1b", 0},
		{"detach key not at the end", legacy + "abc", 0},
		{"kitty sequence not at the end", kitty + "abc", 0},
		{"unrelated CSI (cursor up)", "\x1b[A", 0},
		{"malformed params", "\x1b[1x9;5u", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trailingDetachKeyLen([]byte(tc.in)); got != tc.want {
				t.Fatalf("trailingDetachKeyLen(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestTrailingDetachKeyLen_RebindableKey proves the encoded forms track the
// CONFIGURED key (config.ParseDetachKey only emits ctrl-<letter> and ctrl-\]^_),
// rather than hard-coding ctrl-w's codepoint.
func TestTrailingDetachKeyLen_RebindableKey(t *testing.T) {
	t.Run("ctrl-a", func(t *testing.T) {
		withDetachKey(t, 1, "ctrl-a") // codepoint 'a' = 97
		if got := trailingDetachKeyLen([]byte("\x1b[97;5u")); got != len("\x1b[97;5u") {
			t.Fatalf("rebound ctrl-a kitty encoding not matched: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte("\x1b[119;5u")); got != 0 {
			t.Fatalf("ctrl-w must not detach when ctrl-a is bound: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte{1}); got != 1 {
			t.Fatalf("rebound legacy byte not matched: got %d", got)
		}
	})

	t.Run("ctrl-] maps to its punctuation codepoint", func(t *testing.T) {
		withDetachKey(t, 29, "ctrl-]") // codepoint ']' = 93
		if got := trailingDetachKeyLen([]byte("\x1b[93;5u")); got != len("\x1b[93;5u") {
			t.Fatalf("ctrl-] kitty encoding not matched: got %d", got)
		}
	})

	// ctrl-[ IS Esc, which the kitty protocol reports specially; hijacking a bare
	// Esc would steal a key the agent needs, so it stays legacy-only.
	t.Run("ctrl-[ has no encoded form", func(t *testing.T) {
		withDetachKey(t, 27, "ctrl-[")
		if got := trailingDetachKeyLen([]byte("\x1b[27;5u")); got != 0 {
			t.Fatalf("ctrl-[ must not match an encoded form: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte{27}); got != 1 {
			t.Fatalf("ctrl-[ legacy byte must still detach: got %d", got)
		}
	})
}
