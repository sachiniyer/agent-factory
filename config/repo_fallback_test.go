package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProjectPathFallbackSpelling(t *testing.T) {
	base := testguard.CanonicalTempDir(t)
	plain := filepath.Join(base, "plain")
	require.NoError(t, os.Mkdir(plain, 0755))
	alias := filepath.Join(base, "alias")
	require.NoError(t, os.Symlink(plain, alias))
	for _, path := range []string{plain, plain + "/./", plain + "/", alias, alias + "/./", alias + "/", filepath.Join(base, "missing"), base + "/missing/./"} {
		t.Run(path, func(t *testing.T) {
			assert.Equal(t, RepoIDFromRoot(path), RepoIDForPath(path))
			assert.Equal(t, ResolvedProject{ID: RepoIDFromRoot(filepath.Clean(path))}, ResolveProjectPath(path))
		})
	}
}
