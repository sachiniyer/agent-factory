package termpane

import (
	"github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/terminal"
)

// terminalModesAuthority is the complete ownership-snapshot state machine. A
// single enum prevents independent "known", "connected", and cursor-coverage
// booleans from drifting into combinations that falsely advertise an owner.
type terminalModesAuthority uint8

const (
	terminalModesUnknown terminalModesAuthority = iota
	// terminalModesDisconnected retains an authoritative base only so an exact
	// replay can continue evolving it. It is never exposed to input routing.
	terminalModesDisconnected
	terminalModesCurrent
	// terminalModesCursorCovered is current and permits exactly one recovery
	// cursor re-seed immediately after the authoritative repaint that established it.
	terminalModesCursorCovered
)

// TerminalModes returns the most recent terminal ownership modes and whether a
// complete authoritative base is known. Live DECSET/DECRST callbacks evolve a
// known base, but cannot create one: observing one changed mode does not prove
// every unmentioned mode was off before this client subscribed.
func (t *TermPane) TerminalModes() (terminal.Modes, bool) {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	known := t.modeAuthority == terminalModesCurrent ||
		t.modeAuthority == terminalModesCursorCovered
	return t.terminalModes, known
}

// installTerminalModesLocked makes one repaint the authoritative base for every
// ownership mode. It also covers a cursor re-seed delivered immediately after
// that repaint (the broker's recovery ordering is repaint -> cursor -> data).
func (t *TermPane) installTerminalModesLocked(modes terminal.Modes) {
	t.terminalModes = modes
	t.modeAuthority = terminalModesCursorCovered
}

// invalidateTerminalModesLocked is the single authority-loss transition. The
// last values remain for a seamless replay to evolve, but callers cannot use
// them as an ownership decision until another complete snapshot establishes a
// base.
func (t *TermPane) invalidateTerminalModesLocked() {
	t.modeAuthority = terminalModesUnknown
}

// connectTerminalModesLocked makes a retained base public only when the broker
// accepted the exact cursor. A clamp crossed bytes that may contain mode changes,
// so it discards authority until a fresh metadata-bearing repaint arrives.
func (t *TermPane) connectTerminalModesLocked(exactReplay bool) {
	if !exactReplay {
		t.invalidateTerminalModesLocked()
		return
	}
	if t.modeAuthority == terminalModesDisconnected {
		t.modeAuthority = terminalModesCurrent
	}
}

func (t *TermPane) disconnectTerminalModesLocked() {
	switch t.modeAuthority {
	case terminalModesCurrent, terminalModesCursorCovered:
		t.modeAuthority = terminalModesDisconnected
	}
}

func (t *TermPane) observeTerminalDataLocked() {
	if t.modeAuthority == terminalModesCursorCovered {
		t.modeAuthority = terminalModesCurrent
	}
}

// observeTerminalCursorLocked accepts only the broker's opening hello and the
// one recovery cursor covered by an immediately preceding authoritative repaint.
// Every other jump may cross unretained DECSET/DECRST bytes and fails closed.
func (t *TermPane) observeTerminalCursorLocked(opening bool) {
	if opening {
		return
	}
	if t.modeAuthority == terminalModesCursorCovered {
		t.modeAuthority = terminalModesCurrent
		return
	}
	t.invalidateTerminalModesLocked()
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
