//go:build linux

package proctree

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// unixListenerPaths reads which sockets pid holds from /proc/<pid>/fd and the
// listening unix sockets among them from /proc/<pid>/net/unix, which lists the
// sockets of pid's own network namespace. Paths come back as bound, relative
// ones included; UnixListenerPaths resolves those.
func unixListenerPaths(pid int) ([]string, error) {
	fdDir := fmt.Sprintf("/proc/%d/fd", pid)
	ents, err := os.ReadDir(fdDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, err
	}
	inodes := make(map[string]bool, len(ents))
	for _, e := range ents {
		target, err := os.Readlink(fdDir + "/" + e.Name())
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue // closed since the listing: no longer held
			}
			return nil, err
		}
		if ino, ok := strings.CutPrefix(target, "socket:["); ok {
			if ino, ok := strings.CutSuffix(ino, "]"); ok {
				inodes[ino] = true
			}
		}
	}
	if len(inodes) == 0 {
		return nil, nil
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/net/unix", pid))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, ErrProcessExited
		}
		return nil, err
	}
	return parseNetUnixListeners(data, inodes)
}
