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

func TestResetProjectRegistryMissingHomeIsReadOnly(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, ResetProjectRegistry())
	_, statErr := os.Stat(home)
	require.ErrorIs(t, statErr, os.ErrNotExist, "an empty reset must not materialize the AF home")
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
		_, err := os.Stat(projectRecordPath(filepath.Join(base, "af-home", ProjectRegistryDirName), project.ID))
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
	_, err = os.Stat(projectCheckoutMarkerPath(t, repo))
	require.NoError(t, err, "registration must anchor identity in the Git common directory")
}

func TestResetProjectRegistryRefusesMarkerItDoesNotOwn(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	repo := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	registered, err := RegisterProject(repo)
	require.NoError(t, err)

	marker := projectCheckoutMarkerPath(t, repo)
	foreignID := "chk_ffffffffffffffffffffffffffffffff"
	require.NoError(t, os.WriteFile(marker, []byte(foreignID+"\n"), 0o644))

	err = ResetProjectRegistry()
	require.Error(t, err)
	require.Contains(t, err.Error(), registered.ID)
	data, readErr := os.ReadFile(marker)
	require.NoError(t, readErr)
	require.Equal(t, foreignID+"\n", string(data), "reset must not remove a marker it cannot validate")
	_, statErr := os.Stat(filepath.Join(home, ProjectRegistryDirName, registered.ID, projectMetadataFileName))
	require.NoError(t, statErr, "the registry must remain retryable when marker validation fails")
}

func TestResetProjectRegistryClearsUnavailableRoots(t *testing.T) {
	for _, tc := range []struct {
		name       string
		makeAbsent func(*testing.T, string)
	}{
		{
			name: "moved",
			makeAbsent: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.Rename(root, filepath.Join(filepath.Dir(root), "moved-repo")))
			},
		},
		{
			name: "deleted",
			makeAbsent: func(t *testing.T, root string) {
				t.Helper()
				require.NoError(t, os.RemoveAll(root))
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			home := filepath.Join(base, "af-home")
			t.Setenv("AGENT_FACTORY_HOME", home)
			repo := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
			_, err := RegisterProject(repo)
			require.NoError(t, err)
			tc.makeAbsent(t, repo)

			projects, err := ListProjects()
			require.NoError(t, err)
			require.Len(t, projects, 1)
			require.False(t, projects[0].PathExists)

			require.NoError(t, ResetProjectRegistry(),
				"an unavailable last-known root must not strand AF's registry")
			projects, err = ListProjects()
			require.NoError(t, err)
			require.Empty(t, projects)
			require.NoDirExists(t, filepath.Join(home, ProjectRegistryDirName))
		})
	}
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

func TestProjectRegistryMoveIgnoresReusedUnmarkedOldRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(Project, string) (Project, error)
	}{
		{
			name: "register",
			move: func(_ Project, movedRoot string) (Project, error) {
				return RegisterProject(movedRoot)
			},
		},
		{
			name: "rebind",
			move: func(project Project, movedRoot string) (Project, error) {
				return RebindProject(project.ID, movedRoot)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
			oldRoot := filepath.Join(base, "repo")
			project, err := RegisterProject(initProjectRegistryRepo(t, oldRoot))
			require.NoError(t, err)

			movedRoot := filepath.Join(base, "moved")
			require.NoError(t, os.Rename(oldRoot, movedRoot))
			require.NoError(t, os.Mkdir(oldRoot, 0o755),
				"an unrelated directory now reuses the last-known path")

			moved, err := tc.move(project, movedRoot)
			require.NoError(t, err,
				"an existing old path without this checkout's marker is not a duplicate identity")
			require.Equal(t, project.ID, moved.ID)
			require.Equal(t, project.CheckoutID, moved.CheckoutID)
			require.Equal(t, canonicalExistingPath(t, movedRoot), moved.Root)
		})
	}
}

