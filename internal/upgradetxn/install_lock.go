package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// preparationLockName is the single lock that serialises publishing a
// transaction against an in-place binary swap. Named once so Prepare and
// WithInstallLock cannot drift onto different files and silently stop
// excluding each other.
const preparationLockName = "prepare.lock"

// WithInstallLock runs fn while holding the SAME preparation lock Prepare takes,
// so an in-place binary swap and a transaction publish cannot interleave (#2212).
//
// Two mechanisms replace the same executable: the in-place installers
// (`af upgrade`, launch-time auto-update) and this transactional path. Checking
// for an active transaction and then writing the binary is a
// time-of-check-to-time-of-use window on its own — a transaction published in
// between is invisible to a check that already passed, and the swap lands
// underneath it.
//
// It is also why Prepare snapshots the running executable INSIDE these locks:
// reading it outside would let a transaction preserve pre-swap bytes as its
// "previous" binary while newer bytes sit on disk, and a later rollback would
// then silently undo that install.
//
// The lock blocks rather than failing: an installer that arrives mid-preparation
// should wait out a bounded, seconds-long publish, not refuse. Callers that must
// not block are the ones that should be reading the journal instead.
//
// Like Prepare, this materialises the upgrade root if it is absent — taking a
// lock requires a file to take it on. That is a write, so this belongs on paths
// that are already mutating (an install), never on a read-only probe.
func WithInstallLock(homeDir, executablePath string, fn func() error) (retErr error) {
	return withInstallLock(homeDir, executablePath, false, fn)
}

// TryWithInstallLock is WithInstallLock for a caller that must not wait. It
// returns ErrInstallLockBusy rather than queueing when either lock is held.
//
// The launch-time updater needs this: it runs in front of a TUI that has not
// opened yet, and a daemon publishing a transaction holds the preparation lock
// for as long as it takes to copy and fsync a preserved binary. Blocking there
// would make an unattended update stall the launch the user actually asked for,
// which is the one thing that path may never do. `af upgrade` was asked for
// explicitly and keeps the waiting form.
func TryWithInstallLock(homeDir, executablePath string, fn func() error) (retErr error) {
	return withInstallLock(homeDir, executablePath, true, fn)
}

// ErrInstallLockBusy reports that another writer holds the upgrade locks.
var ErrInstallLockBusy = errors.New("another upgrade holds the install lock")

func withInstallLock(homeDir, executablePath string, nonblocking bool, fn func() error) (retErr error) {
	home, err := canonicalExistingDir(homeDir)
	if err != nil {
		return fmt.Errorf("validate upgrade home: %w", err)
	}
	root := upgradeRoot(home)
	if err := ensureLockRoot(root); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(root, preparationLockName), nonblocking)
	if err != nil {
		if nonblocking && errors.Is(err, ErrRecoveryActive) {
			return ErrInstallLockBusy
		}
		return fmt.Errorf("lock upgrade preparation: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(lock))
	}()
	return withExecutableLock(executablePath, nonblocking, fn)
}

