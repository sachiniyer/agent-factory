package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// Publication holds .progress while creating this lease. The runner retains
// the descriptor across every launch gap until finish; a daemon exit releases
// it automatically, leaving the existing scope/launcher probes to protect the
// survivor. No timeout, PID-reuse guess, or heartbeat freshness is involved.
func newHookProgressLease(dir string) (*os.File, error) {
	file, err := os.OpenFile(filepath.Join(dir, "runner.lock"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

// Called under .progress after the batched liveness probes. Keep the lease
// locked through deletion. Never create a missing lease: older journals have
// none, and opening for pruning must not recreate a removed receipt directory.
func pruneUnleasedHookProgress(path string, p *hookProgress) (bool, error) {
	return withInactiveHookProgressLease(p.Directory, func() error { return removeHookProgress(path, p) })
}

func withInactiveHookProgressLease(dir string, remove func() error) (bool, error) {
	leasePath := filepath.Join(dir, "runner.lock")
	file, err := os.OpenFile(leasePath, os.O_RDWR|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if os.IsNotExist(err) {
		return true, remove()
	}
	if err != nil {
		return false, err
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	current, err := file.Stat()
	if err != nil {
		return false, err
	}
	linked, err := os.Lstat(leasePath)
	if err != nil {
		return false, err
	}
	if !current.Mode().IsRegular() || !os.SameFile(current, linked) {
		return false, fmt.Errorf("hook runner lease was replaced: %s", leasePath)
	}
	return true, remove()
}
