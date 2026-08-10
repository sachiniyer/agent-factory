package session

import "github.com/sachiniyer/agent-factory/session/git"

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
