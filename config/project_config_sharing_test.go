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

// TestGitDirAtRootPointsAtCommonDirDetectsSeparateGitDir pins the
// direct-gitdir half of the shared-marker detection. sharedWorktreeCommonDir
// recognizes a linked worktree's .git file (whose gitdir target sits inside
// <commonDir>/worktrees) and a main checkout's .git directory; the
// separate-git-dir layout (git init --separate-git-dir) leaves <root>/.git
// as a regular "gitdir: <commonDir>" file that points DIRECTLY at commonDir
// rather than into its worktrees subdir, so both of those predicates miss
// it. Two such checkouts both pointing at the same external common
// directory share the marker at commonDir the same way a linked worktree
// does, so gitDirAtRootPointsAtCommonDir must read true here — without it,
// the absent-marker scan would skip the registry probe and the
// claimed-marker branch would fall through to the copied-marker deletion
// remedy even when another registration shares the marker through the
// same external git dir.
func TestGitDirAtRootPointsAtCommonDirDetectsSeparateGitDir(t *testing.T) {
	base := t.TempDir()
	// A separate-git-dir checkout: <root>/.git is a gitdir file pointing
	// at the common dir directly (not into its worktrees subdir), so the
	// direct-gitdir predicate must read true even though the common dir
	// lives outside the root.
	root := filepath.Join(base, "root")
	separateDir := filepath.Join(base, "separate.git")
	require.NoError(t, os.MkdirAll(root, 0o755))
	runProjectRegistryGit(t, base, "init", "--quiet", "--separate-git-dir", separateDir, root)
	separateBinding, err := resolveProjectBinding(root)
	require.NoError(t, err, "a separate-git-dir checkout must resolve like any other")
	require.False(t, sameProjectPath(separateBinding.root, separateBinding.gitCommonDir),
		"--separate-git-dir must keep the common dir outside the root for this test to mean anything")
	require.True(t, gitDirAtRootPointsAtCommonDir(separateBinding.root, separateBinding.gitCommonDir),
		"a separate-git-dir checkout's .git file points directly at commonDir and the predicate must catch it")
	require.False(t, gitDirAtRootIsSymlink(separateBinding.root),
		"the .git file is a regular file, not a symlink — the symlink predicate must not double-count it")

	// A plain main checkout's .git is a directory, not a gitdir file at
	// commonDir, so the direct-gitdir predicate stays false.
	mainRoot := initProjectRegistryRepo(t, filepath.Join(base, "main"))
	mainBinding, err := resolveProjectBinding(mainRoot)
	require.NoError(t, err)
	require.False(t, gitDirAtRootPointsAtCommonDir(mainBinding.root, mainBinding.gitCommonDir),
		"a plain main checkout whose .git is a directory does not point at commonDir")

	// A linked worktree's .git file points INTO <commonDir>/worktrees/<name>,
	// not at commonDir directly, so the direct-gitdir predicate stays false
	// and the linked-worktree predicate handles that shape instead.
	seed := initProjectRegistryRepo(t, filepath.Join(base, "seed"))
	runProjectRegistryGit(t, seed, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, seed, "config", "user.name", "Test")
	runProjectRegistryGit(t, seed, "commit", "--quiet", "--allow-empty", "-m", "initial")
	bare := filepath.Join(base, "backing.git")
	runProjectRegistryGit(t, base, "clone", "--quiet", "--bare", seed, bare)
	linked := filepath.Join(base, "linked")
	runProjectRegistryGit(t, base, "--git-dir", bare, "worktree", "add", "--quiet", "--detach", linked)
	linkedBinding, err := resolveProjectBinding(linked)
	require.NoError(t, err)
	require.False(t, gitDirAtRootPointsAtCommonDir(linkedBinding.root, linkedBinding.gitCommonDir),
		"a linked worktree's .git points at <commonDir>/worktrees/<name>, not commonDir directly")
	require.True(t, sharedWorktreeCommonDir(linkedBinding.root, linkedBinding.gitCommonDir),
		"linked-worktree predicate handles the worktree metadata shape, not the direct-gitdir predicate")
}

