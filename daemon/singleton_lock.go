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

// daemonLockPathIn returns <dir>/daemon.lock for an arbitrary home directory.
// It is what lets `af doctor` ask the lock question of a home it is inspecting
// rather than only the current one (#1989).
func daemonLockPathIn(dir string) string {
	return filepath.Join(dir, daemonLockFileName)
}

// daemonLockPath returns <home>/daemon.lock for the current home.
func daemonLockPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return daemonLockPathIn(dir), nil
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

// The read side of the daemon lock, for `af doctor` (#1989).
//
// The lock the daemon holds for its whole lifetime is the SOUND answer to "is a
// daemon running for this home?" — the kernel releases it on death including
// SIGKILL, so a takeable lock is proof, not inference, that no live daemon owns
// the home. That is what a temp-home delete must rest on: doctor's old
// process-scan predicate ("I saw no process referencing this home") was an
// inferred negative that four consecutive P1 reviews each found a fresh way to
// falsify, and every one authorised an rm -rf. The lock cannot be falsified that
// way — but it CAN lie in two directions we must design against, below.

var (
	// errLockHeld reports that a live daemon holds the home's lock (EWOULDBLOCK).
	errLockHeld = errors.New("a live daemon holds this home's lock")
	// errLockUnprovable reports that we could not establish whether a daemon
	// holds the lock: no lock file at all, a filesystem whose flock cannot be
	// trusted, or an I/O error. It must never be read as "unused".
	errLockUnprovable = errors.New("cannot prove whether a daemon owns this home")
)

// lockFSReliable reports whether flock on a file under dir can be trusted: true
// on local filesystems, false on network ones (NFS, SMB, 9p, FUSE) where a
// successful flock may silently no-op and hand back a fabricated "I took it".
// A package var so a test can force either verdict without a real NFS mount.
var lockFSReliable = flockReliableFilesystem

// acquireHomeLockAt tries to take dir's daemon lock WITHOUT creating the lock
// file, and returns the held lock on success. The no-create rule is
// load-bearing: creating the file would let us take a lock nobody ever held,
// manufacturing the very "unused" proof we are testing for (#1989).
//
//   - (*homeLock, nil): the lock file existed on a filesystem whose flock we
//     trust, and we took it — proof that no live daemon owns the home. The
//     caller MUST release() it.
//   - (nil, errLockHeld): a live daemon holds it (EWOULDBLOCK). The home is in
//     use.
//   - (nil, errLockUnprovable): unprovable — no lock file (absence of a lock is
//     not proof of non-use), a filesystem whose flock we cannot vouch for, or an
//     I/O error.
func acquireHomeLockAt(dir string) (*homeLock, error) {
	path := daemonLockPathIn(dir)
	// No O_CREATE: a missing file means no daemon ever wrote a lock here, which
	// is UNKNOWN, not "free". Any other open error (permission, I/O) is equally
	// a failure to look, never evidence of freedom.
	f, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return nil, errLockUnprovable
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLockHeld
		}
		// A flock error that is not EWOULDBLOCK is not evidence of freedom.
		return nil, errLockUnprovable
	}
	// We took the lock — but a successful flock on a filesystem that does not
	// really lock (NFS and friends) is a FABRICATED positive: it reports
	// provably-unused when a daemon on this same host holds its own copy that
	// also silently did not lock. Trust the acquisition only where flock is
	// reliable; anywhere else the answer is unknown, never a delete-authorising
	// "no".
	if ok, ferr := lockFSReliable(dir); ferr != nil || !ok {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		return nil, errLockUnprovable
	}
	return &homeLock{f: f}, nil
}

// ProbeHomeLock answers "is a daemon running for this home?" through the
// per-home daemon.lock — the kernel-guaranteed fact the PID file and a process
// scan can only heuristically approximate (#960). It never creates the lock
// file, and it never blocks.
//
//   - AnswerYes    → a live daemon holds the lock. The home is IN USE.
//   - AnswerNo     → the lock existed on a trusted filesystem and we took it: no
//     live daemon owns the home. This is the ONLY answer that may authorise
//     removing an abandoned temp home (#1989) — a positive proof, not an
//     inferred negative.
//   - Undetermined → we could not tell: no lock file at all, a filesystem whose
//     flock cannot be trusted, or an I/O error. Leave the home alone and report
//     it.
//
// A flock proves only that no live DAEMON owns the home, not that nothing uses
// it: a home with live tmux sessions but a dead daemon holds no lock. Callers
// must keep that second signal (doctor's liveTmuxHomes) — it asks tmux, a
// different and sound surface — before acting on an AnswerNo.
func ProbeHomeLock(dir string) ProbeAnswer {
	lock, err := acquireHomeLockAt(dir)
	switch {
	case err == nil:
		lock.release()
		return AnswerNo()
	case errors.Is(err, errLockHeld):
		return AnswerYes()
	default:
		return Undetermined(fmt.Errorf("cannot verify whether a daemon owns %s: %w", dir, err))
	}
}
