package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	aflog "github.com/sachiniyer/agent-factory/log"
)

// This file is the per-writer half of #3660. `AtomicWriteFile` writes a temp
// file and renames it over the target, and os.Rename replaces the LINK rather
// than what it points at — so before the fix, the first write of any kind turned
// a symlinked config.toml into a regular file, left the dotfiles target holding
// the old content, and said nothing.
//
// Every writer gets its own case because the shared helper is the only thing
// holding the group together; a writer that grows its own os.WriteFile would
// pass a test written against AtomicWriteFile alone. TestEveryConfigWriterGoes
// ThroughAtomicWriteFile pins that convention separately.

// linkedConfigHome builds an AF home whose config.toml is a symlink to a file
// OUTSIDE that home — the dotfiles-repo shape the issue describes. It returns
// the link path and the real file's path.
func linkedConfigHome(t *testing.T, content string) (link, real string) {
	t.Helper()
	home := t.TempDir()
	dotfiles := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	// LoadConfig probes the user's shell for a claude alias; a plain sh takes
	// the fast `which` path instead of an interactive bash.
	t.Setenv("SHELL", "/bin/sh")

	real = filepath.Join(dotfiles, "af-config.toml")
	require.NoError(t, os.WriteFile(real, []byte(content), 0644))
	link = filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(real, link))
	return link, real
}

// pinnedTestTarget is the lockedTarget for a config that is a real file rather
// than a link — the resolution withFollowedFileLock would have pinned for it.
// Tests that drive a writer directly, below the lock, need one to hand it.
func pinnedTestTarget(path string) lockedTarget {
	return lockedTarget{link: path, file: path}
}

// assertWroteThroughLink is the shared verdict: the link survives as a link, and
// the content landed in the file it points at.
func assertWroteThroughLink(t *testing.T, link, real, wantSubstring string) {
	t.Helper()

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the write must not replace the symlink with a regular file")

	target, err := os.Readlink(link)
	require.NoError(t, err)
	assert.Equal(t, real, target, "the link must still point where the user pointed it")

	realBytes, err := os.ReadFile(real)
	require.NoError(t, err)
	assert.Contains(t, string(realBytes), wantSubstring,
		"the new content must land in the file the link points at")
}

func TestConfigWritersFollowASymlinkedConfig(t *testing.T) {
	t.Run("af config set", func(t *testing.T) {
		link, real := linkedConfigHome(t, "schema_version = 1\nbranch_prefix = 'old/'\n")

		_, err := SetGlobalConfigValue("branch_prefix", "new/")
		require.NoError(t, err)

		assertWroteThroughLink(t, link, real, "branch_prefix = 'new/'")
	})

	t.Run("af config unset", func(t *testing.T) {
		link, real := linkedConfigHome(t, "schema_version = 1\n\n[docker]\nmount_agent_credentials = true\n")

		_, err := UnsetGlobalConfigValue("docker.mount_agent_credentials")
		require.NoError(t, err)

		assertWroteThroughLink(t, link, real, "schema_version")
		realBytes, err := os.ReadFile(real)
		require.NoError(t, err)
		assert.NotContains(t, string(realBytes), "mount_agent_credentials = true")
	})

	t.Run("SaveConfig", func(t *testing.T) {
		link, real := linkedConfigHome(t, "schema_version = 1\nbranch_prefix = 'old/'\n")

		cfg, err := LoadConfig()
		require.NoError(t, err)
		cfg.BranchPrefix = "saved/"
		require.NoError(t, SaveConfig(cfg))

		assertWroteThroughLink(t, link, real, "saved/")
	})

	t.Run("af config migrate", func(t *testing.T) {
		link, real := linkedConfigHome(t, "schema_version = 1\nlisten_addr = '0.0.0.0:8443'\n")

		result, err := MigrateGlobalConfig()
		require.NoError(t, err)
		require.Len(t, result.Migrated, 1)

		assertWroteThroughLink(t, link, real, "[network]")

		// The decision: the .bak belongs beside the REAL file, since that is the
		// file being rewritten and the place a dotfiles `git status` will show it.
		//
		// Compared through ResolveForCompare because macOS spells the same
		// directory two ways: t.TempDir() hands back /var/folders/…, EvalSymlinks
		// resolves /var to /private/var, and a raw string compare fails on a
		// backup that is sitting in exactly the right place.
		assert.Equal(t,
			pathutil.ResolveForCompare(filepath.Dir(real)),
			pathutil.ResolveForCompare(filepath.Dir(result.Backup)),
			"the backup belongs beside the file that was rewritten, not beside the link")
		backup, err := os.ReadFile(result.Backup)
		require.NoError(t, err)
		assert.Contains(t, string(backup), "listen_addr = '0.0.0.0:8443'")
		assert.NotContains(t, string(backup), "[network]", "the backup is the pre-migration original")
	})
}

