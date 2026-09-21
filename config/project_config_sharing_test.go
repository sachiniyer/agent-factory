package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerSymlinkedGitDir
// pins the symlinked-gitdir half of the retained-marker case. Like the
// main-with-worktrees case above, the marker at /foo's binding path belongs
// to bar and bar's recorded root still carries a matching marker; but here
// /foo is a MAIN checkout whose <root>/.git is a SYMLINK to an external git
// directory, so both sharedWorktreeCommonDir (os.Stat follows the symlink and
// reads a directory, not a regular gitdir file) and
// mainCheckoutHasLinkedWorktrees (the external target has no
// <commonDir>/worktrees subdir) return false. Another registered checkout
// whose <root>/.git points at the same external directory shares the marker at
// /foo's binding path, so af must refuse without recommending deletion
// instead of falling through to the "remove the copied checkout marker"
// remedy that the private-copy fall-through reaches when both predicates miss
// the symlink.
func TestResolveProjectSelectorRejectsSharedLinkedWorktreeMarkerSymlinkedGitDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	// /bar is the registered marker owner; it is later replaced with a
	// fresh independent repo that RETAINS bar's checkout marker, the same
	// retained-marker shape as the linked-worktree case above.
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	// /foo is the registered current checkout: it is set up as a main repo
	// first so RegisterProject writes its checkout marker at
	// /foo/.git/agent-factory/... and the binding resolves with root=/foo
	// and gitCommonDir=/foo/.git.
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Move /foo's .git to an external location and replace /foo/.git with
	// a symlink to it, so /foo's git common directory is the external .git
	// reached through the symlink while /foo itself stays a main checkout.
	externalGit := filepath.Join(base, "external.git")
	require.NoError(t, os.Rename(filepath.Join(fooRoot, ".git"), externalGit))
	require.NoError(t, os.Symlink(externalGit, filepath.Join(fooRoot, ".git")))
	// resolveProjectBinding canonicalizes the git common directory through
	// filepath.EvalSymlinks, and on macOS t.TempDir() returns /var/...
	// which is itself a symlink to /private/var/... — canonicalize the
	// external path the same way so the comparison holds on every platform.
	externalGit = canonicalExistingPath(t, externalGit)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.Equal(t, fooRoot, binding.root)
	require.Equal(t, externalGit, binding.gitCommonDir,
		"/foo's git common dir is the external .git reached through the symlink")
	require.True(t, gitDirAtRootIsSymlink(binding.root),
		"the symlink predicate catches the symlinked-<root>/.git case both main predicates miss")
	require.False(t, sharedWorktreeCommonDir(binding.root, binding.gitCommonDir),
		"os.Stat follows the symlink and reads a directory, so the linked-worktree predicate misses it")
	require.False(t, mainCheckoutHasLinkedWorktrees(binding.gitCommonDir),
		"the external .git has no worktrees subdir, so the main-with-worktrees predicate misses it")

	// Overwrite the marker at the shared external .git with bar's checkout
	// id so /foo takes the claimed-marker branch with checkoutID != foo.
	require.NoError(t, os.MkdirAll(filepath.Dir(binding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(binding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))

	// Replace bar's recorded root with a fresh independent repo that
	// retains bar's checkout marker, the same retained-marker shape as the
	// linked-worktree case: ownerBinding resolves to a different git common
	// dir than the external .git, and the owner-marker check passes
	// (matches owner.CheckoutID). Without the new symlink check, both main
	// predicates would miss the symlinked <root>/.git and fall through to
	// the "remove the copied checkout marker" remedy.
	require.NoError(t, os.RemoveAll(barRoot))
	initProjectRegistryRepo(t, barRoot)
	ownerBinding, err := resolveProjectBinding(bar.Root)
	require.NoError(t, err)
	require.False(t, sameProjectPath(ownerBinding.gitCommonDir, binding.gitCommonDir),
		"/bar resolves to a different git common dir than /foo's external .git")
	require.NoError(t, os.MkdirAll(filepath.Dir(ownerBinding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(ownerBinding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))
	retainedID, retainedExists, err := readCheckoutID(ownerBinding.checkoutMarkerPath)
	require.NoError(t, err)
	require.True(t, retainedExists, "bar's replaced root retains a matching marker")
	require.Equal(t, bar.CheckoutID, retainedID)

	// Give bar a real personal override so the refused write can be
	// checked for "no mutation".
	_, err = SetProjectConfigValue(bar.ID, "default_program", "codex")
	require.NoError(t, err)
	barPersonalPath, err := ProjectConfigTomlPath(bar.ID)
	require.NoError(t, err)
	barBefore, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)

	// The CLI write path must refuse without recommending removal: /foo's
	// <root>/.git is a symlink to an external git directory another
	// registration could share, and the marker at /foo's binding path may
	// be bar's own shared marker rather than a private copy af can ask the
	// user to delete.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "is a symlink to an external git directory")
	assert.Contains(t, err.Error(), bar.ID)
	assert.NotContains(t, err.Error(), "remove the copied checkout marker")

	// The write path goes through ResolveProjectSelector, so it must
	// surface the same refusal and touch neither project's personal file.
	_, err = SetProjectConfigValue(fooRoot, "default_program", "codex")
	require.Error(t, err)
	_, fooStatErr := os.Stat(fooPersonalPath)
	assert.ErrorIs(t, fooStatErr, os.ErrNotExist,
		"the refused write must not create foo's personal file")
	barAfter, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, barBefore, barAfter, "the refused write must not touch bar's personal file")
}

// TestResolveProjectSelectorBoundsSharedLinkedWorktreeMarkerOwnerProbe pins
// the bounded-probe half of the claimed-marker case. The owner probe here used
// to call resolveProjectBinding, which uses context.Background() internally;
// when the marker owner's recorded root has its git metadata on a wedged or
// unavailable mount, the probe never returned and `af config --project <path>
// set/unset` hung instead of failing closed with the unresolvable-owner refusal
// this branch is meant to surface. The absent-marker scan above already
// bounds each per-root probe to registeredProjectScanTimeout the same way
// projectForWorkspaceContext bounds the daemon's registry scan; the
// claimed-marker owner probe must respect the same budget so an unavailable
// owner fails closed within that deadline rather than hanging the CLI.
func TestResolveProjectSelectorBoundsSharedLinkedWorktreeMarkerOwnerProbe(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	// Register bar at a linked worktree of the bare: a worktree of a bare
	// repo resolves binding.root to the worktree path and
	// binding.gitCommonDir to the bare, so the marker the bare shares is
	// bar's registry marker.
	barRoot := filepath.Join(base, "bar")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", barRoot)
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Replace foo's registered root with another linked worktree of the
	// same bare. /foo's new binding.checkoutMarkerPath is the same shared
	// marker file bar's registration wrote, so it carries bar's checkout
	// ID (not foo's).
	require.NoError(t, os.RemoveAll(fooRoot))
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", fooRoot)
	binding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	markerID, _, err := readCheckoutID(binding.checkoutMarkerPath)
	require.NoError(t, err)
	require.Equal(t, bar.CheckoutID, markerID,
		"the marker at /foo's binding path is bar's own shared record marker")

	// Install a `git` wrapper that sleeps far longer than the scan budget
	// whenever af probes bar's recorded root, simulating git metadata on a
	// wedged mount. Without the fix the claimed-marker owner probe called
	// resolveProjectBinding (context.Background()) and blocked for the
	// full sleep; with the fix the bounded probe ends at
	// registeredProjectScanTimeout and the unresolvable-owner refusal
	// surfaces well inside that deadline.
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	binDir := t.TempDir()
	wrapper := filepath.Join(binDir, "git")
	require.NoError(t, os.WriteFile(wrapper, []byte("#!/bin/sh\ncase \" $* \" in\n  *\"$AF_STALLED_ROOT\"*) /bin/sleep 10; exit 1 ;;\nesac\nexec \"$AF_REAL_GIT\" \"$@\"\n"), 0o755))
	t.Setenv("AF_STALLED_ROOT", barRoot)
	t.Setenv("AF_REAL_GIT", realGit)
	t.Setenv("PATH", binDir)

	started := time.Now()
	_, err = ResolveProjectSelector(fooRoot)
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.Less(t, elapsed, 3*time.Second,
		"the claimed-marker owner probe must respect registeredProjectScanTimeout instead of hanging on a wedged owner root")
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "could not be resolved")
	assert.Contains(t, err.Error(), bar.ID)
}
