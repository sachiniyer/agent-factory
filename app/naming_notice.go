package app

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/session"
)

// A notice the naming form raises — a title conflict, a missing program —
// explains why the form refused the input it is still holding. It used to
// expire after 3 seconds like any other notice, and the key that reopens an
// expired notice, E, is a character the title field types. The only way to
// read why the form refused was to discard the form (#4123).
//
// So a notice the form raised stays on the bar for as long as that form is
// open, and inside the form ctrl+e opens it in full and returns to the form.
// ctrl+e is form-local, like ctrl+r and ctrl+o: the title field has no cursor,
// so readline's end-of-line has nothing to do there.

// namingFormDetailsKey is the key that opens a clipped notice while the naming
// form has the keyboard, in place of the global `E details`.
var namingFormDetailsKey = key.NewBinding(key.WithKeys("ctrl+e"), key.WithHelp("ctrl+e", "details"))

// namingFormNotice pins a notice to the naming form that raised it. instance
// tells a form still open from a new one opened after it.
type namingFormNotice struct {
	id       uint64
	instance *session.Instance
	// returnFromDetails sends the details overlay back to the form on dismissal.
	returnFromDetails bool
	// deferred holds the form's daemon replies that arrived while its details
	// overlay was open, to be replayed once the form is back.
	deferred []tea.Msg
}

// handleNamingFormKey is the naming form's key entry point. It pins any notice
// the form's own handler raises; ctrl+e never gets here, because
// openNoticeDetails claims it first (notice_details.go).
func (m *home) handleNamingFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	return m.pinningNamingNotice(func() (tea.Model, tea.Cmd) { return m.handleStateNew(msg) })
}

// pinningNamingNotice runs a handler that belongs to the naming form and pins
// any notice it raised, if the form is open once it returns. Every handler that
// can raise a notice onto the form goes through here — the form's own keys, its
// nested fields (program, backend, account, prompt), and the daemon answers
// those fields wait on — because a nested field refuses a pick by closing back
// to the form and raising the reason, and that reason must not expire either.
func (m *home) pinningNamingNotice(handle func() (tea.Model, tea.Cmd)) (tea.Model, tea.Cmd) {
	before := m.transientNoticeID
	mod, cmd := handle()
	if m.transientNoticeID != before && m.namingInstance != nil {
		m.namingNotice.id, m.namingNotice.instance = m.transientNoticeID, m.namingInstance
	}
	return mod, cmd
}

// deferNamingReply holds a daemon reply for the naming form that lands while
// the form's details overlay is open, and reports whether it did. Each reply's
// handler drops it unless the form has the keyboard, so without this a ctrl+e
// at the wrong moment loses the answer — and a backend picker promised by the
// new_remote binding stays pending, with the form answering "Loading backends…"
// to every submit.
func (m *home) deferNamingReply(msg tea.Msg, naming *session.Instance) bool {
	if m.state != stateHelp || !m.namingNotice.returnFromDetails ||
		naming == nil || naming != m.namingInstance {
		return false
	}
	m.namingNotice.deferred = append(m.namingNotice.deferred, msg)
	return true
}

// namingNoticePinned reports whether noticeID was raised by the naming form
// that is still open, and so must stay on the bar.
func (m *home) namingNoticePinned(noticeID uint64) bool {
	n := m.namingNotice
	return n.id == noticeID && n.instance != nil && n.instance == m.namingInstance
}

// returnToNamingFormAfterDetails reports, once, whether a closing details
// overlay was opened from a naming form that is still open.
func (m *home) returnToNamingFormAfterDetails() bool {
	back := m.namingNotice.returnFromDetails && m.namingInstance != nil
	m.namingNotice.returnFromDetails = false
	if !back {
		m.namingNotice.deferred = nil
	}
	return back
}

// replayDeferredNamingReplies delivers, in arrival order, the replies held
// while the form's details overlay was open. It runs once the form has the
// keyboard again, synchronously, so no key can reach the form ahead of them.
func (m *home) replayDeferredNamingReplies() tea.Cmd {
	deferred := m.namingNotice.deferred
	m.namingNotice.deferred = nil
	cmds := make([]tea.Cmd, 0, len(deferred))
	for _, msg := range deferred {
		_, cmd := m.Update(msg)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}
