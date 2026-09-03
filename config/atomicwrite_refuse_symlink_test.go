package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The third answer in the tree (#3672): af's own managed files refuse a
// symlinked target rather than replacing it or writing through it.
//
// The two existing answers are pinned next door — AtomicWriteFileFollowingLink
// rewrites the target and keeps the link (#3660), and the in-repo writer follows
// one only as far as the repository goes (#1092). What is proved here is that
// the refusing writer changes NOTHING on disk and says which two paths are
// involved, because the whole value of failing closed is that the person who
// made the link gets to decide what happens to it.

func TestAtomicWriteFileRefusingLinkLeavesBothEndsAlone(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "token-target")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0600))

	link := filepath.Join(dir, "daemon-token")
	require.NoError(t, os.Symlink(target, link))

	err := AtomicWriteFileRefusingLink(link, []byte("replaced\n"), 0600)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), target, "and the target it points at")

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the link the user made is still a link")

	untouched, rerr := os.ReadFile(target)
	require.NoError(t, rerr)
	assert.Equal(t, "original\n", string(untouched),
		"and nothing was written through it either")
}

// A relative link is the common spelling inside a dotfiles arrangement, and the
// raw text ("../elsewhere/x") names nothing from the reader's shell. The error
// has to resolve it against the LINK's directory, the way the kernel does.
func TestAtomicWriteFileRefusingLinkNamesARelativeTargetAbsolutely(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(root, "home"), 0755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "dotfiles"), 0755))
	target := filepath.Join(root, "dotfiles", "unit")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0644))

	link := filepath.Join(root, "home", "af-daemon.service")
	require.NoError(t, os.Symlink(filepath.Join("..", "dotfiles", "unit"), link))

	err := AtomicWriteFileRefusingLink(link, []byte("replaced\n"), 0644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), target,
		"a relative link must be reported as the path it actually resolves to")
}

// A DANGLING link is refused by the same rule, not as a special case: the policy
// is about the link being there at all. Following one would materialize a file
// at a path the user believes lives somewhere else, and replacing one would
// silently discard the arrangement they were part-way through setting up.
func TestAtomicWriteFileRefusingLinkRefusesADanglingLink(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	missing := filepath.Join(elsewhere, "gone")
	link := filepath.Join(dir, "daemon-token")
	require.NoError(t, os.Symlink(missing, link))

	err := AtomicWriteFileRefusingLink(link, []byte("replaced\n"), 0600)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)

	assert.NoFileExists(t, missing, "and nothing was created at the far end")
	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)
}

// The refusal must leave the filesystem exactly as it found it, which is why the
// check runs BEFORE the parent directory is created. A refused write that had
// already made directories would be a side effect nobody asked for on a path af
// just declined to touch.
func TestAtomicWriteFileRefusingLinkCreatesNothingOnRefusal(t *testing.T) {
	root, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "cache.json")
	require.NoError(t, os.WriteFile(target, []byte("{}\n"), 0644))

	nested := filepath.Join(root, "a", "b")
	require.NoError(t, os.MkdirAll(nested, 0755))
	link := filepath.Join(nested, "update-check.json")
	require.NoError(t, os.Symlink(target, link))

	require.Error(t, AtomicWriteFileRefusingLink(link, []byte("new\n"), 0644))

	entries, err := os.ReadDir(nested)
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file was left behind")
	assert.Equal(t, "update-check.json", entries[0].Name())
}

// The ordinary paths must stay ordinary: a regular file is rewritten and a
// missing one is created, both exactly as AtomicWriteFile does. A guard that
// only ever refuses is easy to write and useless.
func TestAtomicWriteFileRefusingLinkWritesOrdinaryPaths(t *testing.T) {
	dir := t.TempDir()

	fresh := filepath.Join(dir, "created", "daemon-token")
	require.NoError(t, AtomicWriteFileRefusingLink(fresh, []byte("first\n"), 0600))
	data, err := os.ReadFile(fresh)
	require.NoError(t, err)
	assert.Equal(t, "first\n", string(data))
	info, err := os.Stat(fresh)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0600), info.Mode().Perm(),
		"and applies perm exactly, as the token file depends on")

	require.NoError(t, AtomicWriteFileRefusingLink(fresh, []byte("second\n"), 0600))
	data, err = os.ReadFile(fresh)
	require.NoError(t, err)
	assert.Equal(t, "second\n", string(data))
}

// RemoveFileRefusingLink is the other half of #3672's title. A writer that
// refuses a link paired with a cleanup that unlinks one is the asymmetry the
// issue was filed about, just pointed the other way.
func TestRemoveFileRefusingLinkRefusesToUnlinkALink(t *testing.T) {
	dir, elsewhere := t.TempDir(), t.TempDir()
	target := filepath.Join(elsewhere, "unit-target")
	require.NoError(t, os.WriteFile(target, []byte("user content\n"), 0644))

	link := filepath.Join(dir, "af-daemon.service")
	require.NoError(t, os.Symlink(target, link))

	err := RemoveFileRefusingLink(link)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrManagedFileSymlink)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), target)

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the link survives a refused cleanup")
	assert.FileExists(t, target)
}

// Callers keep their os.IsNotExist checks, so an absent path must still produce
// the ordinary ENOENT rather than a refusal or a nil.
func TestRemoveFileRefusingLinkRemovesRegularFilesAndReportsENOENT(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "af-daemon.service")
	require.NoError(t, os.WriteFile(real, []byte("unit\n"), 0644))

	require.NoError(t, RemoveFileRefusingLink(real))
	assert.NoFileExists(t, real)

	err := RemoveFileRefusingLink(real)
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err), "a second remove is the caller's usual ENOENT, got %v", err)
	assert.False(t, errors.Is(err, ErrManagedFileSymlink))
}
