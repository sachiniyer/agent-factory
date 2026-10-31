package session

import "github.com/sachiniyer/agent-factory/internal/hostlayout"

// noteHostLayout records the strings an instance's data describes about the
// daemon HOST — project path, worktree path, tmux names — in the process-wide
// layout index, so a sandbox response can be scrubbed of them in one pass without
// anything on the response path walking sessions or taking their locks
// (internal/hostlayout). Every projection of an instance (ToInstanceData) and
// every restore (FromInstanceData) passes through here, so a string is in the set
// before any response built from that data can carry it.
func noteHostLayout(d InstanceData) InstanceData {
	hostlayout.Default.Note(d.Path, d.Worktree.RepoPath, d.Worktree.WorktreePath, d.TmuxName)
	for i := range d.Tabs {
		hostlayout.Default.Note(d.Tabs[i].TmuxName)
	}
	return d
}
