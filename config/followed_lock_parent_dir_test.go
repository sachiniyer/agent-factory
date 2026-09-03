package config

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// retargetableHome stages the arrangement #3697 is about, and it is deliberately
// the BORING one: an AGENT_FACTORY_HOME that is itself a symlink, holding a
// perfectly ordinary regular config.toml with no link anywhere near it.
//
// That is what makes this a separate defect from #3688. resolveWriteTarget is
// asked about a path whose last component is not a link, so it hands back that
// path unchanged and there is nothing for the file-level pin to pin. Every
// path-based syscall in the operation then resolves the home link afresh —
// including the ones that create the temp file and rename it into place.
func retargetableHome(t *testing.T) (link, home, first, second string) {
	t.Helper()
	root := t.TempDir()
	first = filepath.Join(root, "A")
	second = filepath.Join(root, "B")
	require.NoError(t, os.Mkdir(first, 0o755))
	require.NoError(t, os.Mkdir(second, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(first, TomlConfigFileName), []byte("schema_version = 1\n# A\n"), 0644))
	require.NoError(t, os.WriteFile(filepath.Join(second, TomlConfigFileName), []byte("schema_version = 1\n# B\n"), 0644))
	home = filepath.Join(root, "af-home")
	require.NoError(t, os.Symlink(first, home))
	link = filepath.Join(home, TomlConfigFileName)

	// The premise, asserted rather than assumed: nothing here looks like a
	// symlink to the resolver, so the #3688 pin has no work to do.
	resolved, err := resolveWriteTarget(link)
	require.NoError(t, err)
	require.Equal(t, link, resolved,
		"premise: config.toml is a regular file, so resolveWriteTarget returns the path unchanged")
	return link, home, first, second
}

// retargetHome repoints the home link the way `stow`, `chezmoi`, a branch
// switch in a dotfiles repo, or an operator moving a mount does.
func retargetHome(t *testing.T, home, to string) {
	t.Helper()
	require.NoError(t, os.Remove(home))
	require.NoError(t, os.Symlink(to, home))
}

func entryNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func readString(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

// TestFollowedLockPinsTheDirectoryItLocked is #3697: the same lost update as
// #3688, reached through the parent directory instead of through the file.
//
// The lock resolves the path once and the write resolves it again, so a home
// link repointed in between leaves two af processes rewriting one config while
// each holds a .lock in a different directory. #3696's pin cannot see it — at
// acquisition there was no symlink to pin, and its confirm compares two
// spellings of a path that never changed.
func TestFollowedLockPinsTheDirectoryItLocked(t *testing.T) {
	t.Run("a retarget before the write does not write the unlocked directory", func(t *testing.T) {
		link, home, first, second := retargetableHome(t)

		err := withFollowedFileLock(link, func(target lockedTarget) error {
			retargetHome(t, home, second)
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'wrote/'\n"), 0644)
		})

		require.Error(t, err, "af must not write while the path it was given reaches a directory it holds no lock in")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(first), "the refusal names the directory the lock covers")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(second), "and the directory the path now reaches")

		assert.NotContains(t, readString(t, filepath.Join(second, TomlConfigFileName)), "wrote/",
			"the write must never land in the directory the home link moved to — nothing holds a lock there")
		assert.NotContains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "wrote/",
			"and a refused write leaves the locked directory alone too")
		assert.FileExists(t, filepath.Join(first, TomlConfigFileName+".lock"),
			"the lock was taken in the directory the home link named at acquisition")
		assert.NoFileExists(t, filepath.Join(second, TomlConfigFileName+".lock"),
			"and nowhere else")
	})

	t.Run("a retarget in the window confirm cannot close still lands in the locked directory", func(t *testing.T) {
		// This one is the difference between pinning the directory and merely
		// checking it. The retarget happens AFTER confirm has passed, in the
		// microseconds before the bytes move — the check-then-act window no
		// sequence of stat calls can close. A path-based stage-and-rename
		// re-resolves the home link there and drops the write into a directory
		// holding somebody else's .lock; an openat/renameat pair against a
		// directory fd cannot be redirected at all.
		link, home, first, second := retargetableHome(t)

		followedWriteRaceHookForTest = func() { retargetHome(t, home, second) }
		t.Cleanup(func() { followedWriteRaceHookForTest = nil })

		require.NoError(t, withFollowedFileLock(link, func(target lockedTarget) error {
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'pinned/'\n"), 0644)
		}))

		assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "pinned/",
			"the bytes belong in the directory the lock was taken in")
		assert.NotContains(t, readString(t, filepath.Join(second, TomlConfigFileName)), "pinned/",
			"and must not follow a link that moved after the last thing anyone could check")
	})

	t.Run("a directory replaced under the same name is a move too", func(t *testing.T) {
		// stow -D && stow, or a dotfiles checkout, does not repoint the link:
		// it removes the directory and makes a new one under the same name. The
		// path spells identically before and after, which is why the identity
		// this refuses on is dev/inode rather than a resolved string.
		link, _, first, _ := retargetableHome(t)

		err := withFollowedFileLock(link, func(target lockedTarget) error {
			require.NoError(t, os.RemoveAll(first))
			require.NoError(t, os.Mkdir(first, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(first, TomlConfigFileName), []byte("schema_version = 1\n"), 0644))
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'wrote/'\n"), 0644)
		})

		require.Error(t, err, "a lock held on an unlinked directory excludes nobody, and must not be written under")
		assert.NotContains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "wrote/",
			"the replacement directory is not the one this operation locked")
	})

	t.Run("two holders through a retargeted parent link cannot both proceed", func(t *testing.T) {
		link, home, _, second := retargetableHome(t)

		held := make(chan struct{})
		release := make(chan struct{})
		firstDone := make(chan error, 1)
		go func() {
			firstDone <- withFollowedFileLock(link, func(target lockedTarget) error {
				close(held)
				<-release
				return target.write([]byte("schema_version = 1\nbranch_prefix = 'first/'\n"), 0644)
			})
		}()
		<-held

		// The home link moves while the first holder is inside its critical
		// section. A peer af now reaches a different directory and takes THAT
		// directory's .lock — a different lock file — so it gets all the way in.
		// Nothing here can stop that; what must not happen is both of them
		// rewriting one config.
		retargetHome(t, home, second)
		require.NoError(t, withFollowedFileLock(link, func(target lockedTarget) error {
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'second/'\n"), 0644)
		}), "the peer holds the lock in the directory it reached, and is entitled to write there")

		close(release)
		// assert, not require: the lost update below is the claim, and stopping
		// here would hide it in exactly the run where it happened.
		assert.Error(t, <-firstDone,
			"the holder whose directory no longer answers to that path must refuse rather than write past a peer")

		got := readString(t, filepath.Join(second, TomlConfigFileName))
		assert.Contains(t, got, "second/", "the peer's write is the one that landed")
		assert.NotContains(t, got, "first/",
			"two processes holding two different .lock files must not both rewrite one config — "+
				"that is the lost update this pinning exists to prevent")
	})
}

