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
	// The menu registers per-hint click zones during render (#1024 PR 6); it
	// sits at the top of the status-bar rect, so that is its zone origin.
	s.menu.SetOrigin(layout.Point{X: r.X, Y: r.Y})
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

// HandleMouse implements layout.Pane. Hint clicks are zone-id-based at the
// root (#1024 PR 6): the menu registers a StatusHint zone per rendered hint
// and the root synthesizes that key, so the pane-local fallback consumes
// nothing.
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
