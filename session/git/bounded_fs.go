package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
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

// BoundedFSOp runs op and returns its result, or a DeadlineExceeded error when it
// has not finished within the bound.
func BoundedFSOp[T any](what string, op func() (T, error)) (T, error) {
	type result struct {
		v   T
		err error
	}
	timeout := pruneFSOpTimeout
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

// BoundedRename renames within one directory, bounded. A timeout is ambiguous (the
// rename may still complete), which the caller must treat as unknown.
func BoundedRename(from, to string) error {
	_, err := BoundedFSOp("renaming "+from, func() (struct{}, error) { return struct{}{}, os.Rename(from, to) })
	return err
}

// BoundedFreeSpace is a Statfs-and-Stat probe, bounded.
func BoundedPathExists(path string) (bool, error) {
	_, err := BoundedLstat(path)
	switch {
	case err == nil:
		return true, nil
	case os.IsNotExist(err):
		return false, nil
	default:
		return false, err
	}
}
