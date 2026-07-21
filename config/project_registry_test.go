package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestListProjectsMissingHomeIsReadOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Empty(t, projects)
	_, statErr := os.Stat(home)
	require.ErrorIs(t, statErr, os.ErrNotExist, "a read-only list must not materialize the AF home")
}

func TestProjectRegistryRebindPreservesIdentityAndClonesStayDistinct(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))

	originalRoot := initProjectRegistryRepo(t, filepath.Join(base, "checkout-before-move"))
	registered, err := RegisterProject(originalRoot)
	require.NoError(t, err)
	require.Regexp(t, projectIDPattern, registered.ID)
	require.Regexp(t, checkoutIDPattern, registered.CheckoutID)
	require.True(t, registered.PathExists)
	require.Equal(t, ".", registered.RelativeRoot)

	again, err := RegisterProject(originalRoot)
	require.NoError(t, err)
	require.Equal(t, registered.ID, again.ID, "registration of one canonical root must be idempotent")

	movedRoot := filepath.Join(base, "checkout-after-move")
	require.NoError(t, os.Rename(originalRoot, movedRoot))
	beforeRebind, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, beforeRebind, 1)
	require.False(t, beforeRebind[0].PathExists, "a moved checkout must retain only its unavailable path hint, not silently rebind")

	rebound, err := RebindProject(registered.ID, movedRoot)
	require.NoError(t, err)
	require.Equal(t, registered.ID, rebound.ID)
	require.Equal(t, registered.CheckoutID, rebound.CheckoutID)
	require.True(t, rebound.PathExists)
	require.Equal(t, canonicalExistingPath(t, movedRoot), rebound.Root)

	secondClone := initProjectRegistryRepo(t, filepath.Join(base, "second-clone"))
	second, err := RegisterProject(secondClone)
	require.NoError(t, err)
	require.NotEqual(t, rebound.ID, second.ID, "two clones need distinct project identities")
	require.NotEqual(t, rebound.CheckoutID, second.CheckoutID, "two clones need distinct checkout identities")

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 2)
	assert.ElementsMatch(t, []string{rebound.ID, second.ID}, []string{projects[0].ID, projects[1].ID})
	for _, project := range projects {
		_, err := os.Stat(projectRecordPath(filepath.Join(base, "af-home", "projects"), project.ID))
		require.NoError(t, err, "each listed identity must come from durable project.json metadata")
	}
}

func TestProjectRegistryPersistsSessionlessRepoFromArbitraryPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	repo := initProjectRegistryRepo(t, filepath.Join(base, "empty-repo"))
	nestedPath := filepath.Join(repo, "services", "api")
	require.NoError(t, os.MkdirAll(nestedPath, 0o755))

	registered, err := RegisterProject(nestedPath)
	require.NoError(t, err)
	require.Equal(t, repo, registered.Root, "an arbitrary path inside the checkout must bind the git repo root")
	require.Equal(t, ".", registered.RelativeRoot)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Equal(t, []Project{registered}, projects,
		"the registry must list an explicitly registered project with zero sessions and zero tasks")
	_, err = os.Stat(filepath.Join(repo, ".git", checkoutMarkerDirName, checkoutMarkerFileName))
	require.NoError(t, err, "registration must anchor identity in the Git common directory")
}

func TestProjectRegistryConcurrentRegistrationIsIdempotent(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	repo := initProjectRegistryRepo(t, filepath.Join(base, "repo"))

	const workers = 8
	ids := make(chan string, workers)
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			project, err := RegisterProject(repo)
			if err != nil {
				errs <- err
				return
			}
			ids <- project.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		require.Equal(t, first, id)
	}
	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1)
}

func TestProjectRegistryRebindRefusesAnotherProjectsRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	first, err := RegisterProject(initProjectRegistryRepo(t, filepath.Join(base, "first")))
	require.NoError(t, err)
	secondRoot := initProjectRegistryRepo(t, filepath.Join(base, "second"))
	second, err := RegisterProject(secondRoot)
	require.NoError(t, err)

	_, err = RebindProject(first.ID, secondRoot)
	require.Error(t, err)
	require.Contains(t, err.Error(), second.ID)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 2)
}

func TestRegisterProjectRediscoversWholeCheckoutMoveByMarker(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	originalRoot := initProjectRegistryRepo(t, filepath.Join(base, "before"))
	original, err := RegisterProject(originalRoot)
	require.NoError(t, err)

	movedRoot := filepath.Join(base, "after")
	require.NoError(t, os.Rename(originalRoot, movedRoot))
	rediscovered, err := RegisterProject(movedRoot)
	require.NoError(t, err)
	require.Equal(t, original.ID, rediscovered.ID)
	require.Equal(t, original.CheckoutID, rediscovered.CheckoutID)
	require.Equal(t, canonicalExistingPath(t, movedRoot), rediscovered.Root)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Equal(t, []Project{rediscovered}, projects)
}

