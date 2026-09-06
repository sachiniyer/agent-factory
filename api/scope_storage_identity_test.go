package api

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

func TestSessionScopeKeepsHistoricalNonGitAlias(t *testing.T) {
	root := testguard.CanonicalTempDir(t)
	alias := filepath.Join(testguard.CanonicalTempDir(t), "alias")
	require.NoError(t, os.Symlink(root, alias))
	for _, path := range []string{root, alias, alias + "/./", alias + "/", alias + "/missing"} {
		require.Equal(t, config.RepoIDFromRoot(path), sessionRepoID(&session.InstanceData{Path: path}))
	}
	require.Equal(t, config.RepoIDFromRoot(root), config.RepoIDForPath(alias), "display has a different fallback contract")
}
