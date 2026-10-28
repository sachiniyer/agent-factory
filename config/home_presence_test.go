package config

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// #3845. A daemon whose AF home is deleted out from under it used to re-create
// the directory on its very next state write: every atomic write and every file
// lock goes through ensureStorageParent, whose os.MkdirAll creates the home as a
// parent of whatever it was asked to create. watchDaemonHome (#1093/#1094) then
// stats a home that is present again and clears its consecutive-miss counter, so
// the daemon never observes its own deletion and keeps firing schedules forever.
//
// #3843 closed the same door at the socket binds. These pin the write side.

// armedHome returns an AF home this process has observed present, with the latch
// released when the test ends — the daemon-startup arming, in one line.
func armedHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	forget, err := MarkAFHomePresent(home)
	require.NoError(t, err)
	t.Cleanup(forget)
	return home
}

func TestAtomicWriteRefusesToResurrectAnObservedHomeThatWasDeleted(t *testing.T) {
	home := armedHome(t)
	require.NoError(t, os.RemoveAll(home))

	err := AtomicWriteFile(filepath.Join(home, "instances", "abc", InstancesFileName), []byte("[]"), 0644)

	require.Error(t, err, "a write into a home this process saw deleted must be refused")
	assert.True(t, errors.Is(err, ErrAFHomeRemoved), "want ErrAFHomeRemoved, got %v", err)
	assert.Contains(t, err.Error(), home, "the refusal must name the home")
	assert.NoDirExists(t, home, "the refused write must not re-create the home")
}

func TestFileLockRefusesToResurrectAnObservedHomeThatWasDeleted(t *testing.T) {
	home := armedHome(t)
	require.NoError(t, os.RemoveAll(home))

	ran := false
	err := WithFileLock(filepath.Join(home, "tasks.json"), func() error {
		ran = true
		return nil
	})

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAFHomeRemoved), "want ErrAFHomeRemoved, got %v", err)
	assert.False(t, ran, "the locked section must not run — its .lock file is what re-created the home")
	assert.NoDirExists(t, home, "taking the lock must not re-create the home")
}

// The daemon's own steady-state write, in the shape probe measurement caught it:
// a live session's status poll saves instances/<repoID>/instances.json every
// tick, and it was that MkdirAll that brought the home back 0.5s after `rm -rf`.
func TestMkdirAllUnderAFHomeRefusesToResurrectADeletedHome(t *testing.T) {
	home := armedHome(t)
	require.NoError(t, os.RemoveAll(home))

	err := MkdirAllUnderAFHome(filepath.Join(home, "events"), 0755)

	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrAFHomeRemoved), "want ErrAFHomeRemoved, got %v", err)
	assert.NoDirExists(t, home)
}

// Subdirectories under a PRESENT home keep MkdirAll semantics: the refusal is
// about resurrecting the home, not about creating storage inside a live one.
func TestWritesUnderAPresentHomeStillCreateTheirSubdirectories(t *testing.T) {
	home := armedHome(t)

	nested := filepath.Join(home, "instances", "abc", InstancesFileName)
	require.NoError(t, AtomicWriteFile(nested, []byte("[]"), 0644))
	assert.FileExists(t, nested)

	require.NoError(t, MkdirAllUnderAFHome(filepath.Join(home, "events"), 0755))
	assert.DirExists(t, filepath.Join(home, "events"))

	require.NoError(t, WithFileLock(filepath.Join(home, "logs", "x"), func() error { return nil }))
	assert.DirExists(t, filepath.Join(home, "logs"))
}

// The CLI case the guard must not break: `af config set` on a fresh machine
// legitimately creates the home on its first write. No process has observed the
// home present, so nothing is being resurrected — there is no latch, and the
// write behaves exactly as it always has.
func TestAnUnobservedHomeIsStillCreatedOnFirstWrite(t *testing.T) {
	home := filepath.Join(t.TempDir(), "fresh-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	require.NoError(t, AtomicWriteFile(filepath.Join(home, TomlConfigFileName), []byte("schema_version = 1\n"), 0644))

	assert.DirExists(t, home, "a first write on a fresh machine must still create the home")
}

// The latch names ONE home. A process that observed home A must not refuse a
// write into an unrelated home B — which is what keeps a test binary (and `af
// reset`, which resolves another daemon's home) from inheriting a foreign latch.
func TestARefusalIsScopedToTheHomeThisProcessObserved(t *testing.T) {
	observed := armedHome(t)
	require.NoError(t, os.RemoveAll(observed))

	other := filepath.Join(t.TempDir(), "other-home")
	require.NoError(t, AtomicWriteFile(filepath.Join(other, "config.toml"), []byte("x"), 0644))
	assert.DirExists(t, other)
}

// A write OUTSIDE the home is not the daemon's own state — the upgrade binary
// swap, the autostart unit — and a deleted home says nothing about it.
func TestAWriteOutsideTheObservedHomeIsNeverRefused(t *testing.T) {
	home := armedHome(t)
	require.NoError(t, os.RemoveAll(home))

	outside := filepath.Join(t.TempDir(), "elsewhere", "file")
	require.NoError(t, AtomicWriteFile(outside, []byte("x"), 0644))
	assert.FileExists(t, outside)
}

// Only a POSITIVE observation of absence refuses, mirroring applyHomeCheck and
// requireDaemonHomePresent: an inconclusive stat (EACCES here) leaves the home's
// fate unknown, and refusing on "I could not tell" would take a healthy daemon's
// writes down on the strength of a transient error. The write still fails — the
// kernel says EACCES — but it fails as the I/O error it is, not as a refusal.
func TestAnInconclusiveStatDoesNotRefuse(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions, so no EACCES can be induced")
	}
	parent := t.TempDir()
	home := filepath.Join(parent, "home")
	require.NoError(t, os.MkdirAll(home, 0700))
	t.Setenv("AGENT_FACTORY_HOME", home)
	forget, err := MarkAFHomePresent(home)
	require.NoError(t, err)
	t.Cleanup(forget)

	require.NoError(t, os.Chmod(parent, 0000))
	t.Cleanup(func() { _ = os.Chmod(parent, 0700) })

	err = AtomicWriteFile(filepath.Join(home, "state.json"), []byte("{}"), 0644)

	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrAFHomeRemoved),
		"an unreadable parent is not evidence the home was deleted; got %v", err)
}

