//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// openFilePaths reads /proc/<pid>/fd and returns every absolute filesystem
// path the process currently holds open. Non-path descriptors (sockets,
// pipes, anon_inodes) are skipped: they cannot name a file.
//
// The directory itself is readable only by the process owner (or root) under
// every procfs mount option, including hidepid=2 — so for the pane-member
// processes af inspects a read failure is either "it is gone" or a refusal
// that means something, never routine. Both reach the caller as errors: an
// empty result must mean "provably holds nothing open", not "could not look".
func openFilePaths(pid int) (map[string]struct{}, error) {
	dir := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, fmt.Errorf("read %s: %w", dir, err)
	}
	paths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join(dir, entry.Name()))
		if err != nil {
			// The fd closed between the directory listing and the read;
			// that is a smaller open set, not a failure to see it.
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("readlink %s/%s: %w", dir, entry.Name(), err)
		}
		if strings.HasPrefix(target, "/") {
			paths[target] = struct{}{}
		}
	}
	return paths, nil
}
