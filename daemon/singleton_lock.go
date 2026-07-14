package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/sachiniyer/agent-factory/config"
)

// daemonLockFileName is the per-home advisory lock file. A single daemon holds
// an exclusive flock on it for its whole lifetime, making it the source of
// truth for "is a daemon already running for this home" — stronger than the
// PID file, which readers can only heuristically validate (#960 split-brain).
const daemonLockFileName = "daemon.lock"

// daemonLockPath returns <home>/daemon.lock.
func daemonLockPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonLockFileName), nil
}

// homeLock is a held exclusive advisory lock on the per-home daemon lock file.
// The open fd MUST stay open for the daemon's whole lifetime: the flock is tied
// to the fd, so closing it — or the process dying, including a SIGKILL/crash —
// releases the lock. That auto-release is what makes a stale lock impossible:
// there is no pid-liveness guessing, a dead daemon's lock is simply gone.
type homeLock struct {
	f *os.File
}

// daemonLockHeldError reports that another live daemon already holds the
// per-home lock. It carries the holder's PID (best-effort, read from the PID
// file) purely for a human-readable message.
type daemonLockHeldError struct {
	holderPID int
}

func (e *daemonLockHeldError) Error() string {
	if e.holderPID > 0 {
		return fmt.Sprintf(
			"an af daemon is already running for this home (pid %d); refusing to start a second",
			e.holderPID,
		)
	}
	return "an af daemon is already running for this home; refusing to start a second"
}

// isDaemonLockHeldErr reports whether err is the "another daemon holds the
// lock" case (as opposed to an I/O error opening the lock file).
func isDaemonLockHeldErr(err error) bool {
	var held *daemonLockHeldError
	return errors.As(err, &held)
}

// acquireHomeLock takes the exclusive per-home advisory lock without blocking.
// On success the returned homeLock's fd is held open; the caller MUST keep it
// for the daemon's lifetime and release() it on exit. If another live daemon
// already holds the lock it returns a *daemonLockHeldError (checkable with
// isDaemonLockHeldErr) so the caller can fail fast with a clear message; any
// other error is an I/O failure opening the lock file.
func acquireHomeLock() (*homeLock, error) {
	path, err := daemonLockPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, fmt.Errorf("failed to create daemon lock directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, fmt.Errorf("failed to open daemon lock file %s: %w", path, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			holderPID := 0
			if pid, ok := readPIDFromFile(); ok {
				holderPID = pid
			}
			return nil, &daemonLockHeldError{holderPID: holderPID}
		}
		return nil, fmt.Errorf("failed to acquire daemon lock on %s: %w", path, err)
	}
	return &homeLock{f: f}, nil
}

// release drops the flock and closes the fd. Safe on a nil receiver. flock also
// auto-releases when the process exits, so this is only needed for a graceful
// shutdown that keeps the process alive afterward (tests) — but calling it is
// cheap and makes the lifetime explicit.
func (l *homeLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
	l.f = nil
}
