package upgradetxn

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
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

// requireACLAwarePlatform skips a test that depends on the lock actually being
// WIDENED. The widening is Linux-only by design: on other platforms ACL state
// cannot be determined without the platform ACL API, and "cannot determine"
// reads as ambiguous, so the classifier declines and the lock stays private.
//
// These tests would therefore fail everywhere else for the right reason, which
// is worse than not running: a red macOS job that is reporting correct
// behaviour teaches the next reader to ignore it.
func requireACLAwarePlatform(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the lock widening is Linux-only: elsewhere ACL state cannot be determined, so the classifier declines and the lock stays private")
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
	requireACLAwarePlatform(t)
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
	requireACLAwarePlatform(t)
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
	requireACLAwarePlatform(t)
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
	requireACLAwarePlatform(t)
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

// A STICKY group-writable directory (1770) is not shared for this purpose.
// Group write and search let a member CREATE entries, but the sticky bit
// restricts renaming and removing to the entry's owner, the directory's owner,
// or a privileged process — so a group member generally cannot replace a binary
// owned by someone else. Widening there would hand a blocking lock to exactly
// the principals who cannot install, which is a denial of service the private
// lock never had.
//
// os.FileMode.Perm() drops the sticky bit, so a classifier reading Perm() alone
// cannot see this at all.
func TestExecutableLock_NotWidenedInAStickyDirectory(t *testing.T) {
	requireACLAwarePlatform(t)
	executable := sharedInstallDir(t, 0o770)
	dir := filepath.Dir(executable)
	require.NoError(t, os.Chmod(dir, 0o770|os.ModeSticky))
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.NotZero(t, info.Mode()&os.ModeSticky, "fixture must actually be sticky")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"a sticky directory does not let the group replace another user's binary, so the group must not get the lock either")
}

// With a POSIX ACL present, the group-class bits stat returns are the ACL MASK,
// not the owning group's effective rights. A directory can grant a named user
// rwx while the owning group has only r-x and the mask reads rwx: the mode says
// "group-writable", the truth is that the group cannot write and a user the mode
// cannot express can. Widening on that gets both halves wrong at once — it hands
// a blocking lock to non-writers and still excludes the real writer.
//
// So the classifier declines whenever an ACL makes the bits ambiguous. This
// drives setfacl rather than hand-encoding an ACL blob, and skips honestly where
// the tool or the filesystem cannot provide one.
func TestExecutableLock_NotWidenedWhenAnACLMakesTheModeBitsAmbiguous(t *testing.T) {
	requireACLAwarePlatform(t)
	executable := sharedInstallDir(t, 0o770)
	dir := filepath.Dir(executable)

	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is unavailable, so a real POSIX ACL cannot be created here")
	}
	// A named-user entry is what makes the group bits a mask rather than a
	// statement about the group.
	if out, err := exec.Command("setfacl", "-m", "u:root:rwx", dir).CombinedOutput(); err != nil {
		t.Skipf("this filesystem does not support POSIX ACLs: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	// Read the xattr directly rather than through the helper under test, so this
	// test compiles — and fails — against the code that lacks the helper.
	if _, err := unix.Getxattr(dir, "system.posix_acl_access", nil); err != nil {
		t.Skipf("no access ACL landed on the fixture: %v", err)
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o770), info.Mode().Perm(),
		"precondition: the mode bits must still LOOK group-writable, or the test proves nothing about ambiguity")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"an ACL makes the group bits a mask, so they no longer describe who may replace the binary and must not decide who gets the lock")
}

