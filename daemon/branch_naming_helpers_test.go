package daemon

import "github.com/sachiniyer/agent-factory/session/git"

// globalBranchNaming is the naming a create resolves in a repo whose project sets
// no branch_prefix override: the manager's live global value (#4539). Tests that
// drive a title rule directly pass it where admission passes its resolved value.
func (m *Manager) globalBranchNaming() branchNaming {
	prefix := m.Config().BranchPrefix
	return branchNaming{prefix: prefix, global: prefix, local: true}
}

// branchForTitle is the branch a create in such a repo derives for title.
func (m *Manager) branchForTitle(title string) string {
	return m.globalBranchNaming().branchFor(title)
}

// titlesCollide is the branch-collision rule a create in such a repo applies.
func (m *Manager) titlesCollide(a, b string) bool {
	return git.TitlesCollide(a, b, m.globalBranchNaming().prefix)
}
