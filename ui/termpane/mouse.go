package termpane

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// Mouse forwarding into the embedded terminal (#1024 R4, RFC §2.5): while
// interactive, mouse events over the pane's terminal grid forward through the
// emulator's SendMouse — which is MODE-AWARE: it encodes and writes to the
// PTY only when the inner application enabled a mouse tracking mode (tmux
// mirrors the inner app's request onto its client), and no-ops otherwise. So
// forwarding degrades to suppression exactly when the inner app doesn't want
// the mouse, and the host never has to guess who owns the wheel.

// SendMouse forwards one mouse event to the embedded terminal at grid cell
// (x, y) — pane-content-local coordinates, which the zone registry's local
// point already is. It reports whether the event was forwarded; false means it
// was ignored — either the event type has no encoding (never a guessed
// sequence, mirroring SendKey's contract) or it fell outside the live grid
// during a resize gap. Whether a forwarded event actually reaches the inner app
// is the emulator's mode-aware call. The lock is required even though this only
// writes input: the encoder reads terminal modes (mouse tracking, SGR encoding)
// that the PTY reader pump mutates through emu.Write.
func (t *TermPane) SendMouse(msg tea.MouseMsg, x, y int) bool {
	ev, ok := translateMouse(msg, x, y)
	if !ok {
		return false
	}
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	// During a resize gap the pane zone can grow before the emulator is resized to
	// match, so a click in the not-yet-propagated region can land past the current
	// grid. Forwarding it would encode a bogus row/col the inner app never drew, so
	// drop any event outside the live grid instead (#1534). The bounds read the
	// emulator under the same lock SendMouse already holds. The streamed bytes are
	// the pane itself, so grid row == content row (no status offset).
	if y < 0 || y >= t.emu.Height() || x < 0 || x >= t.emu.Width() {
		return false
	}
	t.emu.SendMouse(ev)
	return true
}

// MouseTrackingEnabled reports whether the inner application has requested mouse
// reporting — i.e. it has a mouse-tracking DECMode active (X10 9, normal 1000,
// highlight 1001, button-event 1002, or any-event 1003). This is exactly the set
// emu.SendMouse consults before it encodes anything, so a true here means a
// forwarded event would actually reach the program. The host uses it to decide
// who owns the mouse WHEEL (tmux semantics): a program that has NOT enabled
// tracking leaves the wheel to pane scrollback (#1024 wheel fix). The read is
// under the same lock the mode-change callbacks write beneath.
func (t *TermPane) MouseTrackingEnabled() bool {
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	return len(t.mouseModes) > 0
}

// isMouseTrackingMode reports whether mode is one of the DECModes that makes the
// terminal report mouse events to the inner app. It mirrors the tracking-mode
// list emu.SendMouse iterates; the SGR encoding mode (1006) is deliberately
// excluded — it only changes how reports are encoded, not whether they happen.
func isMouseTrackingMode(mode ansi.Mode) bool {
	switch mode {
	case ansi.ModeMouseX10,
		ansi.ModeMouseNormal,
		ansi.ModeMouseHighlight,
		ansi.ModeMouseButtonEvent,
		ansi.ModeMouseAnyEvent:
		return true
	}
	return false
}

// translateMouse maps a bubbletea v1 mouse message to the emulator's event
// model at grid cell (x, y). Wheel presses become wheel events; other presses
// clicks; releases and (drag) motions their own kinds — the same taxonomy
// the SGR protocol distinguishes.
func translateMouse(msg tea.MouseMsg, x, y int) (vt.Mouse, bool) {
	btn, ok := translateMouseButton(msg.Button)
	if !ok {
		return nil, false
	}
	m := vt.MouseClick{X: x, Y: y, Button: btn, Mod: translateMouseMod(msg)}
	switch msg.Action {
	case tea.MouseActionPress:
		if isWheelButton(btn) {
			return vt.MouseWheel(m), true
		}
		return m, true
	case tea.MouseActionRelease:
		return vt.MouseRelease(m), true
	case tea.MouseActionMotion:
		return vt.MouseMotion(m), true
	}
	return nil, false
}

// translateMouseButton maps the bubbletea button to the emulator's X11-based
// codes. ok is false for buttons the encoder doesn't support.
func translateMouseButton(b tea.MouseButton) (vt.MouseButton, bool) {
	switch b {
	case tea.MouseButtonNone:
		return vt.MouseNone, true
	case tea.MouseButtonLeft:
		return vt.MouseLeft, true
	case tea.MouseButtonMiddle:
		return vt.MouseMiddle, true
	case tea.MouseButtonRight:
		return vt.MouseRight, true
	case tea.MouseButtonWheelUp:
		return vt.MouseWheelUp, true
	case tea.MouseButtonWheelDown:
		return vt.MouseWheelDown, true
	case tea.MouseButtonWheelLeft:
		return vt.MouseWheelLeft, true
	case tea.MouseButtonWheelRight:
		return vt.MouseWheelRight, true
	case tea.MouseButtonBackward:
		return vt.MouseBackward, true
	case tea.MouseButtonForward:
		return vt.MouseForward, true
	}
	return vt.MouseNone, false
}

func isWheelButton(b vt.MouseButton) bool {
	switch b {
	case vt.MouseWheelUp, vt.MouseWheelDown, vt.MouseWheelLeft, vt.MouseWheelRight:
		return true
	}
	return false
}

func translateMouseMod(msg tea.MouseMsg) vt.KeyMod {
	var mod vt.KeyMod
	if msg.Shift {
		mod |= vt.ModShift
	}
	if msg.Alt {
		mod |= vt.ModAlt
	}
	if msg.Ctrl {
		mod |= vt.ModCtrl
	}
	return mod
}
