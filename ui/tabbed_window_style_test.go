package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

func TestTabbedWindowFrameStyleUsesPaneBorderThemeSlots(t *testing.T) {
	defaultTheme := config.DefaultThemeConfig()
	t.Cleanup(func() { ApplyTheme(defaultTheme) })

	c := theme.Colors()

	assertFrameColor := func(name string, w *TabbedWindow, want lipgloss.TerminalColor) {
		t.Helper()
		got := w.frameStyle().GetBorderBottomForeground()
		if got != want {
			t.Fatalf("%s border = %v, want %v", name, got, want)
		}
	}

	w := NewTabbedWindow(NewTabPane(previewFromInstance), nil)
	assertFrameColor("default", w, c["border"])

	w.SetSidebarSelected(true)
	assertFrameColor("selected but not focused", w, c["accent"])

	w.Focus()
	assertFrameColor("focused nav", w, c["border"])

	w.SetInteractive(true)
	assertFrameColor("interactive", w, c["accent"])

	w.SetInteractive(false)
	w.SetPreview(nil, 0, "original")
	assertFrameColor("preview", w, c["border"])

	w.ClearPreview()
	w.SetDropTarget(true)
	assertFrameColor("drop target", w, c["accent"])
}