// A DEFAULT ACL is the inheritance case, and it is invisible to a probe that
// looks only at the directory's own access ACL.
//
// A directory can carry a `system.posix_acl_default` named-user entry and no
// access ACL at all: the mode bits then look plainly group-writable. The lock
// created inside it INHERITS that entry, with the mask intersected down by the
// 0600 create mode, so the entry is present but inert — and a later Chmod(0660)
// does not touch the entry, it widens the MASK, which switches the entry on. A
// named user who can traverse but not write the directory can then open a lock
// that BLOCKS, without ever being able to replace the binary.
func TestExecutableLock_NotWidenedWhenTheLockInheritsADefaultACL(t *testing.T) {
	requireACLAwarePlatform(t)
	executable := sharedInstallDir(t, 0o770)
	dir := filepath.Dir(executable)

	if _, err := exec.LookPath("setfacl"); err != nil {
		t.Skip("setfacl is unavailable, so a real default ACL cannot be created here")
	}
	// -d is the point: a DEFAULT entry only, leaving the directory with no access
	// ACL of its own.
	if out, err := exec.Command("setfacl", "-d", "-m", "u:root:rwx", dir).CombinedOutput(); err != nil {
		t.Skipf("this filesystem does not support POSIX ACLs: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	if _, err := unix.Getxattr(dir, "system.posix_acl_default", nil); err != nil {
		t.Skipf("no default ACL landed on the fixture: %v", err)
	}
	info, err := os.Stat(dir)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o770), info.Mode().Perm(),
		"precondition: the mode bits must still look group-writable, or the test proves nothing about the inherited case")

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"widening the mode widens the inherited ACL's mask, turning on entries for principals who cannot replace the binary but can hold the blocking lock")
}

// A NONBLOCKING caller that hits the transient 0600-to-0660 window must be told
// BUSY, not denied.
//
// The distinction decides what happens next, and the outcomes are not close:
// writeExecutableInPlaceWaiting defers only on ErrInstallLockBusy, and on any
// other error it swaps the binary with NEITHER lock held — the interleave with a
// live transaction this lock exists to prevent. Deferring an unattended
// launch-time update costs a retry next launch; swapping unlocked can corrupt
// someone's rollback.
//
// A 0000 lock stands in for the window: unopenable even by its owner, which is
// the same EACCES the racing writer sees.
func TestExecutableLock_NonblockingReportsBusyInsteadOfDeniedWhenShared(t *testing.T) {
	requireACLAwarePlatform(t)
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))

	err := withExecutableLock(executable, true, func() error {
		t.Fatal("the critical section must not run when the lock cannot be opened")
		return nil
	})
	require.Error(t, err)
	require.ErrorIs(t, err, ErrInstallLockBusy,
		"a shared-directory denial must reach the caller as BUSY: any other error makes the launch updater swap the binary with no lock held")
	require.NotErrorIs(t, err, os.ErrPermission,
		"it must not also present as a permission error, or a caller matching on that first would still take the unlocked path")
}

// A PERMANENT denial in a SHARED directory must stay a permission error too.
//
// This is the topology this PR accepts as unfixable: a 0770 directory whose
// owner is not in its group, with the lock already widened to that group. The
// directory owner can still replace the binary through the owner bits, but can
// never open the lock — and the directory is genuinely shared, so a busy answer
// keyed on sharedness alone would tell the launch updater to defer, report
// success, and silently never install again.
//
// The lock is left group-usable but owner-inaccessible, which is what an
// already-completed widening looks like to someone outside its group.
func TestExecutableLock_NonblockingStaysDeniedWhenTheLockIsAlreadyWidened(t *testing.T) {
	requireACLAwarePlatform(t)
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	// Group rw, owner nothing: Linux checks the owner class first, so this is the
	// EACCES a non-group caller sees against a lock whose widening already landed.
	require.NoError(t, os.Chmod(lockPath, 0o060))

	err := withExecutableLock(executable, true, func() error {
		t.Fatal("the critical section must not run when the lock cannot be opened")
		return nil
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInstallLockBusy,
		"the widening already completed, so this denial is permanent: reporting busy would defer the launch updater forever and silently retire auto-update for this user")
	require.ErrorIs(t, err, os.ErrPermission)
}

// The STALE denial. The nonblocking open can fail while the lock is still 0600
// and the creator can finish its chmod before the follow-up stat runs — so the
// lock reads as widened while the error in hand describes a moment that has
// already passed. Inferring "permanent" from the current state there returns a
// stale EACCES, and the caller swaps with no lock held, which is the interleave
// this branch exists to prevent.
//
// The window is between two adjacent syscalls, so it is pinned through the seam
// rather than raced: the probe reports "not transient" (as a completed widening
// would) and, at that instant, makes the lock openable — exactly the ordering
// where the first denial is already out of date. The acquisition must re-ask and
// succeed, not report the error it was holding.
func TestExecutableLock_NonblockingRetriesAStaleDenialAgainstAWidenedLock(t *testing.T) {
	requireACLAwarePlatform(t)
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))

	var probes int
	prev := lockDenialIsTransient
	lockDenialIsTransient = func(path string, dirGid int) bool {
		probes++
		// The creator's chmod lands here — after our open failed, before we decide.
		require.NoError(t, os.Chmod(path, 0o660))
		return false
	}
	t.Cleanup(func() { lockDenialIsTransient = prev })

	ran := false
	require.NoError(t, withExecutableLock(executable, true, func() error {
		ran = true
		return nil
	}), "a denial that was already stale must be re-asked, not reported")
	require.True(t, ran, "the critical section must run once the re-attempt succeeds")
	require.Equal(t, 1, probes, "the discriminator must be consulted exactly once")
}

