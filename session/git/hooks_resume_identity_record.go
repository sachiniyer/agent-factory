package git

import (
	"fmt"
	"path/filepath"
	"syscall"
)

// hookWorktreeIdentity identifies the checkout's .git filesystem node rather
// than its contents. A linked worktree uses a pointer file and a main checkout
// uses a directory; replacing either checkout creates a different node.
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
	if !info.Mode().IsRegular() && !info.IsDir() {
		return nil, fmt.Errorf("worktree .git node is not a regular file or directory")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return nil, fmt.Errorf("worktree .git node has no filesystem identity")
	}
	return &hookWorktreeIdentity{Device: uint64(stat.Dev), Inode: uint64(stat.Ino)}, nil
}
