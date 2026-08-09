package api

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectScopeResolversPropagateGitDiscoveryErrors pins the fail-closed
// half of optional project discovery. Being outside Git is the one condition
// that may widen a command to all projects; broken repository metadata must not
// turn a scoped task/session operation into an unscoped one (#3134).
func TestProjectScopeResolversPropagateGitDiscoveryErrors(t *testing.T) {
	resetScopeFlags(t)
	t.Chdir(t.TempDir())
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing-git-dir"))

	_, discoveryErr := config.CurrentRepo()
	require.Error(t, discoveryErr, "fixture must make Git discovery fail")
	require.False(t, errors.Is(discoveryErr, config.ErrNotGitRepository),
		"fixture must exercise a real Git failure, not the allowed outside-repository fallback")

	previousDaemonURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = ""
	t.Cleanup(func() { apiclient.FlagDaemonURL = previousDaemonURL })
	t.Setenv("AF_DAEMON_URL", "")

	tests := []struct {
		name    string
		resolve func() error
	}{
		{
			name: "task scope",
			resolve: func() error {
				_, err := resolveProjectScope(false)
				return err
			},
		},
		{
			name: "local write scope",
			resolve: func() error {
				_, err := resolveRepoID()
				return err
			},
		},
		{
			name: "local read scope",
			resolve: func() error {
				_, err := resolveRepoIDForLookup()
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.resolve()
			require.Error(t, err, "ambiguous Git failure must fail closed")
			assert.Contains(t, err.Error(), "resolve current repository")
			assert.NotErrorIs(t, err, config.ErrNotGitRepository)
		})
	}
}
