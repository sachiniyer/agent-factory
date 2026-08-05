package upgradetxn

import (
	"errors"
	"fmt"
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
	if err := ensureDurableDirectory(home, root, transactionDirMode); err != nil {
		return fmt.Errorf("prepare upgrade root: %w", err)
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
