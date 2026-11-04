package app

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/ui"
)

// hostCheckDoneMsg delivers a finished host probe back to the UI loop. name is
// the recorded request — the host, "" for check-all — so the pane can rest
// the busy mark the request set.
type hostCheckDoneMsg struct {
	generation uint64
	name       string
	response   daemon.CheckHostResponse
	err        error
}

// hostCheckCmd runs the pane's recorded probe request off the Update goroutine
// — the one piece of the Hosts section the pane cannot run itself: CheckHost
// is a dial (an ssh probe with a 30s bound), not the bounded socket call the
// config writes are. The probe is performed by CheckHostForEditor, which asks
// the daemon this af session is attached to — never this machine's daemon for
// a remote target.
func (m *home) hostCheckCmd(name string) tea.Cmd {
	generation := m.hostCheckGeneration
	check := ui.CheckHostForEditor
	return func() tea.Msg {
		response, err := check(name)
		return hostCheckDoneMsg{generation: generation, name: name, response: response, err: err}
	}
}

// handleHostCheckDone applies a finished probe on the UI loop. A probe asked
// for in an earlier opening of the overlay is discarded the same way the
// accounts and quota reads are: SetFocus(false) already retired that pane's
// busy marks, and writing a verdict it no longer expects would resurrect a
// "probing…" the user is not looking at.
func (m *home) handleHostCheckDone(msg hostCheckDoneMsg) {
	if msg.generation != m.hostCheckGeneration || m.state != stateConfigEditor || !m.configPane.HasFocus() {
		return
	}
	m.configPane.SetHostCheckResults(msg.name, msg.response.Hosts, msg.err)
}
