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

		// kitty appends sub-params to the modifier param (event type) and the key
		// param (shifted / base-layout alternates) when those
		// progressive-enhancement flags are on. Every event type of the key means
		// the user pressed it.
		{"kitty with event-type sub-param", "\x1b[119;5:1u", len("\x1b[119;5:1u")},
		{"kitty repeat event", "\x1b[119;5:2u", len("\x1b[119;5:2u")},
		{"kitty release event", "\x1b[119;5:3u", len("\x1b[119;5:3u")},

		// The report-alternate-keys flag reports the current layout's codepoint
		// first and the PC-101 key in the base-layout slot. On a Cyrillic layout
		// the physical ctrl+w key is 1094 (ц), and 119 appears ONLY as the
		// base-layout alternate — matching just the primary slot forwarded the
		// user's detach key to the agent.
		{"kitty base-layout alternate key", "\x1b[1094::119;5u", len("\x1b[1094::119;5u")},
		{"kitty alternates naming a different key", "\x1b[1094::120;5u", 0},

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

	// ctrl-[ and Esc are the same BYTE, which is why the legacy match cannot tell
	// them apart. The encoded forms can: kitty reports the physical Ctrl+[ as
	// CSI 91;5u and bare Esc as CSI 27u. Matching '[' detaches on the real binding
	// without hijacking the Esc the agent needs — treating byte 27 as legacy-only
	// left ctrl-[ users unable to detach at all once a pane upgraded the terminal.
	t.Run("ctrl-[ matches its encoded form, not Esc", func(t *testing.T) {
		withDetachKey(t, 27, "ctrl-[")
		if got := trailingDetachKeyLen([]byte("\x1b[91;5u")); got != len("\x1b[91;5u") {
			t.Fatalf("ctrl-[ kitty encoding not matched: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte("\x1b[27;5;91~")); got != len("\x1b[27;5;91~") {
			t.Fatalf("ctrl-[ modifyOtherKeys encoding not matched: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte("\x1b[27u")); got != 0 {
			t.Fatalf("bare Esc (CSI 27u) must never be hijacked as detach: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte("\x1b[27;5u")); got != 0 {
			t.Fatalf("ctrl+Esc is not ctrl+[: got %d", got)
		}
		if got := trailingDetachKeyLen([]byte{27}); got != 1 {
			t.Fatalf("ctrl-[ legacy byte must still detach: got %d", got)
		}
	})

	// ctrl-^ and ctrl-_ are the two bindings whose character needs SHIFT to type
	// on a US layout (Ctrl+Shift+6, Ctrl+Shift+-). kitty reports the unshifted key
	// code with the shift bit rather than codepoint 94/95 with ctrl alone, so a
	// ctrl-only match never fired and these supported bindings could not detach.
	t.Run("ctrl-^ matches its shifted physical encoding", func(t *testing.T) {
		withDetachKey(t, 30, "ctrl-^") // '^' = 94, typed as Ctrl+Shift+6 ('6' = 54)
		for _, in := range []string{
			"\x1b[54;6u",    // kitty: unshifted '6' + ctrl|shift
			"\x1b[54:94;6u", // kitty with report-alternate-keys: shifted slot is '^'
			"\x1b[94;5u",    // a layout where '^' needs no shift
			"\x1b[27;6;94~", // modifyOtherKeys: the '^' character + ctrl|shift
			"\x1b[54;134u",  // ctrl|shift with num-lock OR-ed in
		} {
			if got := trailingDetachKeyLen([]byte(in)); got != len(in) {
				t.Fatalf("ctrl-^ encoding %q not matched: got %d, want %d", in, got, len(in))
			}
		}
		// Shift is admitted because it is how the character is typed — not as a
		// licence for any other modifier, and not for a different key.
		for _, in := range []string{
			"\x1b[54;7u",    // ctrl+alt+6 is a different key
			"\x1b[54;2u",    // shift+6 alone is just '^' text
			"\x1b[55;6u",    // ctrl+shift+7 is a different key
			"\x1b[27;6;95~", // that is ctrl-_, not ctrl-^
		} {
			if got := trailingDetachKeyLen([]byte(in)); got != 0 {
				t.Fatalf("%q must not detach when ctrl-^ is bound: got %d", in, got)
			}
		}
		// The shift rule belongs to the CODE, not the binding. '6' is the detach
		// key only WITH shift: unshifted Ctrl+6 is a different key, and swallowing
		// it would steal a keypress the pane should receive.
		for _, in := range []string{
			"\x1b[54;5u",    // kitty: Ctrl+6, no shift
			"\x1b[27;5;54~", // modifyOtherKeys: Ctrl+6, no shift
		} {
			if got := trailingDetachKeyLen([]byte(in)); got != 0 {
				t.Fatalf("unshifted Ctrl+6 %q must reach the pane, not detach, "+
					"when ctrl-^ is bound: got %d", in, got)
			}
		}
	})

	t.Run("ctrl-_ matches its shifted physical encoding", func(t *testing.T) {
		withDetachKey(t, 31, "ctrl-_") // '_' = 95, typed as Ctrl+Shift+- ('-' = 45)
		for _, in := range []string{
			"\x1b[45;6u",    // kitty: unshifted '-' + ctrl|shift
			"\x1b[45:95;6u", // kitty with report-alternate-keys: shifted slot is '_'
			"\x1b[95;5u",    // a layout where '_' needs no shift
			"\x1b[95;6u",    // ...and the same layout with shift held
			"\x1b[27;6;95~", // modifyOtherKeys: the '_' character + ctrl|shift
		} {
			if got := trailingDetachKeyLen([]byte(in)); got != len(in) {
				t.Fatalf("ctrl-_ encoding %q not matched: got %d, want %d", in, got, len(in))
			}
		}
		// Ctrl+- with no shift is NOT ctrl-_ — it is a distinct keypress (font-size
		// shortcuts live there) and must reach the pane. Making shift optional for
		// every code of the binding swallowed it.
		for _, in := range []string{
			"\x1b[45;5u",    // kitty: Ctrl+-, no shift
			"\x1b[27;5;45~", // modifyOtherKeys: Ctrl+-, no shift
			"\x1b[45;133u",  // Ctrl+- with num-lock on: still no shift
		} {
			if got := trailingDetachKeyLen([]byte(in)); got != 0 {
				t.Fatalf("unshifted Ctrl+- %q must reach the pane, not detach, "+
					"when ctrl-_ is bound: got %d", in, got)
			}
		}
		if got := trailingDetachKeyLen([]byte("\x1b[54;6u")); got != 0 {
			t.Fatalf("ctrl+shift+6 must not detach when ctrl-_ is bound: got %d", got)
		}
	})

	// Shift stays a genuine modifier difference for every OTHER binding: the
	// character needs no shift to type, so a shift bit means the user pressed
	// something else.
	t.Run("shift is not admitted for keys that do not need it", func(t *testing.T) {
		withDetachKey(t, 29, "ctrl-]")
		if got := trailingDetachKeyLen([]byte("\x1b[93;6u")); got != 0 {
			t.Fatalf("ctrl+shift+] must not detach when ctrl-] is bound: got %d", got)
		}
	})
}

// TestTrailingDetachKeyLen_BatchedEventTypes pins the swallow contract when a
// pane program turns on kitty's report-event-types flag: ONE tap of the detach
// key is reported as a press and a release (and, held down, repeats between
// them), which a single stdin read can batch.
//
// The suffix test sees only the trailing release, so reporting just its length
// would forward the press half of the very key being swallowed to the agent as
// input — the detach key mutating the pane on its way out. All halves of the
// tap must be consumed together.
//
// The boundary is the tap, not the chunk: an EARLIER tap batched into the same
// read is a separate keypress and stays subject to #975's rule that batched
// leading input is forwarded rather than swallowed.
func TestTrailingDetachKeyLen_BatchedEventTypes(t *testing.T) {
	withDetachKey(t, 23, "ctrl-w")

	const (
		press   = "\x1b[119;5:1u"
		repeat  = "\x1b[119;5:2u"
		release = "\x1b[119;5:3u"
	)

	for _, tc := range []struct {
		name string
		in   string
		want int
	}{
		{"one tap: press and release are swallowed together", press + release, len(press + release)},
		{"held key: press, repeats and release are one tap", press + repeat + repeat + release, len(press + repeat + repeat + release)},
		{"typed input before a tap is still forwarded", "abc" + press + release, len(press + release)},

		// Two distinct taps in one read: only the last is swallowed, so the first
		// reaches the agent as input exactly as the legacy double-tap does (#975).
		{"earlier tap is forwarded, not swallowed", press + release + press + release, len(press + release)},

		// A press arriving alone detaches on its own; the release lands after the
		// stdin pump is gone. Nothing to walk back over.
		{"lone press", press, len(press)},
		{"lone release", release, len(release)},

		// The walk-back must not reach past a sequence that is not this key.
		{"different key before the tap is forwarded", "\x1b[120;5:1u" + press + release, len(press + release)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := trailingDetachKeyLen([]byte(tc.in)); got != tc.want {
				t.Fatalf("trailingDetachKeyLen(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