// TestJSONConversionRefusesADanglingConfigLink covers the one-time config.json
// to config.toml conversion.
//
// The conversion can only run when config.toml does NOT resolve, so the only
// symlink it can meet is a BROKEN one — a dotfiles link whose target was moved
// or deleted. Writing a config through a link is therefore not reachable here,
// and a test that staged one would be testing a state af cannot be in.
//
// What is reachable is the broken link, and it must not be papered over: today
// the rename replaces it with a regular file, so the user's link silently
// becomes a file and their dotfiles copy stays missing. af refuses instead and
// names both ends, which is the one thing that tells them what actually broke.
func TestJSONConversionRefusesADanglingConfigLink(t *testing.T) {
	home := t.TempDir()
	dotfiles := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")

	missing := filepath.Join(dotfiles, "af-config.toml")
	link := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(missing, link))
	require.NoError(t, os.WriteFile(filepath.Join(home, ConfigFileName),
		[]byte(`{"branch_prefix":"json/"}`), 0644))

	_, err := LoadConfig()
	require.Error(t, err)
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
		"the broken link is left for the user to repair, not replaced")
	assert.NoFileExists(t, missing)
	assert.FileExists(t, filepath.Join(home, ConfigFileName),
		"and the legacy config.json is still there to convert once the link is fixed")
}

// TestFollowingWriterRefusesADanglingLink pins the error case. Following a link
// to nowhere would either fail obscurely inside CreateTemp or silently create
// the target, and neither tells the user their link is broken.
func TestFollowingWriterRefusesADanglingLink(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "config.toml")
	missing := filepath.Join(dir, "gone", "af-config.toml")
	require.NoError(t, os.Symlink(missing, link))

	err := AtomicWriteFileFollowingLink(link, []byte("schema_version = 1\n"), 0644)
	require.Error(t, err)
	assert.Contains(t, err.Error(), link, "the error names the link")
	assert.Contains(t, err.Error(), missing, "and the target it points at")

	// Nothing was created at either end.
	assert.NoFileExists(t, missing)
	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "the broken link is left as it was")
}

// TestFollowingWriterNoticesTheLinkOncePerProcess pins the one-line notice. It
// is per process rather than per write because af rewrites its config many
// times in a session and the fact does not change between them.
func TestFollowingWriterNoticesTheLinkOncePerProcess(t *testing.T) {
	logged := captureLog(t, &aflog.InfoLog)
	resetSymlinkWriteNotices()

	dir := t.TempDir()
	target := t.TempDir()
	real := filepath.Join(target, "af-config.toml")
	require.NoError(t, os.WriteFile(real, nil, 0644))
	link := filepath.Join(dir, "config.toml")
	require.NoError(t, os.Symlink(real, link))

	for i := 0; i < 3; i++ {
		require.NoError(t, AtomicWriteFileFollowingLink(link, []byte("schema_version = 1\n"), 0644))
	}

	assert.Equal(t, 1, strings.Count(logged.String(), link),
		"three writes through one link must produce one notice, not three")
	// Resolved on both sides: on macOS the notice carries /private/var/… while
	// the fixture holds /var/…, and a substring check would pass by coincidence
	// rather than because the notice named the right file.
	assert.Contains(t, logged.String(), pathutil.ResolveForCompare(real),
		"the notice names where the write landed")
}

