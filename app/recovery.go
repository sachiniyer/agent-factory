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
	m.pendingProgram, m.pendingPrompt = req.Program, failed.rawPrompt
	m.pendingBackend, m.pendingAccount = req.Backend, req.Account
	m.pendingForceRemote = req.ForceRemote
	m.pendingAccountChosen = true
	m.menu.SetNamingHasPrompt(m.pendingPrompt != "")
	m.menu.SetNamingBackend(m.pendingBackend != "")
	m.menu.SetNamingAccount(m.pendingAccount != "")
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.failedCreate = nil
	return true
}