// A LEGACY private lock is not a creation race, however much it looks like one.
//
// A 0600 lock left in a shared directory by a pre-change version, owned by
// another user, denies an authorized group writer permanently — nobody but its
// owner can ever widen it. Its missing group bits are indistinguishable from the
// create-then-chmod window by mode alone, so classifying on mode would return
// busy on every launch: `commands/autoupdate.go` treats that as a successful
// deferral, and auto-update goes silently dead for that writer with no error
// anywhere to say so.
//
// Age separates them. The real window is a create followed immediately by an
// fchmod; anything older is a leftover, and falls through to the permission
// error and the documented unlocked-swap fallback — which is what such a lock
// already does today.
func TestExecutableLock_NonblockingStaysDeniedForALegacyPrivateLock(t *testing.T) {
	requireACLAwarePlatform(t)
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o770)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))
	// Older than any create-then-chmod window, which is what "left by a previous
	// version" means in the only terms the filesystem records.
	old := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(lockPath, old, old))

	err := withExecutableLock(executable, true, func() error {
		t.Fatal("the critical section must not run when the lock cannot be opened")
		return nil
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInstallLockBusy,
		"a leftover private lock never clears, so reporting busy defers the updater on every launch and silently retires auto-update")
	require.ErrorIs(t, err, os.ErrPermission)
}

// The same nonblocking call in a PRIVATE directory must stay a permission error.
// There the denial is permanent and means a genuinely unauthorized caller;
// reporting BUSY would tell the launch updater to defer forever and silently
// stop auto-updating rather than fall back.
func TestExecutableLock_NonblockingStaysDeniedInAPrivateDirectory(t *testing.T) {
	requireUnprivileged(t)
	executable := sharedInstallDir(t, 0o700)
	lockPath := executableLockPath(executable)
	require.NoError(t, os.WriteFile(lockPath, nil, 0o600))
	require.NoError(t, os.Chmod(lockPath, 0o000))

	err := withExecutableLock(executable, true, func() error {
		t.Fatal("the critical section must not run when the lock cannot be opened")
		return nil
	})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrInstallLockBusy,
		"a permanent denial must not masquerade as transient contention, or the updater defers forever instead of falling back")
	require.ErrorIs(t, err, os.ErrPermission)
}

// The other side of the Linux-only decision, and the one that actually runs on
// the macOS job: where ACL state cannot be determined, a group-writable
// directory must NOT get a widened lock.
//
// Reporting "no ACL" on a platform whose ACLs this cannot see would hand that
// platform exactly the defect the probe removes on Linux — a 0660 lock published
// to an owning group an ACL may deny, while the ACL-authorized writer stays shut
// out and installs unlocked. Declining costs the widening and nothing else: the
// lock stays private, as it was before any of this.
func TestExecutableLock_NotWidenedWherePlatformACLStateIsUnknowable(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux can read POSIX ACLs directly, so it decides on the actual ACL rather than on not knowing")
	}
	executable := sharedInstallDir(t, 0o770)

	require.NoError(t, withExecutableLock(executable, false, func() error { return nil }))

	perm, _ := lockFileStat(t, executable)
	require.Equal(t, os.FileMode(journalFileMode), perm,
		"ACL state is undeterminable here, so the mode bits cannot be trusted to name the writers and the lock must stay private")
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
	requireACLAwarePlatform(t)
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
	requireACLAwarePlatform(t)
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
	requireACLAwarePlatform(t)
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