// TestFollowingWriterStillHardensAFHome is the ordering pin.
// ensureStorageParent -> secureAFHomeForPath only tightens the AF home when the
// write path is at or inside it (pathutil.IsAtOrInside), and only for the
// CONCRETE default home, so this test uses that home rather than a temp-dir
// AGENT_FACTORY_HOME — an arbitrary home is never hardened and could not detect
// the bug.
//
// Resolving the link BEFORE ensureStorageParent would move the path outside the
// home and silently skip the hardening, for exactly the users who symlink their
// config into a dotfiles repo. The resolution has to happen after.
func TestFollowingWriterStillHardensAFHome(t *testing.T) {
	userHome := t.TempDir()
	dotfiles := t.TempDir()
	afHome := filepath.Join(userHome, ".agent-factory")
	t.Setenv("HOME", userHome)
	t.Setenv("AGENT_FACTORY_HOME", "")
	t.Setenv("SHELL", "/bin/sh")
	require.NoError(t, os.Mkdir(afHome, 0o755))
	require.NoError(t, os.Chmod(afHome, 0o755))

	real := filepath.Join(dotfiles, "af-config.toml")
	require.NoError(t, os.WriteFile(real, nil, 0644))
	link := filepath.Join(afHome, TomlConfigFileName)
	require.NoError(t, os.Symlink(real, link))

	require.NoError(t, AtomicWriteFileFollowingLink(link, []byte("schema_version = 1\n"), 0644))

	info, err := os.Stat(afHome)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(),
		"the AF home is still hardened even though the write landed outside it")
	assertWroteThroughLink(t, link, real, "schema_version")
}

// TestFollowingWriterDoesNotWidenTheTarget pins the mode rule. Ordinary
// writers pass 0644 because that is the mode for a config af itself created; a
// dotfiles target the user keeps at 0600 is a deliberate choice about a file af
// is only REWRITING, and following the link must not quietly relax it
// (#3660 review).
func TestFollowingWriterDoesNotWidenTheTarget(t *testing.T) {
	for _, mode := range []os.FileMode{0600, 0640} {
		t.Run(mode.String(), func(t *testing.T) {
			link, real := linkedConfigHome(t, "schema_version = 1\nbranch_prefix = 'old/'\n")
			require.NoError(t, os.Chmod(real, mode))

			// The ordinary writer, passing its usual 0644.
			_, err := SetGlobalConfigValue("branch_prefix", "new/")
			require.NoError(t, err)

			info, err := os.Stat(real)
			require.NoError(t, err)
			assert.Equal(t, mode, info.Mode().Perm(),
				"the user's restrictive dotfiles mode survives an af write")
			assertWroteThroughLink(t, link, real, "new/")
		})
	}
}

// TestFollowedFileLockGuardsTheResolvedTarget pins that the followed lock and
// the followed write agree on which file they are about. Two AF homes linking to
// one dotfiles config would otherwise take two different <link>.lock files while
// rewriting a single file, which is no mutual exclusion at all (#3660 review).
func TestFollowedFileLockGuardsTheResolvedTarget(t *testing.T) {
	dotfiles := t.TempDir()
	real := filepath.Join(dotfiles, "af-config.toml")
	require.NoError(t, os.WriteFile(real, []byte("schema_version = 1\n"), 0644))

	home := t.TempDir()
	link := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(real, link))

	held := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- withFollowedFileLock(link, func(lockedTarget) error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	// Holding it through the LINK must exclude a plain lock on the REAL file:
	// that is what proves the lock landed on the file the write will touch.
	acquired, err := TryWithFileLock(real, func() error { return nil })
	require.NoError(t, err)
	assert.False(t, acquired, "the followed lock must guard the resolved target")

	close(release)
	require.NoError(t, <-done)

	assert.FileExists(t, real+".lock")
	assert.NoFileExists(t, link+".lock",
		"and must not leave a lock file beside the link")
}

