package config

import (
	"os"
	"path/filepath"
	"runtime"
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
