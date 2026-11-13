package session

import (
	"io/fs"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// An agent's conversation store (Claude's ~/.claude/projects/<spelling>,
// Codex's sessions tree) is read on the daemon's poll and at load, by the
// restore guard, conversation capture and the carry check-ahead. It may sit on
// a mount that stops answering (NFS, FUSE, a dead sshfs), and an unbounded
// read there would stall the poll goroutine and the session monitor with it.
// So every listing of a store runs inside fsprobe's bounded flight (#7212). A
// listing that does not answer within the bound is an error like any other
// unreadable directory: the store could not be looked at, which every caller
// already treats as "says nothing" rather than as an empty store.

// walkConversationStore is filepath.WalkDir over root with every directory
// listing, and the root's own lstat, bounded by fsprobe. Paths reach fn
// joined onto root, as filepath.WalkDir gives them. withInfo resolves each
// entry's Info inside its directory's flight, for a walk that reads entries'
// metadata.
func walkConversationStore(root string, withInfo bool, fn fs.WalkDirFunc) error {
	store := boundedStoreFS{root: root, withInfo: withInfo}
	return fs.WalkDir(store, ".", func(name string, d fs.DirEntry, err error) error {
		return fn(store.path(name), d, err)
	})
}

// boundedStoreFS is the fs.FS view walkConversationStore walks. fs.WalkDir
// reads it only through Stat and ReadDir; Open is never reached.
type boundedStoreFS struct {
	root     string
	withInfo bool
}

func (f boundedStoreFS) path(name string) string {
	if name == "." {
		return f.root
	}
	return filepath.Join(f.root, filepath.FromSlash(name))
}

func (f boundedStoreFS) Open(name string) (fs.File, error) {
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrInvalid}
}

// Stat is an lstat, as filepath.WalkDir's of its root: a root that is itself a
// symbolic link is reported, not followed.
func (f boundedStoreFS) Stat(name string) (fs.FileInfo, error) {
	return fsprobe.Lstat(f.path(name))
}

func (f boundedStoreFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if f.withInfo {
		return fsprobe.ReadDirInfo(f.path(name))
	}
	return fsprobe.ReadDir(f.path(name))
}
