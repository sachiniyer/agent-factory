package app

import (
	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/keys"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// A notice too wide for the status bar ends in a hint naming the key that opens
// it in full. That hint is honest only if pressing the key opens the details,
// and whether it does is a fact about key dispatch, not about which overlay is
// up: an overlay's handler owns the keyboard while it is open, interactive mode
// forwards every key to the agent, and the Projects section swallows keys it
// does not know. The bar used to decide from the naming form alone, so every
// other overlay kept advertising a dead `E details` (#4940).
//
// So one function, noticeDetailsKey, names the key that opens the details in
// the current state. The bar advertises exactly that key, and dispatch opens the
// details on exactly that key, ahead of every other handler, and on no other
// path. A state noticeDetailsKey does not name opens nothing and advertises
// nothing, so a new overlay starts out honest instead of inheriting the hint.

// noticeDetailsKey returns the key that opens the retained notice's details in
// the current state, and false when no key does.
func (m *home) noticeDetailsKey() (key.Binding, bool) {
	switch {
	case m.state == stateNew && m.namingInstance != nil:
		// E is a character the title field types, so the form opens the
		// details on its own ctrl+e (#4123).
		return namingFormDetailsKey, true
	case m.state != stateDefault:
		// Every other state is an overlay or a naming-form field whose own
		// handler takes the keyboard.
		return key.Binding{}, false
	case m.interactive:
		// Every key goes to the agent in the focused pane.
		return key.Binding{}, false
	case m.ring.Active() == layout.RegionProjects:
		// The section is captive: a key it does not route is a no-op (#1620).
		return key.Binding{}, false
	}
	b := keys.GlobalKeyBindings[keys.KeyErrorDetails]
	return b, b.Enabled() && !onlyHardExit(b)
}

// onlyHardExit reports whether every key of b is ctrl+c, which always quits
// before any binding is consulted, so b can never open anything.
func onlyHardExit(b key.Binding) bool {
	for _, k := range b.Keys() {
		if k != "ctrl+c" {
			return false
		}
	}
	return true
}

// noticeDetailsHint is the hint a clipped notice carries in the current state,
// and hide reports that no key opens the details here.
func (m *home) noticeDetailsHint() (hint string, hide bool) {
	b, ok := m.noticeDetailsKey()
	if !ok {
		return "", true
	}
	if h := b.Help(); h.Key != "" && h.Desc != "" {
		return h.Key + " " + h.Desc, false
	}
	return "", false
}

// openNoticeDetails opens the retained notice when msg is the key
// noticeDetailsKey names, and reports whether it did. dispatchKeyAction asks it
// before any state's handler; it is the only way a key opens the details.
func (m *home) openNoticeDetails(msg tea.KeyMsg) (tea.Model, tea.Cmd, bool) {
	b, ok := m.noticeDetailsKey()
	if !ok || msg.String() == "ctrl+c" || !key.Matches(msg, b) {
		return m, nil, false
	}
	fromForm := m.state == stateNew
	mod, cmd := m.showErrorDetails()
	m.namingNotice.returnFromDetails = fromForm && m.state == stateHelp
	return mod, cmd, true
}
