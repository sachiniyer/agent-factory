package task

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectPathIdentityAudit is the cross-resolver matrix for #3931.
// Expected hashes name fixture paths explicitly, independently of the resolvers.
func TestProjectPathIdentityAudit(t *testing.T) {
	base := testguard.CanonicalTempDir(t)
	repo, plain := filepath.Join(base, "repo"), filepath.Join(base, "plain")
	require.NoError(t, exec.Command("git", "init", repo).Run())
	require.NoError(t, os.MkdirAll(filepath.Join(repo, "sub"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(plain, "sub"), 0755))
	repoAlias, plainAlias := filepath.Join(base, "repo-alias"), filepath.Join(base, "plain-alias")
	require.NoError(t, os.Symlink(repo, repoAlias))
	require.NoError(t, os.Symlink(plain, plainAlias))
	nestedAlias := filepath.Join(repo, "outside")
	require.NoError(t, os.Symlink(plain, nestedAlias))
	missing := filepath.Join(base, "missing")
	broken := filepath.Join(base, "broken")
	require.NoError(t, os.Symlink(missing, broken))
	h := config.RepoIDFromRoot
	for _, shape := range []struct {
		name, path, canonical, projectRoot string
		git                                bool
	}{
		{"git-root", repo, repo, repo, true},
		{"git-subdirectory", filepath.Join(repo, "sub"), repo, repo, true},
		{"git-symlink", repoAlias, repo, repo, true},
		{"non-git", plain, plain, "", false},
		{"non-git-symlink", plainAlias, plain, "", false},
		{"non-git-symlink-inside-git", nestedAlias, plain, "", false},
		{"missing", missing, missing, "", false},
		{"broken-symlink", broken, broken, "", false},
		{"missing-git-child", filepath.Join(repo, "gone"), repo, repo, false},
	} {
		for _, spelling := range []struct{ name, suffix string }{
			{"clean", ""}, {"raw", "/./"}, {"trailing-slash", "/"},
		} {
			t.Run(shape.name+"/"+spelling.name, func(t *testing.T) {
				path := shape.path + spelling.suffix
				targetID, displayID, writerID := h(path), h(filepath.Clean(path)), ""
				if shape.name == "non-git" || shape.name == "non-git-symlink" || shape.name == "non-git-symlink-inside-git" {
					targetID, displayID = h(plain), h(plain)
				}
				if shape.projectRoot != "" {
					displayID = h(shape.projectRoot)
				}
				if shape.git {
					targetID, writerID = h(repo), h(repo)
				}
				assert.Equal(t, targetID, config.RepoIDForPath(path), "config target")
				resolved := config.ResolveProjectPath(path)
				assert.Equal(t, displayID, resolved.ID, "config recorded path")
				assert.Equal(t, shape.projectRoot, resolved.Root, "fallback must not prove a repository")
				assert.Equal(t, targetID, newRepoScope(path).id, "task target")
				assert.Equal(t, displayID, resolveProjectID(path), "task recorded path")
				assert.Equal(t, writerID, repoIDForPath(path), "task writer")
				switched, err := config.RepoFromPath(path) // TUI project switch's admission resolver.
				if shape.git {
					require.NoError(t, err)
					assert.Equal(t, writerID, switched.ID)
				} else {
					assert.Error(t, err)
				}

				// Compare each spelling to the canonical directory, in BOTH directions:
				// exact-match fast paths otherwise conceal target/recorded-path asymmetry.
				canonicalID := h(shape.canonical)
				got, _ := newRepoScope(shape.canonical).matches(Task{ProjectPath: path})
				assert.Equal(t, path == shape.canonical || displayID == canonicalID, got, "canonical target")
				reverse, _ := newRepoScope(path).matches(Task{ProjectPath: shape.canonical})
				assert.Equal(t, path == shape.canonical || targetID == canonicalID, reverse, "variant target")
				exact, _ := newRepoScope(path).matches(Task{ProjectPath: path})
				assert.True(t, exact, "unavailable exact paths remain addressable")
				retained, _ := newRepoScopeWithID(path, "retained").matches(Task{ProjectPath: path, RepoID: "retained"})
				assert.True(t, retained)
				foreign, _ := newRepoScope(path).matches(Task{ProjectPath: path, RepoID: "other"})
				assert.False(t, foreign, "retained ID precedes even an exact path")
			})
		}
	}
}

func TestProjectPathFallbackDisplayDoesNotMigrate(t *testing.T) {
	base := testguard.CanonicalTempDir(t)
	plain, alias := filepath.Join(base, "plain"), filepath.Join(base, "alias")
	require.NoError(t, os.Mkdir(plain, 0755))
	require.NoError(t, os.Symlink(plain, alias))
	oldID := config.RepoIDFromRoot(alias)
	tasksPath := setupTestTasks(t, []Task{
		{ID: "legacy01", Name: "legacy", ProjectPath: alias},
		{ID: "retain01", Name: "retained", ProjectPath: alias, RepoID: oldID},
	})
	// setupTestTasks writes the old array format; finish its schema upgrade
	// before measuring whether display resolution changes any stored identity.
	_, err := LoadTasks()
	require.NoError(t, err)
	before, err := os.ReadFile(tasksPath)
	require.NoError(t, err)
	got, err := LoadTasksForRepo(plain)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "legacy01", got[0].ID)
	assert.Empty(t, got[0].RepoID, "display must not stamp an invented binding")
	retained, err := LoadTasksForKnownRepo(alias, oldID)
	require.NoError(t, err)
	// Exact path matching keeps the legacy row addressable too.
	require.Len(t, retained, 2)
	after, err := os.ReadFile(tasksPath)
	require.NoError(t, err)
	assert.Equal(t, before, after, "display cannot rewrite retained IDs or legacy rows")

	// Fallbacks must be recomputed: removing an alias restores its historical
	// unavailable-path hash rather than retaining a cached directory identity.
	assert.Equal(t, config.RepoIDFromRoot(plain), resolveProjectID(alias))
	_, cached := projectIDMemo.Load(alias)
	assert.False(t, cached)
	require.NoError(t, os.Remove(alias))
	assert.Equal(t, oldID, resolveProjectID(alias))
	assert.Equal(t, oldID, newRepoScope(alias).id)
}