// ensureLockRoot makes the upgrade root usable as a lock location WITHOUT
// restyling it. Prepare hardens the directory it creates for a transaction; an
// in-place installer only needs somewhere to take the lock, and it must not
// chmod a directory it did not create. That distinction matters because this now
// runs on every in-place upgrade: an AF home pointed at a broad user directory
// would otherwise have a routine `af upgrade` silently change the mode of an
// unrelated `upgrade/` folder.
//
// An existing directory is used exactly as it is. Only a directory this call
// creates is secured. Anything that is not a directory — a file, a symlink — is
// an error, and the caller's contract is to log it and install unlocked rather
// than let broken lock storage stand between a user and their upgrade.
func ensureLockRoot(path string) error {
	err := validateDirectoryNoSymlink(path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if mkErr := os.Mkdir(path, transactionDirMode); mkErr != nil {
		if !errors.Is(mkErr, os.ErrExist) {
			return fmt.Errorf("create upgrade lock root %s: %w", path, mkErr)
		}
	} else if chErr := os.Chmod(path, transactionDirMode); chErr != nil {
		return fmt.Errorf("secure new upgrade lock root %s: %w", path, chErr)
	}
	return validateDirectoryNoSymlink(path)
}

// executableLockPath returns the lock that serialises every writer of one
// executable, whatever AF home they belong to.
//
// The home lock cannot do this job. One `af` binary can serve many AF homes
// (AGENT_FACTORY_HOME), but a transaction is home-scoped while the executable is
// not — so two homes take two different `prepare.lock`s and exclude nothing over
// the binary they share. This lock is keyed to the executable instead, which is
// the thing actually being contended.
//
// It lives beside the executable because that is the only location derivable
// from the executable alone, and every writer already needs write access to that
// directory: an in-place swap renames a temp file into it, and upgradetxn stages
// its preserved and candidate binaries there. A world-writable location such as
// /tmp would be derivable too, and is rejected — a local user could hold the lock
// and stall every upgrade on the box, or pre-plant the path.
//
// The file is left behind on purpose. Removing a lock another process may be
// holding is a race, and an empty 0600 dotfile beside the binary costs nothing.
func executableLockPath(executable string) string {
	return filepath.Join(
		filepath.Dir(executable),
		"."+filepath.Base(executable)+".af-upgrade.lock",
	)
}

// acquireExecutableLock is the ONE way this package takes the executable lock.
// Prepare and the in-place installer both go through it: they contend for the
// same file, so an acquisition either of them spelled separately could widen on
// one path and not the other, leaving a lock usable by whichever writer happened
// to create it.
//
// The lock must be openable by every principal the install directory already
// trusts to replace the binary, or it does not do the job it was added for
// (#2948). Created 0600, it was openable by its creator alone: on a shared
// install the first user to upgrade owned a lock nobody else could open, the
// second user got EACCES, and — because a lock failure must never block an
// install — writeExecutableInPlace logged it and swapped the binary UNLOCKED.
// The cross-user interleave the executable key exists to prevent came back
// silently, for everyone except the user who got there first.
//
// So the lock's audience is derived from the directory's: group-writable
// directory, group-usable lock, carrying the DIRECTORY's group rather than the
// creator's (a new file takes its creator's primary group, which on a shared box
// is the wrong one and excludes exactly the users this is meant to admit).

// A first acquisition in a shared directory has an unavoidable moment of private
// state: the file must exist before its mode can be adjusted, so it is briefly
// 0600 between the create and the fchmod. A second authorized writer landing in
// that gap gets EACCES, and the in-place installer would swap the binary
// unlocked — the very outcome this is closing. Retry rather than accept it.
//
// Retries are gated twice. On the directory actually being shared, so they never
// delay the ordinary private-install case, where EACCES means a genuinely
// unauthorized caller and no amount of waiting changes the answer. And on the
// caller being willing to wait at all: #2951 gave the launch path a nonblocking
// acquisition precisely so an unattended update cannot stall a start-up, and a
// caller that refused to queue for the lock has not agreed to wait out someone
// else's chmod either.
//
// Vars, not constants, so a test can widen the budget instead of racing a real one.
var (
	executableLockOpenRetries    = 4
	executableLockOpenRetryDelay = 10 * time.Millisecond
)

func acquireExecutableLock(executable string, nonblocking bool) (*os.File, error) {
	path := executableLockPath(executable)
	dir := filepath.Dir(executable)
	prepare := func(lock *os.File) { alignLockWithDirectoryWriters(lock, dir) }
	for attempt := 0; ; attempt++ {
		lock, err := acquireFileLockPrepared(path, syscall.O_NOFOLLOW, nonblocking, prepare)
		if err == nil || !errors.Is(err, os.ErrPermission) {
			return lock, err
		}
		gid, shared := directoryWriterGroup(dir)
		if !shared {
			// A private directory. EACCES means a genuinely unauthorized caller and
			// no amount of waiting changes the answer, so neither wait nor dress it
			// up as contention.
			return lock, err
		}
		if nonblocking {
			// A denial here is reported as BUSY only if it is TRANSIENT — the
			// 0600-to-0660 window of another writer's first acquisition.
			//
			// That distinction decides what the caller does with it, and both wrong
			// answers are bad in opposite directions. writeExecutableInPlaceWaiting
			// defers only on ErrInstallLockBusy; on any other error it swaps the
			// binary with NEITHER lock held, which is the interleave with a live
			// transaction this lock exists to prevent. But a PERMANENT denial
			// reported as busy is worse still: the launch updater defers, reports
			// success, and silently never installs again.
			//
			// "Is the directory shared" does not answer that question, which is what
			// an earlier version of this branch got wrong. In the ownership topology
			// this PR accepts as unfixable — a 0770 directory whose owner is not in
			// its group, with the lock already widened to that group — the directory
			// owner is denied permanently while the directory is genuinely shared.
			//
			// Returned without sleeping either way: the caller asked not to wait
			// (#2951), and this reads the lock's state rather than waiting on it.
			if lockDenialIsTransient(path, gid) {
				return nil, ErrInstallLockBusy
			}
			// The lock reads as already widened, which normally means this denial
			// is about who we are. But the widening may have landed BETWEEN the
			// failed open above and that stat, in which case the error in hand is
			// STALE, not permanent — and returning it sends the caller down the
			// swap-with-no-lock path this whole branch exists to keep it off.
			//
			// Re-ask once. No sleep: the answer either changed already or it did
			// not, and the caller asked not to wait. A second denial against a
			// lock that is demonstrably widened is the permanent case.
			return acquireFileLockPrepared(path, syscall.O_NOFOLLOW, nonblocking, prepare)
		}
		if attempt >= executableLockOpenRetries {
			return lock, err
		}
		time.Sleep(executableLockOpenRetryDelay)
	}
}

// directoryWriterGroup reports the group the install directory shares write
// access with, and whether it shares at all. It is the single definition of
// "this binary has more than one authorized writer", so the lock's mode and the
// retry above cannot disagree about which directories are shared.
//
// A world-writable directory reports NOT shared. The lock BLOCKS rather than
// failing, so a lock any local user may open is a lock any local user may hold
// forever, stalling every upgrade on the box. #2897 rejected /tmp as the lock
// location on exactly this reasoning; it applies to the mode for the same
// reason. Nothing is given up by declining — a directory where anyone may swap
// the binary has no integrity left for this lock to protect.
func directoryWriterGroup(dir string) (int, bool) {
	info, err := os.Stat(dir)
	if err != nil {
		return 0, false
	}
	// Write AND execute, for both classes. On a DIRECTORY the write bit alone
	// confers nothing: creating, removing, or renaming an entry needs search
	// permission too, so a class with w and no x cannot replace the binary and
	// cannot plant the lock. Testing write alone gets both directions wrong — a
	// 0720 directory would widen the lock to a group that cannot install
	// anything, and a 0772 directory (group rwx, a stray other-write bit that
	// grants no traversal) would be misread as world-writable and left private,
	// so the group writers who really can install still get EACCES and still swap
	// unlocked.
	const (
		groupMayWrite = 0o030 // group write + group search
		otherMayWrite = 0o003 // other write + other search
	)
	perm := info.Mode().Perm()
	if perm&groupMayWrite != groupMayWrite || perm&otherMayWrite == otherMayWrite {
		return 0, false
	}
	// Everything above reads the mode bits as if they described the set of
	// principals who may replace the binary. Two things make that reading false,
	// and the only safe response to either is to stop reading them.
	//
	// STICKY (e.g. 1770). Group write and search let a member CREATE entries, but
	// the sticky bit restricts renaming and removing to the entry's owner, the
	// directory's owner, or a privileged process — so a group member generally
	// cannot replace a binary owned by someone else. Widening there admits
	// exactly the principals who cannot install, handing them a blocking lock and
	// a denial of service the private lock never had. Perm() drops this bit, so
	// it has to be read off the full mode.
	//
	// EXTENDED ACLs. With a POSIX ACL present, the group class bits returned by
	// stat are the ACL MASK, not the owning group's effective rights. A directory
	// can grant a named user rwx while the owning group has r-x and the mask
	// reads rwx: the mode says "group-writable", the truth is that the group
	// cannot write and a user invisible to the mode can. Widening on that gets
	// both halves wrong at once.
	//
	// Declining costs only the widening — the lock still works for its creator,
	// and the topology behaves exactly as it did before any of this — whereas
	// guessing wrong hands a blocking lock to non-writers.
	if info.Mode()&os.ModeSticky != 0 || hasExtendedACL(dir) {
		return 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(stat.Gid), true
}

// alignLockWithDirectoryWriters keeps the lock's audience equal to the set of
// principals the install directory trusts to replace the binary. Both
// directions: a shared directory widens the lock, an unshared one narrows it
// back. A one-way widen would leave a directory later tightened from 0770 to
// 0750 with a 0660 lock its old group can still OPEN and hold — and because
// this lock blocks, that group could then hang every future upgrade by the
// owner without being able to install anything themselves.
//
// Best-effort is the whole contract: every step accommodates OTHER writers and
// we already hold a working descriptor. When the lock belongs to a different
// user the fchmod/fchown fail EPERM — that user either got it right or their
// own next acquisition repairs it. Nothing here may become an error that stops
// an upgrade.
//
// All of it operates on the descriptor already opened O_NOFOLLOW, never on the
// path: this directory is by definition writable by someone else, so a
// path-based chmod could be aimed at a symlink swapped in after the stat.
func alignLockWithDirectoryWriters(lock *os.File, dir string) {
	info, err := lock.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return
	}
	// O_NOFOLLOW rejects a SYMLINK at the lock path; it says nothing about a HARD
	// LINK, which is not a distinct object at all but a second name for an inode
	// that already exists. Anyone who can write this directory can leave
	// `.af.af-upgrade.lock` pointing at the `af` binary itself, and an fchmod
	// through that name would strip the executable bit off `af` before the swap
	// ever started — bricking the install from a routine upgrade. Nlink is the
	// direct test, and it guards the narrowing branch too: shrinking a linked
	// inode to 0600 destroys it just as thoroughly.
	//
	// Declining to restyle is the safe answer rather than refusing the lock: it
	// falls back to the pre-existing behaviour instead of standing between a user
	// and their upgrade.
	if stat.Nlink != 1 {
		return
	}

	// An ACL the lock INHERITED from the directory's default is inert only while
	// the mask stays narrow. Widening the mode widens the mask, which switches the
	// inherited entries on — so this file's own ACL has to be consulted, not just
	// the directory's. Checked before either branch, because narrowing an
	// ACL-bearing file rewrites its mask too.
	if lockHasInheritedACL(lock) {
		return
	}

	gid, shared := directoryWriterGroup(dir)
	if !shared {
		if info.Mode().Perm() != journalFileMode {
			_ = lock.Chmod(journalFileMode)
		}
		return
	}

	// The group must MATCH the directory's, and the mode must not widen without
	// it. A directory can be owned by one user and group-writable for a group
	// that user does not belong to — they may replace the binary as its owner,
	// but their chown to the directory's group is refused. Widening anyway would
	// publish the lock to the CREATOR's primary group: the authorized writers
	// still get EACCES and still install unlocked, while an unrelated group gains
	// the ability to hold a blocking lock. Strictly worse than leaving it private,
	// so the mode change is gated on the group being right.
	//
	// Group before mode, because chown clears the setuid/setgid bits and would
	// undo a mode set first.
	if int(stat.Gid) != gid {
		if err := lock.Chown(-1, gid); err != nil {
			return
		}
	}
	_ = lock.Chmod(journalFileMode | 0o060)
}

// withExecutableLock runs fn holding the executable-keyed lock. Callers take it
// AFTER the per-home preparation lock, always in that order, so two homes racing
// the same binary cannot deadlock against each other.
func withExecutableLock(executablePath string, nonblocking bool, fn func() error) (retErr error) {
	executable, err := canonicalExistingFile(executablePath)
	if err != nil {
		return fmt.Errorf("validate executable for the upgrade lock: %w", err)
	}
	lock, err := acquireExecutableLock(executable, nonblocking)
	if err != nil {
		if nonblocking && errors.Is(err, ErrRecoveryActive) {
			return ErrInstallLockBusy
		}
		return fmt.Errorf("lock the executable for upgrade: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(lock))
	}()
	return fn()
}

// foreignTransactionOver returns the id of a transaction other than selfID that
// has staged a preserved-previous binary beside executable, or "" when none has.
//
// It exists because a transaction is home-scoped while the executable is not.
// Two AF homes sharing one `af` binary each keep their own journal, each take
// their own per-home lock, and neither can see the other's — so both could hold
// an active transaction over the same file. The executable lock serialises the
// act of publishing, not the lifetime of what was published.
//
// There is no registry of AF homes to consult and none is needed:
// binaryArtifactPaths stages the preserved and candidate binaries next to the
// executable, so its directory is the one place every home's transaction over
// that binary is visible.
//
// Matched on the preserved-PREVIOUS artifact, because that is the rollback
// input a second transaction would put at risk. Read with ReadDir and a literal
// prefix rather than filepath.Glob: an executable whose name contains a glob
// metacharacter would otherwise match the wrong set.
func foreignTransactionOver(executable, selfID string) (string, error) {
	dir := filepath.Dir(executable)
	prefix := "." + filepath.Base(executable) + ".af-upgrade-"
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", fmt.Errorf("inspect %s for other upgrade transactions: %w", dir, err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".previous") {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".previous")
		if id == "" || id == selfID {
			continue
		}
		return id, nil
	}
	return "", nil
}

// hasExtendedACL reports whether the path carries a POSIX access ACL, which
// makes the group-class mode bits a MASK rather than a statement about the
// owning group (see directoryWriterGroup).
//
// Presence is the whole question — the entries are never parsed. This is used
// only to DECLINE widening, so the conservative answer on any uncertainty is
// "assume ambiguity", and a probe that cannot tell simply reports false and
// leaves the mode-bit reading in charge, which is where it was before.
//
// The probe below is Linux's ACL representation. Everywhere else this reports
// AMBIGUOUS rather than absent, which is the whole point: the Linux names always
// fail on Darwin, so reporting "no ACL" there would silently hand a Darwin
// install exactly the defect this probe removes on Linux — a 0660 lock published
// to an owning group an ACL may deny, and the ACL-authorized writer still shut
// out. Determining Darwin ACL state needs the platform ACL API, and shipping an
// implementation of it that cannot be exercised here would be guessing; refusing
// to widen is the answer that is right without being verified.
//
// The effect is that the widening is Linux-only. Every other platform keeps the
// private lock it had before this change, which costs the widening and nothing
// else.
func hasExtendedACL(path string) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	// BOTH ACLs, and the default one is not optional. A directory can carry only
	// a system.posix_acl_default named-user entry and no access ACL of its own, so
	// probing the access ACL alone reports "unambiguous" for a directory whose
	// ACL reaches the lock by INHERITANCE rather than by being present here.
	for _, attr := range []string{"system.posix_acl_access", "system.posix_acl_default"} {
		// A zero-length read: only the error matters, never the value.
		if _, err := unix.Getxattr(path, attr, nil); err == nil {
			return true
		}
	}
	return false
}

