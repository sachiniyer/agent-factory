package ui

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

func TestAppearanceOverridesAndSystemDetection(t *testing.T) {
	old := lipgloss.HasDarkBackground()
	t.Cleanup(func() { lipgloss.SetHasDarkBackground(old) })
	for _, detected := range []bool{false, true} {
		for _, choice := range []string{"light", "dark", "system", "auto", ""} {
			called := false
			applyAppearance(choice, func() bool { called = true; return detected })
			want := detected
			if choice == "light" {
				want = false
			}
			if choice == "dark" {
				want = true
			}
			require.Equal(t, want, lipgloss.HasDarkBackground())
			require.Equal(t, choice != "light" && choice != "dark", called)
		}
	}
	// With no terminal available, termenv's unreported background is dark.
	require.True(t, termenv.NewOutput(io.Discard).HasDarkBackground())
}

func TestAppearanceHidesRetiredPaletteFromOlderManifest(t *testing.T) {
	pane := NewConfigPane()
	pane.SetEntries([]config.ConfigEntry{{Key: "theme"}, {Key: "theme.accent"}, {Key: "appearance", Value: "system", Type: "string", Tier: 1, TierName: "Essentials", Enum: []string{"light", "dark", "system"}}}, "")
	require.Len(t, pane.entries, 1)
	require.Equal(t, "appearance", pane.entries[0].Key)
}

func TestTerminalBackgroundEnvironmentFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("termenv COLORFGBG fallback is Unix-specific")
	}
	t.Setenv("TERM", "dumb") // OSC unavailable: exercise the documented fallback.
	for _, tc := range []struct {
		value string
		dark  bool
	}{{"15;0", true}, {"0;15", false}, {"", true}, {"invalid", true}} {
		t.Setenv("COLORFGBG", tc.value)
		require.Equal(t, tc.dark, termenv.NewOutput(io.Discard, termenv.WithTTY(true)).HasDarkBackground())
	}
}

// Display labels must not leak into the lower-case persisted enum.
func TestAppearanceConfigEditRoundTrip(t *testing.T) {
	for _, tc := range []struct{ value, label string }{{"light", "Light"}, {"dark", "Dark"}, {"system", "System"}} {
		t.Run(tc.value, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("appearance = 'auto'\n"), 0600))
			pane := newTestConfigPane(t)
			selectKey(t, pane, "appearance")
			pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
			require.True(t, pane.IsEditing())
			require.Equal(t, "system", pane.input.Value())
			pane.input.SetValue(tc.value)
			pane.HandleKeyPress(tea.KeyMsg{Type: tea.KeyEnter})
			require.False(t, pane.IsEditing(), pane.status)
			require.False(t, pane.statusIsError, pane.status)
			cfg, err := config.LoadConfig()
			require.NoError(t, err)
			require.Equal(t, tc.value, cfg.Appearance)
			require.Contains(t, pane.String(), tc.label)
		})
	}
}
