package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
		assert.Equal(t, filepath.Dir(real), filepath.Dir(result.Backup),
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
	assert.Contains(t, logged.String(), real, "the notice names where the write landed")
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
		done <- WithFollowedFileLock(link, func() error {
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

// TestAtomicWriteFileDoesNotFollowLinksByDefault is the counterpart guarantee,
// and the reason following is a separate function rather than a flag.
//
// af's own managed files — the bearer token, autostart units, the task store —
// go through the plain writer, and #3672's caller table depends on their
// semantics being unchanged. The concrete case there: daemon/autostart.go writes
// a unit with AtomicWriteFile and cleans up with os.Remove of the SAME path, so
// a writer that silently followed would leave the cleanup unlinking a link whose
// content had gone somewhere else.
//
// This is the behaviour that a follow-by-default AtomicWriteFile broke, which is
// why it is pinned rather than assumed.
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
