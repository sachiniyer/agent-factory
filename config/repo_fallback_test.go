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
			target, recorded := path, filepath.Clean(path)
			if path == plain || path == plain+"/./" || path == plain+"/" || path == alias || path == alias+"/./" || path == alias+"/" {
				target, recorded = plain, plain
			}
			assert.Equal(t, RepoIDFromRoot(target), RepoIDForPath(path))
			assert.Equal(t, ResolvedProject{ID: RepoIDFromRoot(recorded)}, ResolveProjectPath(path))
		})
	}
}
