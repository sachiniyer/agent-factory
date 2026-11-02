package tmux

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd"
)

func TestReadTerminalState(t *testing.T) {
	dir := t.TempDir()
	// A non-80x24 pane size — a custom `default-size` is exactly the shape the
	// broker's repaint path must not assume away (#4480).
	// 12 fields: cursor_y cursor_x alternate_on mouse_any mouse_standard
	// mouse_button mouse_all mouse_utf8 mouse_sgr pane_width pane_height
	// cursor_flag — the 12th is DECTCEM visibility.
	script := "#!/bin/sh\nprintf '7 11 1 1 0 1 0 0 1 200 60 1\\n'\n"
	if err := os.WriteFile(filepath.Join(dir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	ts := NewTmuxSessionWithDeps("terminal-state", "sh", MakePtyFactory(), cmd.MakeExecutor())

	state, err := ts.ReadTerminalState()
	if err != nil {
		t.Fatalf("ReadTerminalState: %v", err)
	}
	if state.CursorRow != 7 || state.CursorCol != 11 {
		t.Fatalf("cursor = (%d,%d), want (7,11)", state.CursorRow, state.CursorCol)
	}
	if state.PaneWidth != 200 || state.PaneHeight != 60 {
		t.Fatalf("pane size = %dx%d, want 200x60 — the pane's real dimensions must ride the capture", state.PaneWidth, state.PaneHeight)
	}
	if !state.Modes.AlternateScreen || !state.Modes.MouseButton || !state.Modes.MouseSGR {
		t.Fatalf("modes = %+v, want alternate + button tracking + SGR", state.Modes)
	}
	if state.Modes.MouseStandard || state.Modes.MouseAll || state.Modes.MouseUTF8 {
		t.Fatalf("modes = %+v, enabled a flag the snapshot reported off", state.Modes)
	}
	if !state.CursorVisible {
		t.Fatalf("CursorVisible = false, want true — the 12th field (cursor_flag) must ride the capture")
	}
}
