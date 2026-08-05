package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// PathExists answers ONE question — "is the last-known project root present?" —
// and the only honest "no" is a determinate one (#2885). A stat that FAILED is
// evidence of nothing, so reporting "gone" there tells a user their checkout
// vanished when it is merely unreadable. This is the same rule config_load.go's
// fileExists states, and the same one projectRootHasCheckoutID/readCheckoutID
// already follow in this very file.

func TestProjectPathExists_DeterminateAnswers(t *testing.T) {
	root := t.TempDir()

	dir := filepath.Join(root, "checkout")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	assert.True(t, projectPathExists(dir), "a readable directory is present")

	assert.False(t, projectPathExists(filepath.Join(root, "gone")),
		"a path that is determinately absent is the one honest false")

	file := filepath.Join(root, "notes.txt")
	require.NoError(t, os.WriteFile(file, []byte("x"), 0o644))
	assert.False(t, projectPathExists(file),
		"a regular file is determinately not a project root")

	// ENOTDIR is as determinate as ENOENT and must not be swept into the
	// fail-closed arm: an ancestor that is a regular file means NOTHING can exist
	// below it (#2889 review). Go does not map it to ErrNotExist, so a rule that
	// only special-cases ErrNotExist reports this as present — a fabricated
	// POSITIVE, the mirror of the bug this function was fixed for.
	ancestorIsAFile := filepath.Join(root, "notes.txt", "checkout")
	assert.False(t, projectPathExists(ancestorIsAFile),
		"a path under a regular-file ancestor cannot exist, and ENOTDIR proves it")
}

// The sibling errnos of ENOTDIR (#2948, from the #2889 review): each PROVES the
// path resolves to nothing, for any process with any credentials, so each is a
// determinate absence rather than the ambiguity that fails closed.
func TestProjectPathExists_UnresolvablePathsAreAbsent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX path resolution errnos")
	}
	root := t.TempDir()

	// A symlink cycle: a -> b -> a. Resolution never terminates, so ELOOP.
	a, b := filepath.Join(root, "a"), filepath.Join(root, "b")
	require.NoError(t, os.Symlink(b, a))
	require.NoError(t, os.Symlink(a, b))
	require.ErrorIs(t, statErrOf(a), syscall.ELOOP, "the fixture must actually produce ELOOP")
	assert.False(t, projectPathExists(a),
		"a symlink cycle resolves to nothing: no checkout is present there")
	assert.False(t, projectPathExists(filepath.Join(a, "checkout")),
		"and nothing below it is present either")

	// A component longer than NAME_MAX cannot name anything on this filesystem.
	tooLong := filepath.Join(root, strings.Repeat("x", 5000))
	require.ErrorIs(t, statErrOf(tooLong), syscall.ENAMETOOLONG, "the fixture must actually produce ENAMETOOLONG")
	assert.False(t, projectPathExists(tooLong),
		"a path that cannot name anything is absent, not ambiguous")
}

// statErrOf is the raw stat error, so a fixture can assert it really produced the
// errno the case is about — a test for ELOOP that silently got ENOENT would be
// asserting nothing.
func statErrOf(path string) error {
	_, err := os.Stat(path)
	return err
}

// The regression this pins: an unreadable root must not be reported as gone.
func TestProjectPathExists_UnreadableRootIsPresentNotGone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX directory permissions")
	}
	root := t.TempDir()
	locked := filepath.Join(root, "locked")
	target := filepath.Join(locked, "checkout")
	require.NoError(t, os.MkdirAll(target, 0o755))
	// Remove +x on the PARENT so stat(target) fails with EACCES while target
	// itself is very much still there.
	require.NoError(t, os.Chmod(locked, 0o000))
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })

	// Never assert a negative the environment cannot produce: root bypasses the
	// DAC check, so under a root test runner this stat SUCCEEDS and the assertion
	// below would be testing nothing.
	if _, probe := os.Stat(target); probe == nil {
		t.Skip("this runner can stat through a 0o000 directory (running as root?), so the denial is unobservable here")
	}

	assert.True(t, projectPathExists(target),
		"an unreadable root is PRESENT: a failed stat is evidence of nothing, and answering 'gone' invents a disappearance")
}

// The #2949 review's scenario: a registered checkout MOVES, and its old path is
// then replaced by a symlink cycle. ListProjects already reports that old root
// absent (determinatelyAbsent), so the registry must be able to act on that
// absence too — re-registering the same checkout at its new path has to rebind
// rather than fail with "too many levels of symbolic links".
//
// Before the fix the two are incoherent: the read says the root is gone while
// every write path that consults it errors out, so the record can be neither
// used nor repaired.
func TestRegisterProject_RebindsWhenTheOldRootBecameASymlinkCycle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX symlink cycles")
	}
	base := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", filepath.Join(base, "af-home"))

	oldRoot := initProjectRegistryRepo(t, filepath.Join(base, "checkout"))
	registered, err := RegisterProject(oldRoot)
	require.NoError(t, err)

	// Move the checkout, then make the OLD path an unresolvable cycle.
	newRoot := filepath.Join(base, "moved")
	require.NoError(t, os.Rename(oldRoot, newRoot))
	loop := filepath.Join(base, "loop-target")
	require.NoError(t, os.Symlink(loop, oldRoot))
	require.NoError(t, os.Symlink(oldRoot, loop))
	require.ErrorIs(t, statErrOf(oldRoot), syscall.ELOOP, "the fixture must actually produce ELOOP")

	// The read already calls it absent…
	assert.False(t, projectPathExists(oldRoot), "an unresolvable old root reads as absent")

	// …so the write must be able to act on that, not fail on the same stat.
	rebound, err := RegisterProject(newRoot)
	require.NoError(t, err, "re-registering the moved checkout must rebind, not fail on the dead old root")
	assert.Equal(t, registered.ID, rebound.ID, "the durable identity survives the rebind")
	assert.Equal(t, canonicalExistingPath(t, newRoot), rebound.Root, "the record now names the new root")
}
