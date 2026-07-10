package termpane

import (
	tea "github.com/charmbracelet/bubbletea"
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
// point already is. It reports whether the event had a translation; false
// means the event type has no encoding and was ignored (never a guessed
// sequence), mirroring SendKey's contract. Whether a translated event
// actually reaches the inner app is the emulator's mode-aware call. The lock
// is required even though this only writes input: the encoder reads terminal
// modes (mouse tracking, SGR encoding) that the PTY reader pump mutates
// through emu.Write.
func (t *TermPane) SendMouse(msg tea.MouseMsg, x, y int) bool {
	ev, ok := translateMouse(msg, x, t.mouseGridY(y))
	if !ok {
		return false
	}
	t.gridMu.RLock()
	defer t.gridMu.RUnlock()
	t.emu.SendMouse(ev)
	return true
}

// mouseGridY maps a zone-local content row to the emulator grid row. With the
// status bar at the top, Render draws the visible window starting at
// sourceY=statusRows so the status rows stay hidden; a forwarded click's y must
// shift down by that same hidden-row count, or the first statusRows visible
// rows would send events into the hidden status area instead of the row the
// user actually clicked (#1534). With the status bar at the bottom the hidden
// rows are past the visible window, so no shift applies. statusPosition and
// statusRows are set once at construction, so this needs no grid lock. Mirrors
// Render's sourceY.
func (t *TermPane) mouseGridY(y int) int {
	if t.statusPosition == statusTop {
		return y + t.statusRows
	}
	return y
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
