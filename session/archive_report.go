package session

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session/git"
)

// GetArchiveReport returns durable metadata for files omitted from this
// session's archive. The empty report means the archive was complete.
func (i *Instance) GetArchiveReport() git.ArchiveReport {
	i.mu.RLock()
	worktree := i.gitWorktree
	i.mu.RUnlock()
	if worktree == nil {
		return git.ArchiveReport{}
	}
	return worktree.GetArchiveReport()
}

// AdoptRetainedArchiveTree records path as a retained complete-source tree in
// this session's durable archive report (#7816); see
// GitWorktree.AdoptRetainedTree. The dest-present adoption route calls it for
// a `.af-source-*` quarantine its arbitration proved is this session's
// stranded complete original.
func (i *Instance) AdoptRetainedArchiveTree(path string) error {
	i.mu.RLock()
	worktree := i.gitWorktree
	i.mu.RUnlock()
	if worktree == nil {
		return fmt.Errorf("cannot adopt a retained tree for %q: instance has no worktree", i.Title)
	}
	return worktree.AdoptRetainedTree(path)
}
