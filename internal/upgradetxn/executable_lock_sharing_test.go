package upgradetxn

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// sharedInstallDir builds an install directory that is GROUP-writable, the shape
// a shared install (/usr/local/bin, a group-owned ~/.local/bin) actually takes,
// and returns the executable inside it.
//
// t.TempDir() is 0700, which is why every existing lock test sees the private
// case. The mode has to be set explicitly: os.MkdirAll applies the umask, so
// asking for 0770 alone would silently land on 0750 under the usual 022.
func sharedInstallDir(t *testing.T, perm os.FileMode) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "bin")
	require.NoError(t, os.Mkdir(dir, perm))
	require.NoError(t, os.Chmod(dir, perm))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, perm, info.Mode().Perm(), "the test fixture must actually have the mode it is testing")

	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("af binary"), 0o755))
	return executable
}

func lockFileStat(t *testing.T, executable string) (os.FileMode, uint32) {
	t.Helper()
	info, err := os.Stat(executableLockPath(executable))
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "need a unix stat to read the lock's group")
	return info.Mode().Perm(), stat.Gid
}

// THE FINDING (#2948, Codex on #2897). The executable lock exists to serialise
// every writer of one binary, across AF homes AND across users — that is the
// whole reason it is keyed to the executable rather than to a home.
//
// It was created 0600. In a group-writable install directory the first user to
// upgrade owns a lock nobody else can open, and a second user who can still
// rename the binary gets EACCES. writeExecutableInPlace treats that as "cannot
// take the lock" and installs UNLOCKED, so the exact interleaving this lock was
// added to prevent is back — silently, and only for the users who did not get
// there first.
//
// So: where the directory says other principals may replace the binary, the lock
// must say they may take it.
func TestExecutableLock_SharedWithTheDirectorysWriters(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm,
		"a group-writable install directory must produce a group-writable lock: anyone who can rename the binary must be able to take the lock that guards it")
}

// The group has to MATCH the directory's, not just be present. A new file takes
// the creator's primary group (the directory is not setgid), so a lock left at
// 0660/creator-group is still unopenable by the other members of the shared
// group — the same failure with an extra step.
//
// Single-user tests cannot see this: the temp dir's group already equals the
// test user's primary group, so an assertion of "lock gid == dir gid" would pass
// against completely unfixed code. Retarget the directory to a SECONDARY group
// first, which makes the two differ and gives the assertion teeth.
func TestExecutableLock_TakesTheDirectorysGroup(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)
	dir := filepath.Dir(executable)

	secondary, ok := secondaryGroup(t)
	if !ok {
		t.Skip("the test user belongs to no group other than its primary one, so a lock keeping its default group would be indistinguishable from one adopting the directory's")
	}
	require.NoError(t, os.Chown(dir, -1, secondary))
	require.NoError(t, os.Chmod(dir, 0o770), "chown clears setgid; restore the mode the fixture asserts")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, gid := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm)
	require.Equal(t, secondary, int(gid),
		"the lock must carry the install directory's group, not its creator's: 0660 against the wrong group excludes exactly the users it is meant to admit")
}

// secondaryGroup returns a group the test user is in that is NOT its primary
// group, and whether one exists.
func secondaryGroup(t *testing.T) (int, bool) {
	t.Helper()
	primary := os.Getegid()
	groups, err := os.Getgroups()
	require.NoError(t, err)
	for _, gid := range groups {
		if gid != primary {
			return gid, true
		}
	}
	return 0, false
}

// The self-heal. A lock created 0600 before this fix — or by a user whose umask
// or directory looked different at the time — must widen on the next
// acquisition by its owner, or the very first upgrade on a shared box would
// poison the lock for every other user permanently.
func TestExecutableLock_WidensAnExistingPrivateLock(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm,
		"an existing private lock must be widened by an owner that can still chmod it")
}

// A world-writable install directory is NOT widened. The lock blocks rather than
// failing, so a lock any local user may open is a lock any local user may hold
// forever — every upgrade on the box hangs. #2897 rejected /tmp as the lock
// location for exactly this reason, and the same reasoning applies to the mode.
//
// Nothing is lost by declining: a directory where anyone may replace the binary
// has no integrity left for this lock to protect.
func TestExecutableLock_NotWidenedInAWorldWritableDirectory(t *testing.T) {
	executable := sharedInstallDir(t, 0o777)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"a world-writable directory must not get a world-usable lock: any local user could then hold it and stall every upgrade")
}

// Prepare takes the executable lock through its own acquisition rather than
// through withExecutableLock, so it can drift. It has the same defect with a
// HARDER failure: it returns the open error, so on a shared box the second
// user's transactional upgrade does not merely go unlocked, it fails outright.
func TestPrepare_SharesTheExecutableLockToo(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)

	_, err := Prepare(Plan{
		ID:             "upgrade-" + strings.Repeat("c", 32),
		HomeDir:        t.TempDir(),
		ExecutablePath: executable,
		FromVersion:    "1.0.100",
		ToVersion:      "1.0.300",
		Candidate:      []byte("candidate-af-binary"),
		Daemon: DaemonSnapshot{
			WasRunning: true,
			BootID:     "boot",
			Owner:      DaemonOwner{Kind: SupervisionAdHoc},
		},
		RecoveryJob: RecoveryJob{Kind: RecoveryJobDetached},
	})
	require.NoError(t, err)

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm,
		"Prepare must take the lock the same way the in-place installer does, or the two acquisitions drift and only one of them is usable on a shared install")
}
