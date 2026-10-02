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
	// whose program was implicit (#4889 review). The restored draft is a
	// confirmed choice either way, so what the resubmit sends is the value the
	// user is looking at.
	m.pendingProgram, m.pendingPrompt = instance.Program, failed.rawPrompt
	m.pendingBackend, m.pendingAccount = req.Backend, req.Account
	m.pendingAccountChosen = true
	// The restored draft's program is a value the user already submitted once —
	// a confirmed choice, so the resubmit sends it concretely on the wire rather
	// than as "" (#4889 review).
	m.pendingProgramChosen = true
	m.menu.SetNamingHasPrompt(m.pendingPrompt != "")
	m.menu.SetNamingBackend(m.pendingBackend != "")
	m.menu.SetNamingAccount(m.pendingAccount != "")
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.failedCreate = nil
	return true
}
