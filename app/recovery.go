package app

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
)

type recoveryNotice struct{ condition, detail, action string }

func (m *home) showRecovery(condition, detail, action string, err error) tea.Cmd {
	m.recovery = &recoveryNotice{condition, detail, action}
	// The recovery overlay is a stateless modal — it never changes m.state — so
	// the state-keyed clearStaleClickTrackerAfter gate in Update cannot see this
	// excursion. Clear the tracker at the raise boundary instead, so a
	// pre-recovery press cannot pair with a post-recovery press into a false
	// double click (#1731, regressed by the recovery overlay added in 4af1c606).
	// Neither dismissal path re-seeds (both early-return before trackClick), so a
	// single clear here is sufficient.
	m.lastClickZone = ""
	m.lastClickAt = time.Time{}
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
	m.pendingAccountChosen = true
	m.menu.SetNamingHasPrompt(m.pendingPrompt != "")
	m.menu.SetNamingBackend(m.pendingBackend != "")
	m.menu.SetNamingAccount(m.pendingAccount != "")
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.failedCreate = nil
	return true
}
