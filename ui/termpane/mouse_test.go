package termpane

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/vt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sgrMouseModes is what an inner application that wants the mouse emits:
// button-event tracking + SGR extended encoding — the modes tmux mirrors to
// its client when the app inside it (vim, an agent CLI) requests them.
const sgrMouseModes = "\x1b[?1002h\x1b[?1006h"

func mouseMsg(action tea.MouseAction, button tea.MouseButton, x, y int) tea.MouseMsg {
	return tea.MouseMsg{X: x, Y: y, Action: action, Button: button}
}

func sendMouse(emu *vt.Emulator, msg tea.MouseMsg, x, y int) bool {
	ev, ok := translateMouse(msg, x, y)
	if !ok {
		return false
	}
	emu.SendMouse(ev)
	return true
}

// TestMouseForwardingEncodesSGR pins the tea.MouseMsg → PTY-bytes mapping
// (#1024 R4): with the inner app requesting SGR button-event tracking, the
// forwarded press/release/wheel/drag arrive as the exact sequences vim/tmux
// expect, with 1-based coordinates.
func TestMouseForwardingEncodesSGR(t *testing.T) {
	for _, tc := range []struct {
		name string
		msg  tea.MouseMsg
		x, y int
		want string
	}{
		{"left press", mouseMsg(tea.MouseActionPress, tea.MouseButtonLeft, 99, 99), 4, 2, "\x1b[<0;5;3M"},
		{"left release", mouseMsg(tea.MouseActionRelease, tea.MouseButtonLeft, 0, 0), 4, 2, "\x1b[<0;5;3m"},
		{"right press", mouseMsg(tea.MouseActionPress, tea.MouseButtonRight, 0, 0), 0, 0, "\x1b[<2;1;1M"},
		{"wheel up", mouseMsg(tea.MouseActionPress, tea.MouseButtonWheelUp, 0, 0), 10, 5, "\x1b[<64;11;6M"},
		{"wheel down", mouseMsg(tea.MouseActionPress, tea.MouseButtonWheelDown, 0, 0), 10, 5, "\x1b[<65;11;6M"},
		{"drag motion", mouseMsg(tea.MouseActionMotion, tea.MouseButtonLeft, 0, 0), 7, 8, "\x1b[<32;8;9M"},
		{"ctrl+left press", func() tea.MouseMsg {
			m := mouseMsg(tea.MouseActionPress, tea.MouseButtonLeft, 0, 0)
			m.Ctrl = true
			return m
		}(), 1, 1, "\x1b[<16;2;2M"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := emuBytes(t, sgrMouseModes, func(emu *vt.Emulator) {
				require.True(t, sendMouse(emu, tc.msg, tc.x, tc.y))
			})
			assert.Equal(t, tc.want, got,
				"forwarded event must arrive as its SGR sequence with 1-based grid coords")
		})
	}
}

// TestMouseForwardingSuppressedWithoutInnerMouseMode is the ownership rule
// the RFC's in-pane decision rests on (#1024 R4, RFC §2.5): the emulator is
// mode-aware, so an inner app that never requested the mouse receives
// NOTHING — forwarding degrades to suppression, and stray clicks can't leak
// escape sequences into a shell prompt.
func TestMouseForwardingSuppressedWithoutInnerMouseMode(t *testing.T) {
	got := emuBytes(t, "", func(emu *vt.Emulator) {
		require.True(t, sendMouse(emu, mouseMsg(tea.MouseActionPress, tea.MouseButtonLeft, 0, 0), 3, 3),
			"the event translates; the emulator decides it goes nowhere")
		require.True(t, sendMouse(emu, mouseMsg(tea.MouseActionPress, tea.MouseButtonWheelDown, 0, 0), 3, 3))
	})
	assert.Empty(t, got, "no mouse mode set → no bytes reach the PTY")
}

// TestTranslateMouseUnknownButton: buttons with no encoding are refused, not
// guessed — SendKey's ignore contract, extended to the mouse.
func TestTranslateMouseUnknownButton(t *testing.T) {
	msg := mouseMsg(tea.MouseActionPress, tea.MouseButton(99), 0, 0)
	_, ok := translateMouse(msg, 0, 0)
	assert.False(t, ok)
}