// TestFollowedLockPinsTheTargetItLocked is the moving-link half of the same
// invariant (#3688).
//
// TestFollowedFileLockGuardsTheResolvedTarget above pins that the lock and the
// write agree on ONE resolution. Nothing carried that resolution into the body:
// the callback was handed the unresolved link, so its read re-resolved at open
// time and AtomicWriteFileFollowingLink resolved a third time at the rename. A
// link retargeted mid-operation — stow, chezmoi, a checkout in the dotfiles
// repo — therefore sent the write to a file nothing had locked, and left two af
// processes read-modify-writing one file while holding two different .lock
// files. The lost update was silent: `af config set` reported success, its key
// was in the file, and the peer's key was gone.
func TestFollowedLockPinsTheTargetItLocked(t *testing.T) {
	// retargetableLink stages the dotfiles shape with a second file to move the
	// link to, and returns link, the file the link starts on, and that second
	// file.
	retargetableLink := func(t *testing.T) (link, locked, moved string) {
		t.Helper()
		dotfiles := t.TempDir()
		locked = filepath.Join(dotfiles, "af-config.toml")
		moved = filepath.Join(dotfiles, "af-config.other.toml")
		require.NoError(t, os.WriteFile(locked, []byte("schema_version = 1\n# locked\n"), 0644))
		require.NoError(t, os.WriteFile(moved, []byte("schema_version = 1\n# moved\n"), 0644))
		link = filepath.Join(t.TempDir(), TomlConfigFileName)
		require.NoError(t, os.Symlink(locked, link))
		return link, locked, moved
	}
	// retarget repoints an existing link, the way `stow` or a branch switch in
	// the dotfiles repo does.
	retarget := func(t *testing.T, link, to string) {
		t.Helper()
		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(to, link))
	}

	t.Run("a retarget while queued for the lock refuses before the body runs", func(t *testing.T) {
		// The resolve happens before the wait, and the wait is unbounded: a peer
		// af can hold this lock for as long as its own operation takes, and the
		// link can move in that window. Whoever comes out of the wait must find
		// out before it reads, edits, or writes anything.
		link, locked, moved := retargetableLink(t)

		followedLockRaceHookForTest = func() { retarget(t, link, moved) }
		t.Cleanup(func() { followedLockRaceHookForTest = nil })

		ran := false
		err := withFollowedFileLock(link, func(lockedTarget) error {
			ran = true
			return nil
		})

		require.Error(t, err)
		assert.False(t, ran, "the body must not run against a file the link no longer names")
		assert.Contains(t, err.Error(), link, "the refusal names the link")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(locked), "the file the lock covers")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(moved), "and the file the link now points at")
	})

	t.Run("a retarget between acquisition and the write cannot redirect the write", func(t *testing.T) {
		link, locked, moved := retargetableLink(t)

		err := withFollowedFileLock(link, func(target lockedTarget) error {
			assert.Equal(t, pathutil.ResolveForCompare(locked), pathutil.ResolveForCompare(target.file),
				"the body must be handed the file the lock covers, not the link")
			retarget(t, link, moved)
			return target.write([]byte("schema_version = 1\n# af wrote this\n"), 0644)
		})

		require.Error(t, err, "af must not write while the link names a file it holds no lock on")
		assert.Contains(t, err.Error(), link, "the refusal names the link")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(locked), "the file the lock covers")
		assert.Contains(t, err.Error(), pathutil.ResolveForCompare(moved), "and the file the link now points at")

		after, readErr := os.ReadFile(moved)
		require.NoError(t, readErr)
		assert.NotContains(t, string(after), "af wrote this",
			"the write must never land on the file the link moved to — nothing holds a lock on it")
		before, readErr := os.ReadFile(locked)
		require.NoError(t, readErr)
		assert.NotContains(t, string(before), "af wrote this",
			"and a refused write leaves the locked file alone too")
	})

	t.Run("a retargeted link cannot leave two holders rewriting one file", func(t *testing.T) {
		link, _, moved := retargetableLink(t)

		held := make(chan struct{})
		release := make(chan struct{})
		first := make(chan error, 1)
		go func() {
			first <- withFollowedFileLock(link, func(target lockedTarget) error {
				close(held)
				<-release
				return target.write([]byte("schema_version = 1\nbranch_prefix = 'first/'\n"), 0644)
			})
		}()
		<-held

		// The link moves while the first holder is inside its critical section.
		// A peer af now resolves to the new target and takes THAT file's .lock —
		// a different lock file — so it gets all the way in. Nothing here can
		// stop that; what must not happen is both of them rewriting one file.
		retarget(t, link, moved)
		require.NoError(t, withFollowedFileLock(link, func(target lockedTarget) error {
			return target.write([]byte("schema_version = 1\nbranch_prefix = 'second/'\n"), 0644)
		}), "the peer holds the lock on the file it resolved, and is entitled to write it")

		close(release)
		firstErr := <-first
		require.Error(t, firstErr,
			"the holder whose lock no longer covers the link must refuse rather than write past a peer")
		assert.Contains(t, firstErr.Error(), link)
		assert.Contains(t, firstErr.Error(), pathutil.ResolveForCompare(moved))

		got, readErr := os.ReadFile(moved)
		require.NoError(t, readErr)
		assert.Contains(t, string(got), "second/", "the peer's write is the one that landed")
		assert.NotContains(t, string(got), "first/",
			"two processes holding two different .lock files must not both rewrite one file — "+
				"that is the lost update this pinning exists to prevent")
	})
}