// TestMainCheckoutHasLinkedWorktreesRejectsCopiedWorktreeMetadata pins the
// backlink-validation half of the worktree-predicate refinement. A
// repository that has spawned linked worktrees, copied wholesale over a
// fresh root, drags both the owner's checkout marker and the
// <commonDir>/worktrees/<name> subtree along with it, but the gitdir file
// inside each copied <name> entry still points at the ORIGINAL worktree's
// <root>/.git, and that .git file's gitdir pointer in turn still names the
// ORIGINAL common directory's worktrees path, not the copy's. The prior
// code counted any copied entry as proof of live sharing and refused the
// safe marker-removal/rebind recovery for the copy indefinitely; the fix
// validates each entry's backlink and disregards copied or stale entries,
// so the copy's mainCheckoutHasLinkedWorktrees reads false while the
// original's still reads true.
func TestMainCheckoutHasLinkedWorktreesRejectsCopiedWorktreeMetadata(t *testing.T) {
	base := t.TempDir()
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	runProjectRegistryGit(t, fooRoot, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, fooRoot, "config", "user.name", "Test")
	runProjectRegistryGit(t, fooRoot, "commit", "--quiet", "--allow-empty", "-m", "initial")
	// Spawn a real linked worktree from /foo so /foo/.git/worktrees/<name>
	// is non-empty and the entry's gitdir closes back onto /foo/.git.
	fooSibling := filepath.Join(base, "foo-sibling")
	runProjectRegistryGit(t, fooRoot, "worktree", "add", "--quiet", "--detach", fooSibling)
	fooBinding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, mainCheckoutHasLinkedWorktrees(fooBinding.gitCommonDir),
		"/foo's git common dir backs the linked worktree git spawned from it")

	// Copy /foo wholesale to /bar. The copy keeps /foo's .git directory
	// (including the marker and the copied worktrees metadata), but the
	// linked worktree's .git file the copied gitdir entry points at still
	// names /foo's git common directory, not the copy's.
	barRoot := filepath.Join(base, "bar")
	require.NoError(t, exec.Command("cp", "-R", fooRoot, barRoot).Run())
	barBinding, err := resolveProjectBinding(barRoot)
	require.NoError(t, err)
	require.False(t, mainCheckoutHasLinkedWorktrees(barBinding.gitCommonDir),
		"the copy's worktrees entry points at the original worktree, not the copy, so the backlink does not close and the entry is disregarded")
}

