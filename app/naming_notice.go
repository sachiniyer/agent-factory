package app

import (
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

// namingFormDetailsHint is what a clipped notice advertises while the naming
// form has the keyboard, in place of the global `E details`.
const namingFormDetailsHint = "ctrl+e details"

// namingFormNotice pins a notice to the naming form that raised it. instance
// tells a form still open from a new one opened after it.
type namingFormNotice struct {
	id       uint64
	instance *session.Instance
	// returnFromDetails sends the details overlay back to the form on dismissal.
	returnFromDetails bool
}

// handleNamingFormKey is the naming form's key entry point. It opens the
// details on ctrl+e, and pins any notice the form's own handler raises.
func (m *home) handleNamingFormKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.Type == tea.KeyCtrlE && m.namingInstance != nil {
		mod, cmd := m.showErrorDetails()
		m.namingNotice.returnFromDetails = m.state == stateHelp
		return mod, cmd
	}
	before := m.transientNoticeID
	mod, cmd := m.handleStateNew(msg)
	if m.transientNoticeID != before && m.namingInstance != nil {
		m.namingNotice = namingFormNotice{id: m.transientNoticeID, instance: m.namingInstance}
	}
	return mod, cmd
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
	return back
}

// noticeDetailsHint is the details hint for the current state; "" means the
// error_details binding's own.
func (m *home) noticeDetailsHint() string {
	if m.state == stateNew && m.namingInstance != nil {
		return namingFormDetailsHint
	}
	return ""
}