// TestFollowedLockGuardsTheOutcomesThatNeverWrite covers the paths #3696's
// review found on the way past the writers: a body can report an outcome
// without ever reaching the guarded write, and a report about the wrong file is
// the same silent wrong answer as a lost update, just in a different shape.
func TestFollowedLockGuardsTheOutcomesThatNeverWrite(t *testing.T) {
	// movedLink stages a link that has ALREADY been retargeted away from the
	// file the lock pinned — the state a body finds itself in when stow or a
	// dotfiles checkout runs mid-operation.
	//
	// The pin is taken the way withFollowedFileLock takes it — resolveWriteTarget
	// through the link, WHILE it still points at the locked file — and only then
	// is the link moved. Handing the fixture a raw path instead builds a handle
	// production cannot produce: on macOS t.TempDir() is under /var, which
	// EvalSymlinks resolves to /private/var, so `file` would have been a
	// spelling the real code never stores. It returns the pinned strings so the
	// assertions match the error exactly rather than a re-resolution of them.
	movedLink := func(t *testing.T, lockedContent string) (target lockedTarget, pinned, movedTo string) {
		t.Helper()
		// Parsing a config probes the user's shell for a claude alias; a plain
		// sh takes the fast `which` path instead of an interactive bash.
		t.Setenv("SHELL", "/bin/sh")
		dotfiles := t.TempDir()
		locked := filepath.Join(dotfiles, "af-config.toml")
		moved := filepath.Join(dotfiles, "af-config.other.toml")
		require.NoError(t, os.WriteFile(locked, []byte(lockedContent), 0644))
		require.NoError(t, os.WriteFile(moved, []byte("schema_version = 1\n"), 0644))
		link := filepath.Join(t.TempDir(), TomlConfigFileName)
		require.NoError(t, os.Symlink(locked, link))

		pinned, err := resolveWriteTarget(link)
		require.NoError(t, err)

		require.NoError(t, os.Remove(link))
		require.NoError(t, os.Symlink(moved, link))
		movedTo, err = resolveWriteTarget(link)
		require.NoError(t, err)

		return lockedTarget{link: link, file: pinned}, pinned, movedTo
	}

	t.Run("unset refuses instead of reporting nothing to remove", func(t *testing.T) {
		// The locked file does not carry the key, so the delete is a no-op and
		// the write — the only thing that confirmed — never runs. Reporting
		// "not set" here describes a file the link no longer names.
		target, pinned, movedTo := movedLink(t, "schema_version = 1\n")
		alias, ok := configAliasForCanonical("network.listen_addr")
		require.True(t, ok, "premise: the key under test is a migrated alias")

		result, err := applyGlobalUnset(target, prettyHomePath(target.link), "network.listen_addr", alias)

		require.Error(t, err, "a no-op report must not be made about a file the link stopped naming")
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), pinned, "the refusal names the file the lock covers")
		assert.Contains(t, err.Error(), movedTo, "and the file the link now points at")
	})

	t.Run("migrate refuses instead of reporting nothing to migrate", func(t *testing.T) {
		target, pinned, movedTo := movedLink(t, "schema_version = 1\nbranch_prefix = 'me/'\n")

		result, err := migrateConfigFile(target)

		require.Error(t, err, "a nothing-to-migrate report must not be made about a file the link stopped naming")
		assert.Nil(t, result)
		assert.Contains(t, err.Error(), pinned, "the refusal names the file the lock covers")
		assert.Contains(t, err.Error(), movedTo, "and the file the link now points at")
	})

	t.Run("root-agent deregistration refuses instead of reporting nothing removed", func(t *testing.T) {
		// The costliest of the three. DeleteProject reads a nil error here as
		// "the durable cleanup succeeded" and deletes the project, so an opt-in
		// surviving in the config the link now names respawns that project on
		// the next daemon start — which is precisely what its own error message
		// promises cannot happen.
		target, pinned, movedTo := movedLink(t, "schema_version = 1\n")

		removed, err := deregisterRootAgentsLocked(target, []string{RepoIDFromRoot("/repos/gone")})

		require.Error(t, err, "a nothing-removed answer must not be made about a file the link stopped naming")
		assert.Nil(t, removed)
		assert.Contains(t, err.Error(), pinned, "the refusal names the file the lock covers")
		assert.Contains(t, err.Error(), movedTo, "and the file the link now points at")
	})

	t.Run("a refused migration leaves no backup behind", func(t *testing.T) {
		// Here the link still names the locked file when the migration starts,
		// so the backup gets written; the retarget lands in the window between
		// that and the guarded write. The error says nothing was written, and a
		// .bak left on disk would make that false — and would consume a slot
		// availableBackupPath keeps for the ORIGINAL pre-migration copy.
		t.Setenv("SHELL", "/bin/sh")
		dotfiles := t.TempDir()
		locked := filepath.Join(dotfiles, "af-config.toml")
		moved := filepath.Join(dotfiles, "af-config.other.toml")
		require.NoError(t, os.WriteFile(locked, []byte("schema_version = 1\nlisten_addr = '0.0.0.0:8443'\n"), 0644))
		require.NoError(t, os.WriteFile(moved, []byte("schema_version = 1\n"), 0644))
		link := filepath.Join(t.TempDir(), TomlConfigFileName)
		require.NoError(t, os.Symlink(locked, link))

		// Pinned the way withFollowedFileLock pins, so the entry confirm passes
		// and the retarget lands in the window this test is about.
		pinned, err := resolveWriteTarget(link)
		require.NoError(t, err)

		var movedTo string
		migrateWriteRaceHookForTest = func() {
			require.NoError(t, os.Remove(link))
			require.NoError(t, os.Symlink(moved, link))
			resolved, hookErr := resolveWriteTarget(link)
			require.NoError(t, hookErr)
			movedTo = resolved
		}
		t.Cleanup(func() { migrateWriteRaceHookForTest = nil })

		before, err := os.ReadFile(locked)
		require.NoError(t, err)

		_, err = migrateConfigFile(lockedTarget{link: link, file: pinned})

		require.Error(t, err, "the guarded write must refuse once the link has moved")
		require.NotEmpty(t, movedTo, "premise: the hook must have run, or the refusal came from the wrong place")
		assert.Contains(t, err.Error(), pinned, "the refusal names the file the lock covers")
		assert.Contains(t, err.Error(), movedTo, "and the file the link now points at")

		entries, rerr := os.ReadDir(dotfiles)
		require.NoError(t, rerr)
		for _, entry := range entries {
			assert.NotContains(t, entry.Name(), ".bak",
				"a refused migration says nothing was written, so it must leave no backup behind")
		}
		after, rerr := os.ReadFile(locked)
		require.NoError(t, rerr)
		assert.Equal(t, string(before), string(after), "and the locked file is untouched")
	})
}

