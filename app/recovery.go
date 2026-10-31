package app

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
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
	m.pendingBackend, m.pendingHost, m.pendingAccount = req.Backend, req.Host, req.Account
	// The wire values can't tell a user choice from a derived one: a program
	// the account-pairing pinned is indistinguishable from a pick, and a
	// deliberate "Ambient identity" is indistinguishable from an untouched
	// field. The draft carries the original chosen flags for exactly this
	// (#4889 review): restore the flags so an account-pinned program goes back
	// to config-following.
	//
	// Only a NAMED account survives as chosen: a chosen-ambient draft has
	// AccountChosen but Account "", which sendProgramForCreate does not pin
	// and the retry sends as ""+"" — daemon-side resolution. Keeping the flag
	// would skip the refreshes below and leave the display describing defaults
	// that may already have moved, so the flag records the pinning decision —
	// "a named account was chosen" — which is the only decision the wire can
	// still see (#4889 review).
	m.pendingAccountChosen = req.AccountChosen && req.Account != ""
	m.pendingProgramChosen = req.ProgramChosen
	if !m.pendingProgramChosen && !m.pendingAccountChosen {
		if isRemoteTarget() {
			// pendingProgram already holds the placeholder's remote-resolved
			// seed; the local config read cannot see the daemon host's
			// default_program, so re-ask ListPrograms and gate submit on the
			// answer — the same contract a fresh remote form gets (#4889
			// review).
			m.remoteDefaultPending = true
			m.menu.SetNamingHasPrompt(strings.TrimSpace(m.pendingPrompt) != "")
			m.menu.SetNamingBackend(m.backendPicked())
			m.menu.SetNamingAccount(m.pendingAccount != "")
			m.state = stateNew
			m.menu.SetState(ui.StateNewInstance)
			m.failedCreate = nil
			return true, tea.Batch(
				m.fetchAccountDefault(instance, sessionenv.AgentForCommand(m.pendingProgram)),
				m.fetchRemoteDefaultProgram(instance))
		}
		// Re-resolve the live default for the restored field's seed: the
		// displayed program is what a retry's implicit "" will resolve to, so
		// it should be the CURRENT default, not the stale one the failed form
		// happened to show (#4889 review).
		//
		// A CHOSEN named account forbids the re-resolve: the wire program was
		// the displayed one pinned to keep the pair, so instance.Program is
		// the agent that account's registry belongs to. Refreshing it onto a
		// moved default would re-pair (for example) claude's "work" account
		// with codex on the retry — silently wrong or a registry mismatch
		// (#4889 review).
		m.refreshDefaultProgram()
		m.pendingProgram = m.defaultProgram()
	}
	m.menu.SetNamingHasPrompt(strings.TrimSpace(m.pendingPrompt) != "")
	m.menu.SetNamingBackend(m.backendPicked())
	m.menu.SetNamingAccount(m.pendingAccount != "")
	m.state = stateNew
	m.menu.SetState(ui.StateNewInstance)
	m.failedCreate = nil
	if m.pendingAccountChosen {
		return true, nil
	}
	// The draft proves only that no account was SENT — an unchosen preselect
	// and a chosen ambient identity both leave req.Account empty, so the
	// restored field would read "no account" while the retry applies the
	// configured default. Re-fetch it exactly as a fresh form does (#4889
	// review).
	return true, m.fetchAccountDefault(instance, sessionenv.AgentForCommand(m.pendingProgram))
}
