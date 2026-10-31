package daemon

import "github.com/sachiniyer/agent-factory/session"

// keptBranchBeforeKill reads the branch a kill will leave in the repo, from the
// worktree's HEAD, before teardown removes the worktree (#6233). A session that
// cannot be resolved reports nothing; the kill itself surfaces that error.
func (m *Manager) keptBranchBeforeKill(req KillSessionRequest) session.KeptBranch {
	instance, _, _, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil || instance == nil {
		return session.KeptBranch{}
	}
	return instance.KeptBranchAtDelete()
}
