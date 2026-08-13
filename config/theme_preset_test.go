package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestThemePresets(t *testing.T) {
	t.Run("nord is the default and zenburn remains selectable", func(t *testing.T) {
		assert.Equal(t, "#2E3440", DefaultThemeConfig().Background)
		assert.Equal(t, "#88C0D0", DefaultThemeConfig().Accent)

		zenburn, ok := ThemePreset("zenburn")
		require.True(t, ok)
		assert.Equal(t, ThemeConfig{
			Foreground:            "#DCDCCC",
			ForegroundStrong:      "#FFFFEF",
			ForegroundMuted:       "#989890",
			ForegroundDim:         "#656555",
			Background:            "#3F3F3F",
			BackgroundSubtle:      "#494949",
			BackgroundPanel:       "#4F4F4F",
			Accent:                "#8CD0D3",
			Success:               "#7F9F7F",
			Warning:               "#F0DFAF",
			Error:                 "#CC9393",
			Info:                  "#93E0E3",
			Purple:                "#DC8CC3",
			SelectionBackground:   "#4F4F4F",
			SelectionForeground:   "#FFFFEF",
			PaneBorderDefault:     "#989890",
			PaneBorderSelected:    "#8CD0D3",
			PaneBorderInteractive: "#7F9F7F",
			PaneBorderPreview:     "#DC8CC3",
			preset:                "zenburn",
			explicitPreset:        true,
		}, zenburn)
	})

	t.Run("the source contract still has nineteen color slots", func(t *testing.T) {
		assert.Equal(t, 19, ThemeSlotCount())
	})
}

func TestThemePresetTOML(t *testing.T) {
	t.Run("loads a named preset", func(t *testing.T) {
		cfg, err := parseConfigTOML([]byte("theme = \"zenburn\"\n"), "config.toml")
		require.NoError(t, err)
		assert.Equal(t, "zenburn", cfg.Theme.Preset())
		assert.Equal(t, "#3F3F3F", cfg.Theme.Background)
		assert.Equal(t, "#8CD0D3", cfg.Theme.Accent)
	})

	t.Run("rejects an unknown named preset", func(t *testing.T) {
		_, err := parseConfigTOML([]byte("theme = \"nrod\"\n"), "config.toml")
		require.Error(t, err)
		assert.Contains(t, err.Error(), `unknown theme preset "nrod"`)
		assert.Contains(t, err.Error(), "nord, zenburn")
	})

	t.Run("custom tables overlay nord without becoming a named preset", func(t *testing.T) {
		cfg, err := parseConfigTOML([]byte("[theme]\naccent = \"#010203\"\n"), "config.toml")
		require.NoError(t, err)
		assert.Empty(t, cfg.Theme.Preset())
		assert.Equal(t, "#010203", cfg.Theme.Accent)
		assert.Equal(t, "#2E3440", cfg.Theme.Background)
	})
}

func TestNamedThemeSurvivesConfigSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, TomlConfigFileName), []byte("theme = \"zenburn\"\n"), 0o644))

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.NoError(t, SaveConfig(cfg))

	written, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(written), "theme = 'zenburn'")
	assert.NotContains(t, string(written), "[theme]")

	reloaded, err := LoadConfig()
	require.NoError(t, err)
	assert.Equal(t, "zenburn", reloaded.Theme.Preset())
	assert.Equal(t, "#3F3F3F", reloaded.Theme.Background)
}

func TestDefaultThemeSaveRemainsReadableByLegacyDecoders(t *testing.T) {
	written, err := marshalConfigTOML(DefaultConfig())
	require.NoError(t, err)
	assert.Contains(t, string(written), "[theme]")
	assert.NotContains(t, string(written), "theme = 'nord'")

	var legacy struct {
		Theme struct {
			Background string `toml:"background"`
			Accent     string `toml:"accent"`
		} `toml:"theme"`
	}
	require.NoError(t, toml.Unmarshal(written, &legacy))
	assert.Equal(t, "#2E3440", legacy.Theme.Background)
	assert.Equal(t, "#88C0D0", legacy.Theme.Accent)
}

func TestCustomThemeSurvivesConfigSave(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	fastShell(t)
	require.NoError(t, os.WriteFile(
		filepath.Join(home, TomlConfigFileName),
		[]byte("[theme]\naccent = \"#010203\"\n"),
		0o644,
	))

	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Empty(t, cfg.Theme.Preset())
	require.NoError(t, SaveConfig(cfg))

	written, err := os.ReadFile(filepath.Join(home, TomlConfigFileName))
	require.NoError(t, err)
	assert.Contains(t, string(written), "[theme]")
	assert.Contains(t, string(written), "accent = '#010203'")
	assert.NotContains(t, string(written), "theme = 'nord'")

	reloaded, err := LoadConfig()
	require.NoError(t, err)
	assert.Empty(t, reloaded.Theme.Preset())
	assert.Equal(t, "#010203", reloaded.Theme.Accent)
	assert.Equal(t, "#2E3440", reloaded.Theme.Background)
}