// TestResolveProjectSelectorRejectsSharedMarkerDirectExternalGitDir pins
// the claimed-marker branch's refusal when <root>/.git is a regular
// "gitdir: <path>" file pointing directly at binding.gitCommonDir — the
// separate-git-dir shape two checkouts may share. The marker at /foo's
// binding path (the external common dir's agent-factory marker) is
// overwritten with /bar's checkout id, so /foo takes the claimed-marker
// branch; the owner probe resolves bar to a different git common dir,
// bar's recorded root still carries a matching marker, and the three
// sharing predicates sharedWorktreeCommonDir catches the linked-worktree
// case, mainCheckoutHasLinkedWorktrees catches the main-with-worktrees
// case, and symlinkedRootMarkerRefusal catches the symlink case — but all
// three miss the direct-gitdir-file shape. Without the new
// directGitDirFileMarkerRefusal guard the branch falls through to the
// "remove the copied checkout marker" remedy and tells the user to
// delete a marker another registered checkout may share through the same
// external git directory; with the guard the write path refuses and
// names the direct-gitdir-file shape instead.
func TestResolveProjectSelectorRejectsSharedMarkerDirectExternalGitDir(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	// /foo is registered as a separate-git-dir checkout: /foo/.git is a
	// regular "gitdir: <external>" file pointing directly at /external,
	// not a symlink and not a directory.
	externalGit := filepath.Join(base, "external.git")
	fooRoot := filepath.Join(base, "foo")
	require.NoError(t, os.MkdirAll(fooRoot, 0o755))
	runProjectRegistryGit(t, base, "init", "--quiet", "--separate-git-dir", externalGit, fooRoot)
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	// /bar is an unrelated main checkout; its marker belongs to bar and
	// is private to /bar's own .git directory, so the claimed-marker owner
	// probe below resolves it to a different git common dir.
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Overwrite the marker at /foo's binding path (inside the shared
	// external common dir) with bar's checkout id, mimicking the claimed
	// marker case. The marker is at /external/af/...: this is the file
	// another registration pointing at /external would share.
	fooBinding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, gitDirAtRootPointsAtCommonDir(fooBinding.root, fooBinding.gitCommonDir),
		"/foo/.git is a regular gitdir file pointing directly at the shared external git dir")
	require.NoError(t, os.MkdirAll(filepath.Dir(fooBinding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(fooBinding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))
	// /bar's recorded root still carries bar's own marker so the owner
	// probe below (a different common dir) reaches the new refusal.
	barBinding, err := resolveProjectBinding(barRoot)
	require.NoError(t, err)
	require.False(t, sameProjectPath(barBinding.gitCommonDir, fooBinding.gitCommonDir),
		"/bar must resolve to a different git common dir than /foo's shared external dir")
	require.NoError(t, os.MkdirAll(filepath.Dir(barBinding.checkoutMarkerPath), 0o755))
	require.NoError(t, os.WriteFile(barBinding.checkoutMarkerPath, []byte(bar.CheckoutID), 0o644))

	// Give bar a real personal override so the refused write can be checked
	// for "no mutation".
	_, err = SetProjectConfigValue(bar.ID, "default_program", "codex")
	require.NoError(t, err)
	barPersonalPath, err := ProjectConfigTomlPath(bar.ID)
	require.NoError(t, err)
	barBefore, err := os.ReadFile(barPersonalPath)
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)

	// /foo's CLI write path must refuse without recommending removal:
	// /foo's <root>/.git is a regular gitdir file pointing directly at an
	// external git directory another registered checkout could share,
	// and the marker at /foo's binding path may be bar's own shared
	// marker rather than a private copy af can ask the user to delete.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has marker "+bar.CheckoutID+" instead of "+foo.CheckoutID)
	assert.Contains(t, err.Error(), "regular gitdir file pointing directly at the external git directory")
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

// TestResolveProjectSelectorMissingMarkerScansUnrelatedUnresolvableRootForDirectGitDirFile
// pins the absent-marker half of the direct-gitdir-file fix. /foo's
// <root>/.git is a regular "gitdir: <external>" file pointing directly at
// an external common dir; without the new gitDirAtRootPointsAtCommonDir
// predicate all three of the existing possibly-shared predicates return
// false (it is not a linked worktree, the external common dir has no
// worktrees subdir, and the .git file is not a symlink), so the
// absent-marker scan would skip the registry probe for any other root and
// reach the rebind advice. With the predicate the scan runs even for the
// direct-gitdir-file shape, so an unresolvable unrelated registration
// (whose root was removed) makes the write path fail closed naming it
// instead of recommending a rebind that would write into the shared
// external directory.
func TestResolveProjectSelectorMissingMarkerScansUnrelatedUnresolvableRootForDirectGitDirFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	// /foo is a separate-git-dir checkout: /foo/.git is a regular
	// "gitdir: <external>" file pointing directly at /external.
	externalGit := filepath.Join(base, "external.git")
	fooRoot := filepath.Join(base, "foo")
	require.NoError(t, os.MkdirAll(fooRoot, 0o755))
	runProjectRegistryGit(t, base, "init", "--quiet", "--separate-git-dir", externalGit, fooRoot)
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	// /bar is an unrelated main checkout registered separately. Its
	// binding will be probed by /foo's absent-marker scan, and the
	// probe must fail closed when /bar's root is removed below.
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	fooBinding, err := resolveProjectBinding(fooRoot)
	require.NoError(t, err)
	require.True(t, gitDirAtRootPointsAtCommonDir(fooBinding.root, fooBinding.gitCommonDir),
		"/foo/.git is a regular gitdir file pointing directly at the external common dir")
	// Strip /foo's marker at the shared external dir so /foo's CLI write
	// path takes the absent-marker branch.
	require.NoError(t, os.Remove(fooBinding.checkoutMarkerPath))

	// Remove /bar's registered root so its binding fails to resolve.
	require.NoError(t, os.RemoveAll(barRoot))
	_, ownerUnresolvableErr := resolveProjectBinding(barRoot)
	require.Error(t, ownerUnresolvableErr,
		"bar's root should no longer resolve after removal")

	// With the direct-gitdir predicate the absent-marker scan now runs,
	// probes /bar (the only other registration), and fails closed
	// naming the unresolvable root instead of recommending the rebind
	// that would write a new marker into the shared external dir.
	_, err = ResolveProjectSelector(fooRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+foo.ID)
	assert.Contains(t, err.Error(), "has no checkout marker")
	assert.Contains(t, err.Error(), "could not be resolved")
	assert.Contains(t, err.Error(), bar.ID)
	assert.NotContains(t, err.Error(), "af projects rebind",
		"the direct-gitdir shape must run the shared-checkout scan rather than skip it for the rebind advice")
}