// TestAtomicWriteFileDoesNotFollowLinksByDefault is the counterpart guarantee,
// and the reason following is a separate function rather than a flag.
//
// Replacing the link is what os.Rename does on its own, and it stays the
// behaviour of the plain writer for every caller that took neither of the other
// two answers — config's own state, TUI state, the project registry, the daemon
// PID file (#3672 decided the af-managed group; anything it did not name keeps
// this).
//
// This is the behaviour that a follow-by-default AtomicWriteFile broke, which is
// why it is pinned rather than assumed. The callers that must NOT reach it —
// af's own managed files — now use AtomicWriteFileRefusingLink and are pinned in
// atomicwrite_refuse_symlink_test.go and beside each caller.
func TestAtomicWriteFileDoesNotFollowLinksByDefault(t *testing.T) {
	dir := t.TempDir()
	elsewhere := t.TempDir()
	target := filepath.Join(elsewhere, "unit-target")
	require.NoError(t, os.WriteFile(target, []byte("original\n"), 0644))

	link := filepath.Join(dir, "af-daemon.service")
	require.NoError(t, os.Symlink(target, link))

	require.NoError(t, AtomicWriteFile(link, []byte("replaced\n"), 0644))

	info, err := os.Lstat(link)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0), info.Mode()&os.ModeSymlink,
		"the plain writer replaces the link with a regular file, exactly as before")

	wrote, err := os.ReadFile(link)
	require.NoError(t, err)
	assert.Equal(t, "replaced\n", string(wrote))

	untouched, err := os.ReadFile(target)
	require.NoError(t, err)
	assert.Equal(t, "original\n", string(untouched),
		"and leaves what the link pointed at alone")
}

