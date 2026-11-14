package session

import (
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// newerConversationIsEvidence reports whether a conversation newer than this
// session's recorded one, launched in launchDir (the directory the agent
// actually runs in, which an `env -C DIR` launch moves), is evidence that this
// session moved on (#7209 review R1). It is the attribution rule both agents'
// restore guards share.
//
// Only a directory af owns exclusively attributes what is filed under it to
// the session: a worktree af created for this session alone. An in-place row
// (the root agent, an `af sessions create --here` session, an adopted external
// worktree) works in the user's own checkout, where the user's own agent, an
// editor extension and any sibling in-place session file conversations too. A
// newer one there says nothing about this row, and nothing there stands in
// for the root's own record either (#7222). The launch directory
// must be that worktree or a directory inside it: an agent launched elsewhere
// files beside whatever else runs there, and one whose directory af cannot
// prove is attributed nothing. Without that evidence a guard resumes the
// recorded conversation and retires nothing.
//
// Each resolution is bounded (fsprobe, #7272): the worktree and the launch dir
// may sit on a mount that no longer answers, on the restore, respawn,
// account-swap and load paths that ask. A resolution that does not answer
// within the bound is a directory af cannot prove, so it is attributed
// nothing.
func (i *Instance) newerConversationIsEvidence(launchDir string) bool {
	i.mu.RLock()
	gw, inPlace, title := i.gitWorktree, i.inPlace, i.Title
	i.mu.RUnlock()
	if gw == nil || gw.IsExternalWorktree() || inPlace || IsReservedTitle(title) {
		return false
	}
	worktree, repo := gw.GetWorktreePath(), gw.GetRepoPath()
	if worktree == "" || strings.TrimSpace(launchDir) == "" {
		return false
	}
	own, err := fsprobe.ResolveForCompare(worktree)
	if err != nil {
		return false
	}
	// A worktree at the repository's own path is the user's checkout, whatever
	// the record's flags say.
	if checkout, err := fsprobe.ResolveForCompare(repo); err != nil || own == checkout {
		return false
	}
	dir, err := fsprobe.ResolveForCompare(launchDir)
	if err != nil {
		return false
	}
	return dir == own || strings.HasPrefix(dir, own+string(filepath.Separator))
}
