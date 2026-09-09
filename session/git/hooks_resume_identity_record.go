package git

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// hookWorktreeIdentity identifies the .git pointer file rather than its text.
// Git can reuse the same registration pathname after deleting and recreating a
// linked worktree, but the replacement pointer is a different filesystem node.
type hookWorktreeIdentity struct {
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
}

func (identity *hookWorktreeIdentity) same(other *hookWorktreeIdentity) bool {
	return identity != nil && other != nil && identity.Device == other.Device && identity.Inode == other.Inode
}

func readHookWorktreeIdentity(worktree string) (*hookWorktreeIdentity, error) {
	info, err := BoundedLstat(filepath.Join(worktree, ".git"))
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("linked worktree .git pointer is not a regular file")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("linked worktree .git pointer has no filesystem identity")
	}
	return &hookWorktreeIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}
