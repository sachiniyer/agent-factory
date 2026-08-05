package upgradetxn

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

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

	// Same umask trap as the directory above, and it bites harder here because it
	// is silent: under umask 077 the write lands on 0700, and the hard-link test
	// then asserts a 0755 that was never there. chmod explicitly, then prove it.
	executable := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(executable, []byte("af binary"), 0o755))
	require.NoError(t, os.Chmod(executable, 0o755))
	execInfo, err := os.Stat(executable)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), execInfo.Mode().Perm(),
		"the fixture binary must actually be 0755 whatever the umask, or a test asserting its mode survived proves nothing")
	return executable
}

// requireUnprivileged skips a test whose mechanism is a permission DENIAL. Root
// bypasses the mode bits entirely: it opens a 0000 file happily, so the EACCES
// these tests are built on never occurs. That does not merely make them fail —
// the retry test would PASS vacuously, reporting that a retry works when no
// retry ever ran — so skipping honestly beats either outcome.
func requireUnprivileged(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("running as root: a 0000 lock is still openable, so the permission denial this test is built on cannot be produced")
	}
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

// On a DIRECTORY, the write bit alone confers nothing — creating, removing or
// renaming an entry needs search permission too. So the classification has to
// test write AND execute per class, and getting it wrong breaks BOTH ways.
//
// 0772: group rwx, plus a stray other-write bit that grants no traversal. Those
// group members really can replace the binary, so reading the mode as
// world-writable and declining leaves them with EACCES and an unlocked install —
// the original bug, reintroduced through the predicate.
func TestExecutableLock_SharedWhenAStrayOtherWriteBitGrantsNoTraversal(t *testing.T) {
	executable := sharedInstallDir(t, 0o772)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm,
		"other-write without other-execute cannot traverse the directory, so this is a group-shared install and its group writers still need the lock")
}

// The inverse. 0720: group write, no group execute. Those members cannot
// traverse the directory and so cannot replace the binary — widening the lock to
// them would hand a blocking handle to principals who are not writers at all,
// which is the same over-admission the world-writable case declines.
func TestExecutableLock_NotWidenedWhenTheGroupCannotTraverse(t *testing.T) {
	executable := sharedInstallDir(t, 0o720)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"group write without group execute is not write access to a directory, so there is no second writer to admit")
}

// A HARD LINK at the lock path aims the mode change at somebody else's inode.
// O_NOFOLLOW rejects a symlink; it says nothing about a hard link, which is not
// a separate object at all but a second name for an existing one. Anyone who can
// write a shared install directory can leave `.af.af-upgrade.lock` pointing at
// `af` itself — and an fchmod through that name strips the executable bit off
// the binary before the swap even starts, bricking the install from a routine
// upgrade.
//
// This hazard is created by adjusting the mode at all, so it ships with the fix
// that introduced it.
func TestExecutableLock_DoesNotRestyleAHardLinkedTarget(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)
	require.NoError(t, os.Link(executable, executableLockPath(executable)),
		"the hazard is a second NAME for the binary's inode, planted at the lock path")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }),
		"a suspicious lock path must not block the upgrade, only decline to restyle")

	info, err := os.Stat(executable)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
		"the executable bit must survive: an fchmod through a hard-linked lock path would leave af non-executable")
}

// The narrowing direction. A directory later tightened from group-writable to
// private leaves a previously widened lock at 0660: the old group can no longer
// replace the binary, but can still OPEN the known lock path and hold it — and
// because this lock blocks, that hangs every future upgrade by the owner. So the
// audience has to track the directory in BOTH directions, not ratchet open.
func TestExecutableLock_NarrowsAgainWhenTheDirectoryIsTightened(t *testing.T) {
	executable := sharedInstallDir(t, 0o770)
	dir := filepath.Dir(executable)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))
	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(0o660), perm, "precondition: the shared directory widened the lock")

	require.NoError(t, os.Chmod(dir, 0o750))
	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ = lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"a tightened directory must narrow the lock back, or its former group keeps a blocking handle on every future upgrade")
}

// The creation window. The file must exist before its mode can be adjusted, so
// it is briefly 0600 even in a shared directory, and a second authorized writer
// arriving in that gap gets EACCES — which the in-place installer turns into an
// UNLOCKED swap, the exact outcome being closed.
//
// A real two-user race is not reproducible in a unit test, so this drives the
// mechanism directly: a lock at 0000 is unopenable even by its owner, which is
// the same EACCES the racing writer sees. Widening it from another goroutine
// stands in for the first writer finishing its chmod.
func TestExecutableLock_RetriesWhileAnotherWriterIsStillWideningIt(t *testing.T) {
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))

	// A budget long enough that a loaded machine cannot lose the race for real
	// reasons; the acquisition returns as soon as the open succeeds, so a passing
	// run does not actually wait this long.
	defer restoreRetryBudget(setRetryBudget(200, 5*time.Millisecond))

	widened := make(chan struct{})
	go func() {
		defer close(widened)
		time.Sleep(20 * time.Millisecond)
		_ = os.Chmod(lockPath, 0o660)
	}()

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }),
		"the acquisition must wait out the brief private window instead of falling back to an unlocked install")
	<-widened
}

// The retry must NOT apply to an ordinary private install. There EACCES means a
// genuinely unauthorized caller, waiting cannot change the answer, and a retry
// budget would only delay every such attempt.
func TestExecutableLock_DoesNotRetryInAPrivateDirectory(t *testing.T) {
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o700)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))

	// Budget long enough that spending it would be unmistakable in the elapsed
	// time below.
	defer restoreRetryBudget(setRetryBudget(50, 100*time.Millisecond))

	started := time.Now()
	err := withExecutableLock(executable, false, func() error {
		t.Fatal("the critical section must not run when the lock cannot be opened")
		return nil
	})
	elapsed := time.Since(started)

	require.Error(t, err)
	require.ErrorIs(t, err, os.ErrPermission)
	require.Less(t, elapsed, time.Second,
		"a private directory must fail fast: the full 5s budget here would mean the shared-directory retry is running where it cannot help")
}

func setRetryBudget(retries int, delay time.Duration) (int, time.Duration) {
	prevRetries, prevDelay := executableLockOpenRetries, executableLockOpenRetryDelay
	executableLockOpenRetries, executableLockOpenRetryDelay = retries, delay
	return prevRetries, prevDelay
}

func restoreRetryBudget(retries int, delay time.Duration) {
	executableLockOpenRetries, executableLockOpenRetryDelay = retries, delay
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
