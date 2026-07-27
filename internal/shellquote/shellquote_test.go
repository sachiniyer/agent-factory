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
			out, err := exec.Command("sh", "-c", "printf %s "+Quote(raw)).CombinedOutput()
			require.NoError(t, err, "Quote(%q) produced a command the shell rejects: %s", raw, out)
			require.Equal(t, raw, string(out), "Quote(%q) did not survive the shell verbatim", raw)
		})
	}
}