// TestFollowedLockLeavesTheOrdinaryArrangementsAlone is the other half of the
// decision on #3697, and the reason the directory is pinned by an OPEN FD
// rather than by resolving its path.
//
// Resolving the path string would have moved the .lock — into the dotfiles
// directory for a linked home, and past the AF-home hardening, which
// secureAFHomeForPath applies only to a path at or inside the home. An fd does
// not: openat(dirfd, "config.toml.lock") reaches the inode the kernel was
// already reaching when it resolved the same name through the link. So nobody's
// lock file moves, and that has to stay true.
func TestFollowedLockLeavesTheOrdinaryArrangementsAlone(t *testing.T) {
	t.Run("no links at all: same file, same mode, same lock, nothing extra", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, TomlConfigFileName)
		require.NoError(t, os.WriteFile(path, []byte("schema_version = 1\n"), 0644))

		require.NoError(t, withFollowedFileLock(path, func(target lockedTarget) error {
			assert.Equal(t, path, target.file, "an ordinary path is its own target")
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'plain/'\n"), 0644)
		}))

		assert.Contains(t, readString(t, path), "plain/")
		info, err := os.Stat(path)
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0644), info.Mode().Perm())
		assert.Equal(t, []string{TomlConfigFileName, TomlConfigFileName + ".lock"}, entryNames(t, dir),
			"the lock sits beside the config exactly where it always has, and no temp file survives")
	})

	t.Run("a symlinked home keeps its lock where the kernel already put it", func(t *testing.T) {
		link, home, first, second := retargetableHome(t)

		require.NoError(t, withFollowedFileLock(link, func(target lockedTarget) error {
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'linked/'\n"), 0644)
		}))

		assert.Contains(t, readString(t, filepath.Join(first, TomlConfigFileName)), "linked/")
		assert.Equal(t, []string{TomlConfigFileName, TomlConfigFileName + ".lock"}, entryNames(t, first),
			"a linked AF home gets its lock in the directory the link resolves to — which is where it "+
				"already was, because the kernel resolved that path at open time long before #3697")
		assert.Equal(t, []string{TomlConfigFileName}, entryNames(t, second),
			"and nothing appears anywhere else")

		info, err := os.Lstat(home)
		require.NoError(t, err)
		assert.NotZero(t, info.Mode()&os.ModeSymlink, "the home link survives as a link")
	})
}
