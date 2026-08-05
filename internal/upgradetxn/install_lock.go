package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
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

// withExecutableLock runs fn holding the executable-keyed lock. Callers take it
// AFTER the per-home preparation lock, always in that order, so two homes racing
// the same binary cannot deadlock against each other.
func withExecutableLock(executablePath string, nonblocking bool, fn func() error) (retErr error) {
	executable, err := canonicalExistingFile(executablePath)
	if err != nil {
		return fmt.Errorf("validate executable for the upgrade lock: %w", err)
	}
	lock, err := acquireFileLockFlags(executableLockPath(executable), syscall.O_NOFOLLOW, nonblocking)
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
