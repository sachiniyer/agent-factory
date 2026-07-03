package ui

import (
	"github.com/sachiniyer/agent-factory/ui/layout"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// StatusBar is the bottom bar of the workspace (RFC §2.1): the context-
// sensitive key hints (the former standalone Menu row) over the error line
// (the former standalone ErrBox row), merged into one layout region (#1024
// PR 4). It wraps — rather than reimplements — the existing Menu and ErrBox so
// their per-state option logic, keydown highlight animation, and hard-won
// error sanitization carry over verbatim; the root model keeps direct handles
// to both for SetState/SetError calls.
//
// It implements layout.Pane; it never takes ring focus (hints follow whichever
// pane does).
type StatusBar struct {
	menu   *Menu
	errBox *ErrBox
	rect   layout.Rect
}

// NewStatusBar wraps the given menu and error box.
func NewStatusBar(menu *Menu, errBox *ErrBox) *StatusBar {
	return &StatusBar{menu: menu, errBox: errBox}
}

// SetRect implements layout.Pane: the hint row(s) take everything above the
// final line, which is the error line.
func (s *StatusBar) SetRect(r layout.Rect) {
	s.rect = r
	menuRows := r.H - 1
	if menuRows < 0 {
		menuRows = 0
	}
	s.menu.SetSize(r.W, menuRows)
	s.errBox.SetSize(r.W, 1)
}

// Focused implements layout.Pane; the status bar is never focusable.
func (s *StatusBar) Focused() bool { return false }

// Focus implements layout.Pane (no-op).
func (s *StatusBar) Focus() {}

// Blur implements layout.Pane (no-op).
func (s *StatusBar) Blur() {}

// HandleKey implements layout.Pane (never consumes).
func (s *StatusBar) HandleKey(tea.KeyMsg) (tea.Cmd, bool) { return nil, false }

// HandleMouse implements layout.Pane. Clickable hints are #1024 PR 6.
func (s *StatusBar) HandleMouse(tea.MouseMsg, layout.Point) tea.Cmd { return nil }

// View implements layout.Pane: exactly rect-sized.
func (s *StatusBar) View() string { return s.String() }

func (s *StatusBar) String() string {
	if s.rect.Empty() {
		return ""
	}
	out := lipgloss.JoinVertical(lipgloss.Left, s.menu.String(), s.errBox.String())
	return layout.ClampToRect(out, s.rect)
}