// MarkAFHomePresent only latches what it can SEE. A home that is already gone
// was never observed present, so arming against it must not arm anything —
// otherwise a daemon that somehow reached startup with no home would refuse
// every write for the rest of its life instead of behaving as it does today.
func TestMarkingAnAbsentHomeArmsNothing(t *testing.T) {
	home := filepath.Join(t.TempDir(), "never-existed")
	t.Setenv("AGENT_FACTORY_HOME", home)

	forget, err := MarkAFHomePresent(home)
	require.Error(t, err)
	require.NotNil(t, forget, "forget must be safe to defer even when arming failed")
	t.Cleanup(forget)

	require.NoError(t, AtomicWriteFile(filepath.Join(home, "config.toml"), []byte("x"), 0644))
	assert.DirExists(t, home)
}

// forget releases the latch, so an in-process daemon (the daemon package's own
// tests run RunDaemon in-process) leaves no refusal behind for the tests after
// it.
func TestForgetReleasesTheLatch(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	forget, err := MarkAFHomePresent(home)
	require.NoError(t, err)
	require.NoError(t, os.RemoveAll(home))

	require.ErrorIs(t, AtomicWriteFile(filepath.Join(home, "a"), []byte("x"), 0644), ErrAFHomeRemoved)
	forget()
	require.NoError(t, AtomicWriteFile(filepath.Join(home, "a"), []byte("x"), 0644))
	assert.DirExists(t, home)
}

// Nested arming: a second daemon in the same process takes the latch, and the
// FIRST one's forget must not clear it out from under the one still running.
func TestForgetOnlyClearsTheLatchItArmed(t *testing.T) {
	first := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", first)
	forgetFirst, err := MarkAFHomePresent(first)
	require.NoError(t, err)

	second := t.TempDir()
	forgetSecond, err := MarkAFHomePresent(second)
	require.NoError(t, err)
	t.Cleanup(forgetSecond)

	forgetFirst()

	t.Setenv("AGENT_FACTORY_HOME", second)
	require.NoError(t, os.RemoveAll(second))
	require.ErrorIs(t, AtomicWriteFile(filepath.Join(second, "a"), []byte("x"), 0644), ErrAFHomeRemoved,
		"the stale forget must not have released the live daemon's latch")
}

// The guard has to hold at the level daemons actually write, not only at the
// primitives. These are the writers a live daemon runs in its steady state —
// SaveRepoInstances on every status poll, SaveState, and the config save — plus
// the two directory creations that used to run AHEAD of any of them and put the
// home back before the seam got a say.
//
// Enumerated by what the DAEMON calls rather than by grepping for os.MkdirAll: a
// grep proves existence, never wiring, and the residue #3845 was filed on
// (events/) came from a call site the issue believed the seam already covered.
func TestEveryDaemonStateWriterRefusesToResurrectADeletedHome(t *testing.T) {
	cases := []struct {
		name  string
		write func(home string) error
	}{
		{"SaveRepoInstances", func(home string) error {
			return SaveRepoInstances("abcdef123456", []byte("[]"))
		}},
		{"SaveState", func(home string) error {
			return SaveState(&State{})
		}},
		{"TrySaveState", func(home string) error {
			_, err := TrySaveState(&State{})
			return err
		}},
		{"SaveConfig", func(home string) error {
			return SaveConfig(&Config{SchemaVersion: GlobalConfigSchemaVersion})
		}},
		{"MkdirAllUnderAFHome/events", func(home string) error {
			return MkdirAllUnderAFHome(filepath.Join(home, "events"), 0755)
		}},
		{"MkdirAllUnderAFHome/logs", func(home string) error {
			return MkdirAllUnderAFHome(filepath.Join(home, "logs"), 0755)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := armedHome(t)
			require.NoError(t, os.RemoveAll(home))

			err := tc.write(home)

			require.Error(t, err, "%s must refuse a home this process saw deleted", tc.name)
			assert.True(t, errors.Is(err, ErrAFHomeRemoved), "%s: want ErrAFHomeRemoved, got %v", tc.name, err)
			assert.NoDirExists(t, home, "%s re-created the deleted home", tc.name)
		})
	}
}
