package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
// The returned cmd re-fetches the account the daemon would apply to an implicit
// submit — the draft carries only the WIRE account, which is "" for an unchosen
// preselect (#4889 review) — so a restored form must not claim "no account" for
// a retry that will run as the configured default.
func (m *home) restoreFailedCreate() (bool, tea.Cmd) {
	failed := m.failedCreate
	if failed == nil || failed.draft == nil || failed.draft.RepoPath != m.repoRoot || m.namingInstance != nil {
		return false, nil
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
	// The wire values can't tell a user choice from a derived one: a program
	// the account-pairing pinned is indistinguishable from a pick, and a
	// deliberate "Ambient identity" is indistinguishable from an untouched
	// field. The draft carries the original chosen flags for exactly this
	// (#4889 review): restore the flags so an account-pinned program goes back
	// to config-following and a chosen ambient account is not overwritten by
	// the refetched default.
	m.pendingAccountChosen = req.AccountChosen
	m.pendingProgramChosen = req.ProgramChosen
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
	if m.pendingAccountChosen {
		return true, nil
	}
	// The draft proves only that no account was SENT — an unchosen preselect
	// leaves req.Account empty, so the restored field would read "no account"
	// while the retry applies the configured default. Re-fetch it exactly as a
	// fresh form does (#4889 review).
	return true, m.fetchAccountDefault(instance, sessionenv.AgentForCommand(m.pendingProgram))
}
