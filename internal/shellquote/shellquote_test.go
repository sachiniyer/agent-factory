package shellquote

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestQuoteRendering pins the exact strings both former copies produced, so the
// consolidation is provably a move rather than a rewrite. The first three cases
// are session/tmux's TestShellQuoteArg verbatim.
func TestQuoteRendering(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"safe value passes through bare", "abc-123_./:@%+=", "abc-123_./:@%+="},
		{"metacharacters are quoted", "thread id?x=1&y=2", "'thread id?x=1&y=2'"},
		{"embedded quote is escaped", "it's", `'it'"'"'s'`},
		{"empty becomes an explicit empty word", "", "''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Quote(tc.in))
		})
	}
}

// TestQuoteAllRendering pins the unconditional sibling's output — every input
// quoted, the empty string an explicit empty word, and the escape idiom the
// folded copies rendered — so the consolidation is provably a move rather than
// a rewrite. The cases are the ones the three former bodies agreed on.
func TestQuoteAllRendering(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"safe value is still quoted", "x", "'x'"},
		{"metacharacters are quoted", "thread id?x=1&y=2", "'thread id?x=1&y=2'"},
		{"embedded quote uses the folded idiom", "it's", `'it'\''s'`},
		{"empty becomes an explicit empty word", "", "''"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, QuoteAll(tc.in))
		})
	}
}

// TestQuoteSurvivesTheShell is the guard that matters: the quoted word must
// reach /bin/sh as one argument, byte-for-byte, with nothing executed on the
// way. The payloads below run destructive or observable commands if the quoting
// is wrong, so a regression fails loudly instead of quietly mangling a value.
func TestQuoteSurvivesTheShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skipf("no sh on PATH: %v", err)
	}
	payloads := []string{
		"plain",
		"has space",
		"it's",
		"$(touch /tmp/af-shellquote-should-not-exist)",
		"`id`",
		"a;b",
		"a|b",
		"a\nb",
		`double"quote`,
		"back\\slash",
		"*",
		"~/tilde",
		"$HOME",
	}
	for _, raw := range payloads {
		t.Run(raw, func(t *testing.T) {
			for name, quote := range map[string]func(string) string{"Quote": Quote, "QuoteAll": QuoteAll} {
				out, err := exec.Command("sh", "-c", "printf %s "+quote(raw)).CombinedOutput()
				require.NoError(t, err, "%s(%q) produced a command the shell rejects: %s", name, raw, out)
				require.Equal(t, raw, string(out), "%s(%q) did not survive the shell verbatim", name, raw)
			}
		})
	}
}
