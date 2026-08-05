package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
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
		if err == nil || nonblocking || attempt >= executableLockOpenRetries ||
			!errors.Is(err, os.ErrPermission) {
			return lock, err
		}
		if _, shared := directoryWriterGroup(dir); !shared {
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