// lockHasInheritedACL reports whether the OPENED lock carries an access ACL of
// its own, read from the descriptor rather than the path.
//
// A file created in a directory with a default ACL inherits it, with the mask
// intersected down by the 0600 create mode — so the inherited entries are
// present but inert. A later Chmod(0660) does not touch those entries; it widens
// the MASK, which switches them on. The named user in an inherited entry can
// then open a lock that BLOCKS, without necessarily being able to write the
// directory or replace the binary at all.
//
// Checking the descriptor closes that regardless of how the ACL arrived, and
// costs no extra TOCTOU: it is the same object about to be chmod'ed.
func lockHasInheritedACL(lock *os.File) bool {
	if runtime.GOOS != "linux" {
		return true
	}
	_, err := unix.Fgetxattr(int(lock.Fd()), "system.posix_acl_access", nil)
	return err == nil
}

// executableLockDenialIsTransient distinguishes a permission denial that will
// clear on its own from one that never will, for a caller that cannot wait to
// find out.
//
// The transient case is another writer's FIRST acquisition: the file must exist
// before its mode can be adjusted, so it is briefly private between the create
// and the fchmod. Microseconds later it is group-usable.
//
// The permanent case looks identical to the failed open and is only
// distinguishable from the lock itself: once the widening has completed, the
// group bits are set and the file carries the directory's group — so a caller
// still denied at that point is denied for WHO IT IS, not for when it arrived.
// The directory owner outside the directory's own group is exactly that caller,
// and telling it to defer would silently retire its auto-update.
//
// stat needs only search permission on the directory, which any caller that got
// this far already has. An absent lock means we are racing its creation, which
// is the transient case by definition.
// lockDenialIsTransient is the seam the nonblocking branch calls through, so a
// test can pin the stale-denial ordering rather than race it. Production never
// reassigns it.
// executableLockFreshWindow is how recently a still-private lock must have been
// created for its denial to read as the creation race rather than as a legacy
// private lock.
//
// A still-private lock owned by someone else is OBSERVATIONALLY IDENTICAL in the
// two cases that matter, and they want opposite answers:
//
//   - a leftover from a pre-change version, which never clears, so reporting
//     busy defers the launch updater on every launch and silently retires
//     auto-update;
//   - a live creation race whose owner is merely stalled, where reporting a
//     permission error sends the caller down the unlocked swap and lets it
//     overlap the creator when it resumes.
//
// Nothing distinguishes them from here. The owner's pid is unknown, and its
// flock cannot be probed without opening the file — which is the very thing
// being denied. Age is the only available signal, so this is a threshold rather
// than a decision, chosen to be wrong as rarely as possible.
//
// A minute is far past any create-then-fchmod pause that is not pathological —
// those two syscalls are adjacent, and this covers a debugger, a SIGSTOP, or a
// scheduler stall by orders of magnitude — while still far below the age of
// anything left by a previous version. Past it, the behaviour is exactly what
// that lock produces today without this change, so the residual case is not a
// regression; inside it, an updater that defers costs one skipped launch.
const executableLockFreshWindow = time.Minute

var lockDenialIsTransient = executableLockDenialIsTransient

func executableLockDenialIsTransient(lockPath string, dirGid int) bool {
	info, err := os.Lstat(lockPath)
	if err != nil {
		return true
	}
	if !info.Mode().IsRegular() {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	const groupUsable = 0o060
	if info.Mode().Perm()&groupUsable == groupUsable && int(stat.Gid) == dirGid {
		// Widened already, so a denial is about who we are. The caller re-asks
		// once before trusting this, since the widening may have landed between
		// its failed open and this stat.
		return false
	}
	// Still private. That is the creation window only if the lock is NEW. A 0600
	// lock left by a pre-change version under another user is permanently private
	// from here — nobody but its owner can widen it — and calling that transient
	// would defer the launch updater on every single launch, reporting success
	// and never installing. Auto-update would be silently dead for that writer,
	// with no error anywhere to say so.
	//
	// The window this is separating them by is microseconds wide in practice: a
	// create followed immediately by an fchmod. Anything older is not a race that
	// is about to resolve, so it falls through to the permission error and the
	// documented unlocked-swap fallback — which is exactly what such a lock does
	// today, before this change.
	return time.Since(info.ModTime()) < executableLockFreshWindow
}
