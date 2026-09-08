package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestReservedTitleRemedyUsesRepoPath(t *testing.T) {
	m := newTitleAdmissionManager()
	repo := t.TempDir()
	for _, err := range []error{
		m.validateTitleAvailableLocked("repo", repo, "root", "claude", runtimeNamespaceLocalTmux, false, nil, false),
		m.validateTitleClaimableLocked("repo", repo, "root", "claude", runtimeNamespaceLocalTmux, false, nil, nil, false),
	} {
		require.Error(t, err)
		require.Contains(t, err.Error(), "af projects add "+config.ShellQuotePath(repo))
		require.Contains(t, err.Error(), "af config set --project "+config.ShellQuotePath(repo))
	}
}
