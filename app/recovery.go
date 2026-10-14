package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

type recoveryNotice struct{ condition, detail, action string }

func (m *home) showRecovery(condition, detail, action string, err error) tea.Cmd {
	m.recovery = &recoveryNotice{condition, detail, action}
	return m.handleError(err)
}

// restoreFailedCreate reopens the captured draft, never a newer form. A user
// who navigated away can recover it with the next create action in its project.
func (m *home) restoreFailedCreate() bool {
	failed := m.failedCreate
	if failed == nil || failed.draft == nil || failed.draft.RepoPath != m.repoRoot || m.namingInstance != nil {
		return false
	}
	req := failed.draft
	instance := failed.instance
	_ = instance.Transition(session.ClearOp())
	_ = instance.Transition(session.BeginCreate())
	m.store.AddInstance(instance)
	m.sidebar.SelectInstance(instance)
	m.namingInstance = instance
	// The field re-seeds from the placeholder's Program — the value the user was
	// shown — not req.Program, which is the WIRE value and is "" for a submit
	// whose program was implicit (#4889 review). What a resubmit SENDS depends
	// on what the original request sent, though: an explicit program stays
	// explicit (it is a choice the user already committed), while an implicit
	// one stays implicit so a retry resolves the live default_program again —
	// the failure that produced this draft may well be the old default going
	// away, and replaying it as a pick would retry the same dead program.
	m.pendingProgram, m.pendingPrompt = instance.Program, failed.rawPrompt
	m.pendingBackend, m.pendingAccount = req.Backend, req.Account
	m.pendingAccountChosen = req.Account != ""
	m.pendingProgramChosen = req.Program != ""
	if !m.pendingProgramChosen {
		// Re-resolve the live default for the restored field's seed: the
		// displayed program is what a retry's implicit "" will resolve to, so
		// it should be the CURRENT default, not the stale one the failed form
		// happened to show (#4889 review).
		m.refreshDefaultProgram()
		m.pendingProgram = m.defaultProgram()
	}
	m.menu.SetNamingHasPrompt(m.pendingPrompt != "")
	m.menu.SetNamingBackend(m.pendingBackend != "")
	m.menu.SetNamingAccount(m.pendingAccount != "")
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.failedCreate = nil
	return true
}
