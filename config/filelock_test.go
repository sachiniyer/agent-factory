package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWithFileLock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	called := false
	err := WithFileLock(path, func() error {
		called = true
		return nil
	})
	assert.NoError(t, err)
	assert.True(t, called)

	// Lock file should exist
	_, err = os.Stat(path + ".lock")
	assert.NoError(t, err)
}

func TestWithFileLockPropagatesError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	err := WithFileLock(path, func() error {
		return assert.AnError
	})
	assert.ErrorIs(t, err, assert.AnError)
}

func TestAtomicWriteFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "test.json")

	data := []byte(`{"key": "value"}`)
	err := AtomicWriteFile(path, data, 0644)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

func TestAtomicWriteFileCreatesDir(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "dir", "test.json")

	data := []byte(`[]`)
	err := AtomicWriteFile(path, data, 0644)
	require.NoError(t, err)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)
}

// TestAtomicWriteFileSucceedsEvenIfDirSyncWouldFail locks in the post-rename
// contract for #608: once os.Rename has placed the data on disk, AtomicWriteFile
// must return nil. The parent-directory fsync is best-effort for crash
// durability and must not turn into a returned error -- callers (api/tasks.go,
// daemon/control.go) treat any error as "data was not persisted" and roll back.
//
// The function as-written makes injecting a real dir-sync failure require
// either a func var seam or an OS-specific hack; both were considered worse
// than a behavior-level assertion of the contract. So this test exercises the
// happy path and asserts (a) nil return, (b) file present, (c) bytes match,
// (d) perm bits as requested.
func TestAtomicWriteFileSucceedsEvenIfDirSyncWouldFail(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "persisted.json")

	data := []byte(`{"persisted": true}`)
	err := AtomicWriteFile(path, data, 0640)
	require.NoError(t, err, "post-rename failures must not surface as an error")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, data, got)

	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0640), info.Mode().Perm())
}

// TestAtomicWriteFileReturnsErrorOnPreRenameFailure proves that errors before
// os.Rename succeeds (the failure modes where data is NOT yet on disk) still
// surface as a returned error. We make the parent directory read-only so that
// CreateTemp inside AtomicWriteFile fails, ensuring no rename can happen and
// any pre-existing file at `path` is left untouched.
func TestAtomicWriteFileReturnsErrorOnPreRenameFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: chmod 0500 does not prevent writes")
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "existing.json")

	original := []byte(`{"original": "untouched"}`)
	require.NoError(t, os.WriteFile(path, original, 0644))

	// Make the directory read+execute only so CreateTemp fails.
	require.NoError(t, os.Chmod(dir, 0500))
	t.Cleanup(func() {
		// Restore perms so t.TempDir cleanup can remove the directory.
		_ = os.Chmod(dir, 0700)
	})

	err := AtomicWriteFile(path, []byte(`{"new": "value"}`), 0644)
	require.Error(t, err, "pre-rename failures must surface as an error so callers can roll back")

	// Re-grant read access for the assertion read; we already failed the write.
	require.NoError(t, os.Chmod(dir, 0700))
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, original, got, "original file must be unchanged when AtomicWriteFile returns error")
}
