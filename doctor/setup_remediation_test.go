package doctor

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// Every check `af doctor --setup` can raise already puts its cause in the
// detail line, so its fix line must say what to do next. The old fallback —
// "inspect the message above and rerun `af doctor`" — pointed the reader back
// at the line it sat under (pt-first-run friction log F1/F21).
func TestSetupChecksHaveSpecificRemediation(t *testing.T) {
	generic := findingRemediation(Finding{Check: "no-such-check"}, false)
	require.Contains(t, generic, "inspect the message above", "the fallback is what these checks must not hit")

	for _, check := range []string{
		"af-home", "config", "state-dir", "repo-state-dir", "log-storage",
		"git", "git-repo", "git-identity", "tmux",
	} {
		t.Run(check, func(t *testing.T) {
			got := findingRemediation(Finding{Check: check}, false)
			require.NotEqual(t, generic, got)
			require.NotContains(t, got, "inspect the message above")
			require.True(t, strings.Contains(got, "af doctor --setup"),
				"a setup fix line should say how to re-check: %q", got)
		})
	}
}
