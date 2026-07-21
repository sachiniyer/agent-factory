package tmux

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/sachiniyer/agent-factory/terminal"
)

// TerminalState is the part of a tmux pane snapshot capture-pane cannot carry:
// the real cursor and the ownership-affecting terminal modes. Applications can
// switch these modes without repainting them for a later subscriber, so the WS
// stream snapshots them explicitly.
type TerminalState struct {
	CursorRow int
	CursorCol int
	Modes     terminal.Modes
}

const terminalStateFormat = "#{cursor_y} #{cursor_x} #{alternate_on} #{mouse_any_flag} #{mouse_standard_flag} #{mouse_button_flag} #{mouse_all_flag} #{mouse_utf8_flag} #{mouse_sgr_flag}"

// ReadTerminalState reads cursor, alternate-screen, mouse tracking, and mouse
// encoding in one bounded tmux request. One display-message keeps those fields
// from describing different instants and avoids multiplying subscribe latency.
func (t *TmuxSession) ReadTerminalState() (TerminalState, error) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	output, err := t.outputTmuxBounded(ctx, "display-message", "-p", "-t", exactTarget(t.sanitizedName), terminalStateFormat)
	if err != nil {
		if ctx.Err() != nil {
			return TerminalState{}, fmt.Errorf("%w: terminal-state display-message after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return TerminalState{}, fmt.Errorf("failed to read tmux terminal state: %v", err)
	}
	fields := strings.Fields(string(output))
	if len(fields) != 9 {
		return TerminalState{}, fmt.Errorf("failed to parse tmux terminal state %q: want 9 fields, got %d", string(output), len(fields))
	}
	values := make([]int, len(fields))
	for i, field := range fields {
		values[i], err = strconv.Atoi(field)
		if err != nil {
			return TerminalState{}, fmt.Errorf("failed to parse tmux terminal state %q: field %d: %v", string(output), i, err)
		}
	}
	return TerminalState{
		CursorRow: values[0],
		CursorCol: values[1],
		Modes: terminal.Modes{
			AlternateScreen: values[2] != 0,
			MouseTracking:   values[3] != 0,
			MouseStandard:   values[4] != 0,
			MouseButton:     values[5] != 0,
			MouseAll:        values[6] != 0,
			MouseUTF8:       values[7] != 0,
			MouseSGR:        values[8] != 0,
		},
	}, nil
}