func TestProjectRegistryMoveIgnoresReusedFileAtOldRoot(t *testing.T) {
	for _, tc := range []struct {
		name string
		move func(Project, string) (Project, error)
	}{
		{
			name: "register",
			move: func(_ Project, movedRoot string) (Project, error) {
				return RegisterProject(movedRoot)
			},
		},
		{
			name: "rebind",
			move: func(project Project, movedRoot string) (Project, error) {
				return RebindProject(project.ID, movedRoot)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
			oldRoot := filepath.Join(base, "repo")
			project, err := RegisterProject(initProjectRegistryRepo(t, oldRoot))
			require.NoError(t, err)

			movedRoot := filepath.Join(base, "moved")
			require.NoError(t, os.Rename(oldRoot, movedRoot))
			require.NoError(t, os.WriteFile(oldRoot, []byte("unrelated\n"), 0o644),
				"an unrelated file now reuses the last-known path")

			moved, err := tc.move(project, movedRoot)
			require.NoError(t, err,
				"a non-directory old path cannot contain this checkout's marker")
			require.Equal(t, project.ID, moved.ID)
			require.Equal(t, project.CheckoutID, moved.CheckoutID)
			require.Equal(t, canonicalExistingPath(t, movedRoot), moved.Root)
		})
	}
}

func TestResetProjectRegistryPreservesAnotherHomesCheckoutIdentity(t *testing.T) {
	base := t.TempDir()
	repo := initProjectRegistryRepo(t, filepath.Join(base, "repo"))
	firstHome := filepath.Join(base, "first-home")
	secondHome := filepath.Join(base, "second-home")

	t.Setenv("AGENT_FACTORY_HOME", firstHome)
	_, err := RegisterProject(repo)
	require.NoError(t, err)

	t.Setenv("AGENT_FACTORY_HOME", secondHome)
	second, err := RegisterProject(repo)
	require.NoError(t, err)
	secondBinding, err := resolveProjectBinding(repo)
	require.NoError(t, err)

	t.Setenv("AGENT_FACTORY_HOME", firstHome)
	require.NoError(t, ResetProjectRegistry())

	t.Setenv("AGENT_FACTORY_HOME", secondHome)
	require.FileExists(t, secondBinding.checkoutMarkerPath,
		"resetting one AF home must not remove another home's checkout identity")
	again, err := RegisterProject(repo)
	require.NoError(t, err,
		"the untouched home must still resolve the checkout after the other home resets")
	require.Equal(t, second.ID, again.ID)
	require.Equal(t, second.CheckoutID, again.CheckoutID)
}

