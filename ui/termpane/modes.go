package termpane

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/terminal"
)

// TerminalModes returns the most recent terminal ownership modes and whether a
// complete authoritative base is known. Live DECSET/DECRST callbacks evolve a
// known base, but cannot create one: observing one changed mode does not prove
// every unmentioned mode was off before this client subscribed.
func (t *TermPane) TerminalModes() (terminal.Modes, bool) {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	return t.terminalModes, t.modesKnown
}

// setTerminalMode mirrors ownership-relevant emulator callbacks into the
// transport-neutral mode snapshot.
func setTerminalMode(m *terminal.Modes, mode ansi.Mode, on bool) {
	switch mode {
	case ansi.ModeMouseNormal:
		m.MouseStandard = on
	case ansi.ModeMouseButtonEvent:
		m.MouseButton = on
	case ansi.ModeMouseAnyEvent:
		m.MouseAll = on
	case ansi.ModeMouseExtUtf8:
		m.MouseUTF8 = on
	case ansi.ModeMouseExtSgr:
		m.MouseSGR = on
	case ansi.ModeMouseX10, ansi.ModeMouseHighlight:
		// tmux has no individual format for these legacy tracking variants.
		// The aggregate MouseTracking field below still preserves ownership.
	default:
		return
	}
}
