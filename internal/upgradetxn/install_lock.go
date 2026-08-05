package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// underneath it. Serialising both through one lock is what closes that window,
// and it is why Prepare snapshots the running executable inside the lock rather
// than before it.
//
// It is also why Prepare snapshots the running executable INSIDE this lock. An
// in-place installer takes the same lock around its swap, so reading the
// executable outside it would let a transaction preserve pre-swap bytes as its
// "previous" binary while newer bytes sit on disk — and a later rollback would
// then silently undo that install.
//
// The lock blocks rather than failing: an installer that arrives mid-preparation
// should wait out a bounded, seconds-long publish, not refuse. Callers that must
// not block are the ones that should be reading the journal instead.
//
// Like Prepare, this materialises the upgrade root if it is absent — taking a
// lock requires a file to take it on. That is a write, so this belongs on paths
// that are already mutating (an install), never on a read-only probe.
func WithInstallLock(homeDir string, fn func() error) (retErr error) {
	home, err := canonicalExistingDir(homeDir)
	if err != nil {
		return fmt.Errorf("validate upgrade home: %w", err)
	}
	root := upgradeRoot(home)
	if err := ensureLockRoot(root); err != nil {
		return err
	}
	lock, err := acquireFileLock(filepath.Join(root, preparationLockName), false)
	if err != nil {
		return fmt.Errorf("lock upgrade preparation: %w", err)
	}
	defer func() {
		retErr = errors.Join(retErr, releaseFileLock(lock))
	}()
	return fn()
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
