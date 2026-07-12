package ui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/sachiniyer/agent-factory/config"
)

func TestTabbedWindowFrameStyleUsesPaneBorderThemeSlots(t *testing.T) {
	defaultTheme := config.DefaultThemeConfig()
	t.Cleanup(func() { ApplyTheme(defaultTheme) })

	custom := defaultTheme
	custom.PaneBorderDefault = "#111111"
	custom.PaneBorderSelected = "#222222"
	custom.PaneBorderInteractive = "#333333"
	custom.PaneBorderPreview = "#444444"
	custom.Warning = "#555555"
	ApplyTheme(custom)

	assertFrameColor := func(name string, w *TabbedWindow, want string) {
		t.Helper()
		got := w.frameStyle().GetBorderBottomForeground()
		if got != lipgloss.TerminalColor(lipgloss.Color(want)) {
			t.Fatalf("%s border = %v, want %s", name, got, want)
		}
	}

	w := NewTabbedWindow(NewTabPane(previewFromInstance), nil)
	assertFrameColor("default", w, "#111111")

	w.SetSidebarSelected(true)
	assertFrameColor("selected but not focused", w, "#222222")

	w.Focus()
	assertFrameColor("focused nav", w, "#111111")

	w.SetInteractive(true)
	assertFrameColor("interactive", w, "#333333")

	w.SetInteractive(false)
	w.SetPreview(nil, 0, "original")
	assertFrameColor("preview", w, "#444444")

	w.ClearPreview()
	w.SetDropTarget(true)
	assertFrameColor("drop target", w, "#555555")
}
