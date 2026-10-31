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
		"git", "git-identity", "tmux",
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

// git-repo carries two different findings; the remediation lives on the
// finding, so an unanswered probe never gets "git init" advice (Codex P2 on #6395).
func TestGitRepoRemediationFollowsTheFinding(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	chdir(t, t.TempDir()) // a directory that is not a repository
	report := &Report{}
	checkGit(report)
	findings := findByCheck(report, "git-repo")
	require.NotEmpty(t, findings)
	require.Contains(t, findings[0].Remediation, "git init")
	require.Contains(t, findings[0].Remediation, "af doctor --setup")
}