// TestStartupDoesNotDestroyOrIgnoreASymlinkedConfig covers the two load paths
// that reached past the write-side guarantee. Both are reachable by doing
// nothing but starting af, which is what made them worth fixing here rather
// than filing: this PR documents that a symlinked global config is followed,
// not replaced, and these two made that false (#3660 review).
func TestStartupDoesNotDestroyOrIgnoreASymlinkedConfig(t *testing.T) {
	t.Run("a contentless target keeps the link", func(t *testing.T) {
		// The contentless recovery exists to clean up a failed first-run write
		// (#864). A symlink is never that — first run creates a regular file —
		// so removing it would unlink the operator's dotfiles arrangement for
		// nothing. Before the fix, merely calling LoadConfig did exactly that.
		home, dotfiles := t.TempDir(), t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		t.Setenv("SHELL", "/bin/sh")
		real := filepath.Join(dotfiles, "af-config.toml")
		require.NoError(t, os.WriteFile(real, []byte("\n"), 0644))
		link := filepath.Join(home, TomlConfigFileName)
		require.NoError(t, os.Symlink(real, link))

		_, err := LoadConfig()
		require.Error(t, err, "a contentless config is a loud error, not a silent re-materialize")
		assert.Contains(t, err.Error(), "empty")

		info, lerr := os.Lstat(link)
		require.NoError(t, lerr)
		assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink,
			"starting af must not unlink the operator's dotfiles arrangement")
		assert.FileExists(t, real)
	})

	t.Run("a dangling link is refused, not silently defaulted", func(t *testing.T) {
		// os.ReadFile on a dangling link returns ENOENT, which read as "no
		// config yet": materializeDefaultConfig's exclusive create then failed
		// EEXIST on the link, and the failed reread fell through to an in-memory
		// DefaultConfig with NO error. af started on defaults — a different
		// default_program and a different listener — while the operator believed
		// it was reading their config.
		home, dotfiles := t.TempDir(), t.TempDir()
		t.Setenv("AGENT_FACTORY_HOME", home)
		t.Setenv("SHELL", "/bin/sh")
		missing := filepath.Join(dotfiles, "gone.toml")
		link := filepath.Join(home, TomlConfigFileName)
		require.NoError(t, os.Symlink(missing, link))

		_, err := LoadConfig()
		require.Error(t, err, "a broken link must not read as 'no config yet'")
		assert.Contains(t, err.Error(), link, "the error names the link")
		assert.Contains(t, err.Error(), missing, "and the target it points at")
	})

	t.Run("the followed lock refuses before the callback runs", func(t *testing.T) {
		// Callbacks read the file before reaching the writer, so falling back to
		// the unresolved path turned the both-ends error into a bare ENOENT
		// naming only config.toml.
		dir, dotfiles := t.TempDir(), t.TempDir()
		missing := filepath.Join(dotfiles, "gone.toml")
		link := filepath.Join(dir, TomlConfigFileName)
		require.NoError(t, os.Symlink(missing, link))

		ran := false
		err := withFollowedFileLock(link, func(lockedTarget) error {
			ran = true
			return nil
		})
		require.Error(t, err)
		assert.False(t, ran, "the callback must not run under a lock on a path that will not resolve")
		assert.Contains(t, err.Error(), link)
		assert.Contains(t, err.Error(), missing)
	})
}

