package session

import (
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
)

// newerConversationIsEvidence reports whether a conversation newer than this
// session's recorded one, found in its working directory, is evidence that
// this session moved on (#7209 review R1). It is the attribution rule both
// agents' restore guards share.
//
// Only a directory af owns exclusively attributes what is filed under it to
// the session: a worktree af created for this session alone. An in-place row
// (the root agent, an `af sessions create --here` session, an adopted external
// worktree) works in the user's own checkout, where the user's own agent, an
// editor extension and any sibling in-place session file conversations too. A
// newer one there says nothing about this row, which is the rule
// daemon/rootagent_claude.go already states for the root. Without that
// evidence a guard resumes the recorded conversation and retires nothing.
func (i *Instance) newerConversationIsEvidence() bool {
	i.mu.RLock()
	gw, inPlace, title := i.gitWorktree, i.inPlace, i.Title
	i.mu.RUnlock()
	if gw == nil || gw.IsExternalWorktree() || inPlace || IsReservedTitle(title) {
		return false
	}
	worktree, repo := gw.GetWorktreePath(), gw.GetRepoPath()
	if worktree == "" {
		return false
	}
	// A worktree at the repository's own path is the user's checkout, whatever
	// the record's flags say.
	return pathutil.ResolveForCompare(filepath.Clean(worktree)) != pathutil.ResolveForCompare(filepath.Clean(repo))
}