// TestResolveProjectSelectorCopiedRepoWorktreeMetadataDoesNotRefuseDeletion
// pins the claimed-marker branch's behavior when /foo's repository,
// including its linked-worktree metadata, is copied wholesale over a
// fresh main /bar that was registered separately. The copied .git
// directory carries /foo's checkout marker (so /bar takes the
// claimed-marker branch with checkoutID = foo's), and the copied
// <copyCommonDir>/worktrees/<name> entry still points back at /foo's
// original common dir, not the copy's. The prior code counted any copied
// entry as proof of live sharing and refused the safe marker-removal
// recovery indefinitely; with backlink validation the copied entry is
// disregarded, mainCheckoutHasLinkedWorktrees reads false on the copy,
// and the claimed-marker branch falls through to the copied-marker
// deletion remedy that lets the user remove the stale copy.
func TestResolveProjectSelectorCopiedRepoWorktreeMetadataDoesNotRefuseDeletion(t *testing.T) {
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))
	// /foo is a main checkout that has spawned a real linked worktree, so
	// /foo/.git/worktrees/<name> is non-empty and its entry's gitdir
	// closes back onto /foo/.git.
	fooRoot := initProjectRegistryRepo(t, filepath.Join(base, "foo"))
	runProjectRegistryGit(t, fooRoot, "config", "user.email", "test@example.com")
	runProjectRegistryGit(t, fooRoot, "config", "user.name", "Test")
	runProjectRegistryGit(t, fooRoot, "commit", "--quiet", "--allow-empty", "-m", "initial")
	fooSibling := filepath.Join(base, "foo-sibling")
	runProjectRegistryGit(t, fooRoot, "worktree", "add", "--quiet", "--detach", fooSibling)
	foo, err := RegisterProject(fooRoot)
	require.NoError(t, err)
	// /bar is an unrelated main checkout registered separately. Its own
	// .git directory's marker carries bar's checkout id; the copy below
	// overwrites that directory (and the marker) with /foo's.
	barRoot := initProjectRegistryRepo(t, filepath.Join(base, "bar"))
	bar, err := RegisterProject(barRoot)
	require.NoError(t, err)
	require.NotEqual(t, foo.ID, bar.ID)
	require.NotEqual(t, foo.CheckoutID, bar.CheckoutID)

	// Replace /bar/.git with a wholesale copy of /foo/.git. The copy
	// carries /foo's checkout marker (foo.CheckoutID) at
	// /bar/.git/af/...; /foo's recorded root still carries its own
	// matching marker so the claimed-marker owner probe below reaches
	// the deletion-advice fall-through.
	require.NoError(t, os.RemoveAll(filepath.Join(barRoot, ".git")))
	require.NoError(t, exec.Command("cp", "-R", filepath.Join(fooRoot, ".git"), filepath.Join(barRoot, ".git")).Run())
	barBinding, err := resolveProjectBinding(barRoot)
	require.NoError(t, err)
	require.False(t, mainCheckoutHasLinkedWorktrees(barBinding.gitCommonDir),
		"the copy's worktrees entry must NOT count as live: its gitdir backlink names /foo's original common dir, not the copy's")
	copyMarker, copyExists, err := readCheckoutID(barBinding.checkoutMarkerPath)
	require.NoError(t, err)
	require.True(t, copyExists)
	require.Equal(t, foo.CheckoutID, copyMarker,
		"the copied /bar/.git carries /foo's checkout marker")

	// Give foo a real personal override so the refused write can be
	// checked for "no mutation".
	_, err = SetProjectConfigValue(foo.ID, "default_program", "codex")
	require.NoError(t, err)
	fooPersonalPath, err := ProjectConfigTomlPath(foo.ID)
	require.NoError(t, err)
	fooBefore, err := os.ReadFile(fooPersonalPath)
	require.NoError(t, err)
	barPersonalPath, err := ProjectConfigTomlPath(bar.ID)
	require.NoError(t, err)

	// /bar's CLI write path takes the claimed-marker branch (the copied
	// marker is foo's, not bar's). mainCheckoutHasLinkedWorktrees on the
	// copied common dir reads false (stale backlink), so the branch must
	// fall through to the copied-marker deletion remedy that lets the
	// user remove the stale copy at /bar's binding path, instead of
	// refusing with the main-with-worktrees language the prior code
	// produced from the copied entry.
	_, err = ResolveProjectSelector(barRoot)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is already the last-known root of project "+bar.ID)
	assert.Contains(t, err.Error(), "has marker "+foo.CheckoutID+" instead of "+bar.CheckoutID)
	assert.Contains(t, err.Error(), "remove the copied checkout marker")
	assert.Contains(t, err.Error(), bar.ID)
	assert.NotContains(t, err.Error(), "main working tree that has spawned linked worktrees",
		"the copied worktree metadata must not refuse deletion the way a live linked worktree would")

	// The write path goes through ResolveProjectSelector, so it must
	// surface the same deletion-advice refusal and touch neither
	// project's personal file.
	_, err = SetProjectConfigValue(barRoot, "default_program", "codex")
	require.Error(t, err)
	_, barStatErr := os.Stat(barPersonalPath)
	assert.ErrorIs(t, barStatErr, os.ErrNotExist,
		"the refused write must not create bar's personal file")
	fooAfter, err := os.ReadFile(fooPersonalPath)
	require.NoError(t, err)
	assert.Equal(t, fooBefore, fooAfter, "the refused write must not touch foo's personal file")
}
