package git

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// Every filesystem read on prune's verdict and resume paths goes through these
// helpers (#5189). A syscall on a stalled FUSE/NFS mount cannot be interrupted, so
// each runs on its own goroutine and the CALLER stops waiting at a deadline; the
// answer on a timeout is UNKNOWN, never "absent" or "fine", and unknown refuses.
// The abandoned goroutine ends when the syscall returns and holds only its own
// result channel — the same bounded leak BoundedLstat accepts.

// pruneFSOpTimeout bounds one filesystem operation. Read once per call so a test
// swapping it cannot race an abandoned goroutine.
var pruneFSOpTimeout = 10 * time.Second

// The start-up resume of interrupted prunes runs under ONE time budget that every
// bounded operation inside it shares: each uses min(its own bound, what remains),
// and once the budget is spent an operation does not start at all and reports
// DeadlineExceeded (unknown), so the resume stops cleanly with its intents left in
// place. The budget is process-wide while the resume runs; a concurrent prune in
// that window sees the same shortened bounds, which can only make it refuse sooner.
var operationBudget atomic.Pointer[time.Time]

// SetOperationBudget installs a deadline for every bounded prune operation until
// the returned function is called.
func SetOperationBudget(deadline time.Time) (clear func()) {
	operationBudget.Store(&deadline)
	return func() { operationBudget.CompareAndSwap(&deadline, nil) }
}

// RemainingBudget returns min(own, what remains of the installed budget) and
// whether the budget is spent. With no budget installed it returns own.
func RemainingBudget(own time.Duration) (time.Duration, bool) {
	p := operationBudget.Load()
	if p == nil {
		return own, false
	}
	rem := time.Until(*p)
	if rem <= 0 {
		return 0, true
	}
	if rem < own {
		return rem, false
	}
	return own, false
}

// BoundedFSOp runs op and returns its result, or a DeadlineExceeded error when it
// has not finished within the bound.
func BoundedFSOp[T any](what string, op func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	timeout, spent := RemainingBudget(pruneFSOpTimeout)
	if spent {
		var zero T
		return zero, fmt.Errorf("%s was not started: the start-up resume budget is spent: %w", what, context.DeadlineExceeded)
	}
	done := make(chan result, 1)
	go func() {
		v, err := op()
		done <- result{v, err}
	}()
	select {
	case r := <-done:
		return r.v, r.err
	case <-time.After(timeout):
		var zero T
		return zero, fmt.Errorf("%s did not finish within %s — the filesystem may be stalled: %w", what, timeout, context.DeadlineExceeded)
	}
}

func boundedReadFile(path string) ([]byte, error) {
	return BoundedFSOp("reading "+path, func() ([]byte, error) { return os.ReadFile(path) })
}

func boundedEvalSymlinks(path string) (string, error) {
	return BoundedFSOp("resolving "+path, func() (string, error) { return filepath.EvalSymlinks(path) })
}

// boundedReadHead reads at most n bytes from the start of a file.
func boundedReadHead(path string, n int) ([]byte, error) {
	return BoundedFSOp("reading "+path, func() ([]byte, error) {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		buf := make([]byte, n)
		got, err := f.Read(buf)
		if err != nil && got == 0 {
			return nil, err
		}
		return buf[:got], nil
	})
}

// A bounded MUTATION that times out has an UNKNOWN outcome, not a failed one: the
// abandoned goroutine can still finish after the caller gave up. BoundedMutation
// therefore records every abandoned mutation under the paths it touches, and
// MutationInFlight lets the caller refuse to act on those paths again until the
// abandoned operation has provably finished (or the process restarts and state is
// re-derived from disk).
var abandonedMutations = struct {
	sync.Mutex
	byKey map[string]<-chan struct{}
}{byKey: map[string]<-chan struct{}{}}

// LeafMutationKey names a registration leaf in the abandoned-mutation registry.
func LeafMutationKey(repoRoot, leafName string) string {
	return "leaf\x00" + repoRoot + "\x00" + leafName
}

// MutationInFlight returns an error naming the first key that has an abandoned
// mutation still running. A key whose abandoned mutation has finished is forgotten.
func MutationInFlight(keys ...string) error {
	abandonedMutations.Lock()
	defer abandonedMutations.Unlock()
	for _, k := range keys {
		ch, ok := abandonedMutations.byKey[k]
		if !ok {
			continue
		}
		select {
		case <-ch:
			delete(abandonedMutations.byKey, k)
		default:
			return fmt.Errorf("an earlier filesystem operation on %q timed out and may still be running; its outcome is unknown, so nothing acts on it again until it finishes (or the daemon restarts)", k)
		}
	}
	return nil
}

// BoundedMutationWithin runs a mutating op and waits at most timeout. On a timeout
// the op is left running, registered under keys, and DeadlineExceeded is returned.
// It refuses to start while an earlier abandoned mutation on any key still runs.
func BoundedMutationWithin(timeout time.Duration, what string, keys []string, op func() error) error {
	if err := MutationInFlight(keys...); err != nil {
		return err
	}
	timeout, spent := RemainingBudget(timeout)
	if spent {
		return fmt.Errorf("%s was not started: the start-up resume budget is spent: %w", what, context.DeadlineExceeded)
	}
	settled := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		err := op()
		close(settled)
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		abandonedMutations.Lock()
		for _, k := range keys {
			abandonedMutations.byKey[k] = settled
		}
		abandonedMutations.Unlock()
		return fmt.Errorf("%s did not finish within %s — its outcome is unknown: %w", what, timeout, context.DeadlineExceeded)
	}
}

// BoundedMutation is BoundedMutationWithin at the standard filesystem bound.
func BoundedMutation(what string, keys []string, op func() error) error {
	return BoundedMutationWithin(pruneFSOpTimeout, what, keys, op)
}

// BoundedRename renames within one directory. A timeout is ambiguous (the rename
// may still complete); it is registered under both paths.
func BoundedRename(from, to string) error {
	return BoundedMutation("renaming "+from, []string{from, to}, func() error { return os.Rename(from, to) })
}

// BoundedFreeSpace is a Statfs-and-Stat probe, bounded.
func BoundedPathExists(path string) (bool, error) {
	_, err := BoundedFSOp("inspecting "+path, func() (os.FileInfo, error) { return BoundedLstat(path) })
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}

// boundedDirIsEmpty reports whether dir is a directory with zero entries, bounded.
// A non-directory, or any entry, is "not empty".
func boundedDirIsEmpty(dir string) (bool, error) {
	return BoundedFSOp("listing "+dir, func() (bool, error) {
		f, err := os.Open(dir)
		if err != nil {
			return false, err
		}
		defer f.Close()
		names, err := f.Readdirnames(1)
		if err != nil && err != io.EOF {
			return false, err
		}
		return len(names) == 0, nil
	})
}