func TestProjectRegistryHomeAliasesShareCheckoutIdentity(t *testing.T) {
	base := t.TempDir()
	realHome := filepath.Join(base, "real-home")
	require.NoError(t, os.MkdirAll(realHome, 0o755))
	aliasHome := filepath.Join(base, "alias-home")
	require.NoError(t, os.Symlink(realHome, aliasHome))
	repo := initProjectRegistryRepo(t, filepath.Join(base, "repo"))

	t.Setenv("AGENT_FACTORY_HOME", realHome)
	registered, err := RegisterProject(repo)
	require.NoError(t, err)
	realMarker := projectCheckoutMarkerPath(t, repo)

	t.Setenv("AGENT_FACTORY_HOME", aliasHome)
	throughAlias, err := RegisterProject(repo)
	require.NoError(t, err,
		"two spellings of one AF home must not mint competing checkout identities")
	require.Equal(t, registered, throughAlias)
	require.Equal(t, realMarker, projectCheckoutMarkerPath(t, repo))
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

func TestRejectedReplacementRegistrationDoesNotMintCheckoutMarker(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	originalPath := filepath.Join(base, "repo")
	original, err := RegisterProject(initProjectRegistryRepo(t, originalPath))
	require.NoError(t, err)
	require.NoError(t, os.Rename(originalPath, filepath.Join(base, "old-checkout")))

	replacement := initProjectRegistryRepo(t, originalPath)
	marker := projectCheckoutMarkerPath(t, replacement)
	require.NoFileExists(t, marker)

	_, err = RegisterProject(replacement)
	require.Error(t, err)
	require.Contains(t, err.Error(), original.ID)
	require.NoFileExists(t, marker,
		"a rejected registration must not mutate a different checkout")
	require.NoFileExists(t, marker+".lock")

	require.NoError(t, ResetProjectRegistry(),
		"the rejected checkout must not poison cleanup of the original record")
	require.NoDirExists(t, filepath.Join(home, ProjectRegistryDirName))
}

func TestRegisterProjectRejectsCopiedCheckoutMarkerAtTwoLiveRoots(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	firstRoot := initProjectRegistryRepo(t, filepath.Join(base, "first"))
	first, err := RegisterProject(firstRoot)
	require.NoError(t, err)
	markerData, err := os.ReadFile(projectCheckoutMarkerPath(t, firstRoot))
	require.NoError(t, err)

	secondRoot := initProjectRegistryRepo(t, filepath.Join(base, "second"))
	secondMarker := projectCheckoutMarkerPath(t, secondRoot)
	require.NoError(t, os.MkdirAll(filepath.Dir(secondMarker), 0o755))
	require.NoError(t, os.WriteFile(secondMarker, markerData, 0o644))
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

func TestRebindProjectDoesNotReuseCheckoutIDAtReplacementPath(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	originalPath := filepath.Join(base, "repo")
	original, err := RegisterProject(initProjectRegistryRepo(t, originalPath))
	require.NoError(t, err)
	require.NoError(t, os.Rename(originalPath, filepath.Join(base, "old-checkout")))

	// A fresh clone now occupies the same spelling as the last-known root, but
	// has no checkout marker. Path equality is not proof that it is the checkout
	// moved aside above; explicit rebind preserves only the stable project ID.
	replacement := initProjectRegistryRepo(t, originalPath)
	rebound, err := RebindProject(original.ID, replacement)
	require.NoError(t, err)
	require.Equal(t, original.ID, rebound.ID)
	require.NotEqual(t, original.CheckoutID, rebound.CheckoutID,
		"a replacement clone must mint its own checkout identity even at the old path")
}

func TestRegisterProjectBareBackedWorktreesUseCheckedOutRoot(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	require.NoError(t, os.WriteFile(filepath.Join(seed, "README.md"), []byte("test\n"), 0o644))
	runProjectRegistryGit(t, seed, "add", "README.md")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "-m", "initial")

	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	firstRoot := filepath.Join(base, "first-worktree")
	secondRoot := filepath.Join(base, "second-worktree")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", firstRoot)
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", secondRoot)

	first, err := RegisterProject(firstRoot)
	require.NoError(t, err)
	require.Equal(t, canonicalExistingPath(t, firstRoot), first.Root,
		"a bare backing repo has no main working tree, so its checked-out worktree is the usable root")
	second, err := RegisterProject(secondRoot)
	require.NoError(t, err)
	require.Equal(t, first.ID, second.ID, "worktrees sharing one bare common directory share checkout identity")
	require.Equal(t, first.Root, second.Root, "the first live registered worktree remains the canonical root")
}

func TestProjectRegistryRefusesNewerMetadata(t *testing.T) {
	base := t.TempDir()
	home := filepath.Join(base, "af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)
	id := "prj_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	dir := filepath.Join(home, ProjectRegistryDirName, id)
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

func projectCheckoutMarkerPath(t *testing.T, root string) string {
	t.Helper()
	binding, err := resolveProjectBinding(root)
	require.NoError(t, err)
	return binding.checkoutMarkerPath
}

func canonicalExistingPath(t *testing.T, path string) string {
	t.Helper()
	resolved, err := filepath.EvalSymlinks(path)
	require.NoError(t, err)
	return filepath.Clean(resolved)
}