// TestReadOnlyLoadRefusesADanglingLink pins that the diagnostics agree with
// startup. LoadConfigReadOnly backs `af config validate` and `af doctor`; with a
// dangling link it reported Missing:true, so validate exited 0 and doctor
// advised starting af to write defaults — while af itself refuses to start on
// that same link. A diagnostic that contradicts the thing it diagnoses sends the
// reader looking in the wrong place (#3660 review).
func TestReadOnlyLoadRefusesADanglingLink(t *testing.T) {
	home, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	missing := filepath.Join(dotfiles, "gone.toml")
	link := filepath.Join(home, TomlConfigFileName)
	require.NoError(t, os.Symlink(missing, link))

	loaded, err := LoadConfigReadOnly()
	require.Error(t, err, "a broken link must not read as 'no config file yet'")
	assert.False(t, loaded.Missing, "and must not be reported as missing")
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)

	// It stays read-only: nothing is created at either end.
	assert.NoFileExists(t, missing)
	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink)

	// And it agrees with startup, which is the whole point.
	_, startupErr := LoadConfig()
	require.Error(t, startupErr)
}

// TestMaterializeRefusesADanglingLinkInstalledDuringTheRace covers the window
// the earlier check cannot: check-then-act is not atomic, and
// materializeRaceHookForTest exists because another process really can install
// a config.toml between the check and the exclusive create. When that arrival
// is a broken link, the create reports EEXIST on it, the reread fails, and the
// old code returned in-memory defaults with no error — the same silent-defaults
// outcome refuseDanglingConfigLink was added to stop (#3660 review).
func TestMaterializeRefusesADanglingLinkInstalledDuringTheRace(t *testing.T) {
	home, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	missing := filepath.Join(dotfiles, "gone.toml")
	link := filepath.Join(home, TomlConfigFileName)

	// The racing process installs the broken link after the pre-read check.
	prev := materializeRaceHookForTest
	materializeRaceHookForTest = func() {
		_ = os.Symlink(missing, link)
	}
	t.Cleanup(func() { materializeRaceHookForTest = prev })

	_, err := LoadConfig()
	require.Error(t, err, "losing the create race to a broken link must not yield silent defaults")
	assert.Contains(t, err.Error(), link)
	assert.Contains(t, err.Error(), missing)
}

// TestBackupNameSkipsADanglingLink pins that a symlink occupies the name even
// when it dangles. availableBackupPath promises never to overwrite an existing
// backup, and fileExists follows links — so a dangling <config>.bak link
// reported ENOENT and the backup was written straight over it, destroying an
// entry the user made (#3660 review).
func TestBackupNameSkipsADanglingLink(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.toml.bak")
	require.NoError(t, os.Symlink(filepath.Join(dir, "nowhere"), base))

	got, err := availableBackupPath(base)
	require.NoError(t, err)
	assert.Equal(t, base+".1", got, "a dangling link holds the name")

	info, lerr := os.Lstat(base)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "and is left intact")
}

// TestMaterializeRefusesAContentlessLinkInstalledDuringTheRace is the sibling
// of the dangling-link race. A link installed mid-materialize whose target is
// contentless makes the exclusive create report EEXIST and the reread come back
// effectively empty — which fell through to in-memory defaults, while the SAME
// arrangement present before startup gives a loud error. One symlink cannot
// mean two different things depending on when it appeared (#3660 review).
func TestMaterializeRefusesAContentlessLinkInstalledDuringTheRace(t *testing.T) {
	home, dotfiles := t.TempDir(), t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("SHELL", "/bin/sh")
	real := filepath.Join(dotfiles, "af-config.toml")
	link := filepath.Join(home, TomlConfigFileName)

	prev := materializeRaceHookForTest
	materializeRaceHookForTest = func() {
		_ = os.WriteFile(real, []byte("# only a comment\n"), 0644)
		_ = os.Symlink(real, link)
	}
	t.Cleanup(func() { materializeRaceHookForTest = prev })

	_, err := LoadConfig()
	require.Error(t, err, "a contentless target must not become silent defaults just because it arrived late")
	assert.Contains(t, err.Error(), "empty")

	info, lerr := os.Lstat(link)
	require.NoError(t, lerr)
	assert.Equal(t, os.ModeSymlink, info.Mode()&os.ModeSymlink, "and the link is left alone")
}