func TestRegisterProjectLinkedWorktreeUsesMainCheckoutIdentity(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	mainRoot := initProjectRegistryRepo(t, filepath.Join(base, "main"))
	runProjectRegistryGit(t, mainRoot, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, mainRoot, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(mainRoot, "README.md"), []byte("test\n"), 0o644))
	runProjectRegistryGit(t, mainRoot, "add", "README.md")
	runProjectRegistryGit(t, mainRoot, "commit", "--quiet", "-m", "initial")
	worktreeRoot := filepath.Join(base, "linked")
	runProjectRegistryGit(t, mainRoot, "worktree", "add", "--quiet", "-b", "registry-linked", worktreeRoot)

	fromMain, err := RegisterProject(mainRoot)
	require.NoError(t, err)
	fromWorktree, err := RegisterProject(worktreeRoot)
	require.NoError(t, err)
	require.Equal(t, fromMain.ID, fromWorktree.ID)
	require.Equal(t, fromMain.CheckoutID, fromWorktree.CheckoutID)
	require.Equal(t, mainRoot, fromWorktree.Root)
}

func TestRegisterProjectDoesNotAdoptDifferentCloneAtReusedPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	originalPath := filepath.Join(base, "repo")
	originalRoot := initProjectRegistryRepo(t, originalPath)
	original, err := RegisterProject(originalRoot)
	require.NoError(t, err)

	require.NoError(t, os.Rename(originalRoot, filepath.Join(base, "old-checkout")))
	replacement := initProjectRegistryRepo(t, originalPath)
	_, err = RegisterProject(replacement)
	require.Error(t, err)
	require.Contains(t, err.Error(), original.ID)
	require.Contains(t, err.Error(), "marker")
	rebound, err := RebindProject(original.ID, replacement)
	require.NoError(t, err, "the explicit repair named by the refusal must adopt the replacement")
	require.Equal(t, original.ID, rebound.ID)
	require.NotEqual(t, original.CheckoutID, rebound.CheckoutID)

	projects, err := ListProjects()
	require.NoError(t, err)
	require.Len(t, projects, 1, "refusal followed by explicit rebind must leave one stable project record")
	require.Equal(t, original.ID, projects[0].ID)
}

func TestRegisterProjectRejectsCopiedCheckoutMarkerAtTwoLiveRoots(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	firstRoot := initProjectRegistryRepo(t, filepath.Join(base, "first"))
	first, err := RegisterProject(firstRoot)
	require.NoError(t, err)
	markerData, err := os.ReadFile(filepath.Join(firstRoot, ".git", checkoutMarkerDirName, checkoutMarkerFileName))
	require.NoError(t, err)

	secondRoot := initProjectRegistryRepo(t, filepath.Join(base, "second"))
	secondMarkerDir := filepath.Join(secondRoot, ".git", checkoutMarkerDirName)
	require.NoError(t, os.MkdirAll(secondMarkerDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(secondMarkerDir, checkoutMarkerFileName), markerData, 0o644))
	_, err = RegisterProject(secondRoot)
	require.Error(t, err)
	require.Contains(t, err.Error(), first.CheckoutID)
	require.Contains(t, err.Error(), "appears at both")
}

func TestRebindProjectCarriesProjectIDToNewCheckoutIdentity(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	original, err := RegisterProject(initProjectRegistryRepo(t, filepath.Join(base, "original")))
	require.NoError(t, err)
	replacementRoot := initProjectRegistryRepo(t, filepath.Join(base, "replacement"))

	rebound, err := RebindProject(original.ID, replacementRoot)
	require.NoError(t, err)
	require.Equal(t, original.ID, rebound.ID)
	require.NotEqual(t, original.CheckoutID, rebound.CheckoutID,
		"a reclone is a new checkout even when the user carries the stable project id forward")
	require.Equal(t, replacementRoot, rebound.Root)
}

func TestProjectRegistryRefusesNewerMetadata(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	id := "prj_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := filepath.Join(home, "projects", id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	metadata := `{
  "schema_version": 2,
  "id": "prj_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
  "checkout_id": "chk_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "root": "/repo",
  "checkout_root": "/repo",
  "relative_root": "."
}`
	require.NoError(t, os.WriteFile(filepath.Join(dir, projectMetadataFileName), []byte(metadata), 0o644))

	_, err := ListProjects()
	require.Error(t, err)
	require.Contains(t, err.Error(), "upgrade af")
}

func initProjectRegistryRepo(t *testing.T, root string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(root, 0o755))
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git init failed: %s", out)
	return canonicalExistingPath(t, root)
}

func runProjectRegistryGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed: %s", args, out)
}

func canonicalExistingPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return filepath.Clean(resolved)
}
