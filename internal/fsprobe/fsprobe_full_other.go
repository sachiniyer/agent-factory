//go:build !unix

package fsprobe

import (
	"fmt"
	"syscall"
)

// statfsFn is the test seam under Full — unused off unix, kept so tests can
// still fake the fullness answer.
var statfsFn = func(string, *syscall.Statfs_t) error { return nil }

// fsFull has no portable statfs off unix: report unsupported rather than a
// false clean bill — a platform that cannot answer the fullness question
// provides no evidence either way (#5295 review).
func fsFull(dir string) (bool, error) {
	return false, fmt.Errorf("filesystem fullness probe unsupported on this platform for %s: %w",
		dir, syscall.EOPNOTSUPP)
}
