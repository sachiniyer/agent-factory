package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/stretchr/testify/require"
)

func TestPaletteRetirementMigration(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"light", "theme = 'light'\n", "light"},
		{"dark", "theme = 'dark'\n", "dark"},
		{"auto", "theme = 'auto'\n", "system"},
		{"system", "theme = 'system'\n", "system"},
		{"nord", "theme = 'nord'\n", "system"},
		{"zenburn", "theme = 'zenburn'\n", "system"},
		{"custom", "[theme]\naccent = '#112233' # keep inline\n# keep final\n", "system"},
		{"inline", "theme = { accent = '#112233' }\n", "system"},
		{"dotted", "theme.accent = '#112233'\n", "system"},
		{"quoted", "'theme'.'accent' = '#112233'\n", "system"},
		{"nested", "[theme.future]\nvalue = 'retired'\n", "system"},
		{"appearance_auto", "appearance = 'auto' # keep inline\n", "system"},
		{"appearance_wins", "appearance = 'light'\ntheme = 'dark'\n", "light"},
		{"system_wins", "appearance = 'system'\ntheme = 'light'\n", "system"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			original := "# keep header\nfuture_option = 'untouched'\n" + tc.body + "\n# keep next\n[future]\nvalue = 'unchanged'\n"
			path := writeTempConfig(t, original)
			var warnings bytes.Buffer
			old := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(old) })
			ro, err := LoadConfigReadOnly()
			require.NoError(t, err)
			require.Equal(t, tc.want, ro.Config.Appearance)
			got, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, string(got))
			entries, err := os.ReadDir(filepath.Dir(path))
			require.NoError(t, err)
			require.Len(t, entries, 1, "diagnostics must not create even a lock")
			cfg, err := LoadConfig()
			require.NoError(t, err)
			require.Equal(t, tc.want, cfg.Appearance)
			first, err := os.ReadFile(path)
			require.NoError(t, err)
			var shape map[string]any
			require.NoError(t, toml.Unmarshal(first, &shape))
			require.NotContains(t, shape, "theme")
			require.Equal(t, tc.want, shape["appearance"])
			for _, part := range []string{"# keep header", "future_option = 'untouched'", "# keep next", "[future]\nvalue = 'unchanged'"} {
				require.Contains(t, string(first), part)
			}
			if strings.Contains(tc.body, "# keep inline") {
				require.Contains(t, string(first), "# keep inline")
			}
			if strings.Contains(tc.body, "# keep final") {
				require.Contains(t, string(first), "# keep final")
			}
			require.Contains(t, warnings.String(), "appearance")
			require.Contains(t, warnings.String(), "migrat")
			warnings.Reset()
			_, err = LoadConfig()
			require.NoError(t, err)
			second, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, first, second)
			require.NotContains(t, warnings.String(), "migrat")
		})
	}
}

func TestPaletteRetirementJSON(t *testing.T) {
	for _, raw := range []string{`"light"`, `"dark"`, `"auto"`, `"system"`, `"nord"`, `"zenburn"`, `{"accent":"#123456"}`} {
		t.Run(raw, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", dir)
			path := filepath.Join(dir, ConfigFileName)
			original := []byte(`{"theme":` + raw + `,"future_option":{"keep":42}}`)
			require.NoError(t, os.WriteFile(path, original, 0600))
			want := "system"
			if raw == `"light"` {
				want = "light"
			}
			if raw == `"dark"` {
				want = "dark"
			}
			ro, err := LoadConfigReadOnly()
			require.NoError(t, err)
			require.Equal(t, want, ro.Config.Appearance)
			data, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, original, data)
			cfg, err := LoadConfig()
			require.NoError(t, err)
			require.Equal(t, want, cfg.Appearance)
			data, err = os.ReadFile(filepath.Join(dir, TomlConfigFileName))
			require.NoError(t, err)
			var shape map[string]any
			require.NoError(t, toml.Unmarshal(data, &shape))
			require.NotContains(t, shape, "theme")
			require.Equal(t, want, shape["appearance"])
			require.Contains(t, shape, "future_option")
		})
	}
}

func TestPaletteRetirementSymlink(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	target := filepath.Join(t.TempDir(), "dotfiles.toml")
	require.NoError(t, os.WriteFile(target, []byte("theme = 'dark'\n"), 0600))
	path := filepath.Join(dir, TomlConfigFileName)
	require.NoError(t, os.Symlink(target, path))
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "dark", cfg.Appearance)
	link, err := os.Readlink(path)
	require.NoError(t, err)
	require.Equal(t, target, link)
	data, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Contains(t, string(data), "appearance = 'dark'")
}

func TestPaletteRetirementManifestAndWrites(t *testing.T) {
	writeTempConfig(t, "appearance = 'system'\n")
	for _, entry := range Manifest() {
		require.False(t, entry.Key == "theme" || strings.HasPrefix(entry.Key, "theme."), entry.Key)
	}
	for _, key := range []string{"theme", "theme.accent", "theme.background"} {
		_, err := SetGlobalConfigValue(key, "dark")
		require.ErrorContains(t, err, "retired")
		require.ErrorContains(t, err, "appearance")
		_, err = UnsetGlobalConfigValue(key)
		require.ErrorContains(t, err, "retired")
		require.ErrorContains(t, err, "appearance")
	}
}

func TestPaletteRetirementPreservation(t *testing.T) {
	cases := []struct{ name, source, want, keep string }{
		{"auto_precedence", "appearance = 'auto'\ntheme = 'dark'\n", "system", ""},
		{"quoted_appearance", "'appearance' = 'auto' # appearance intent\ntheme = 'dark'\n", "system", "# appearance intent"},
		{"multiline", "theme = 'dark'\nnote = '''\n[theme]\ntheme = 'light'\n'''\n", "dark", "note = '''\n[theme]\ntheme = 'light'\n'''"},
		{"unrelated_leaf", "theme = 'dark'\n[future]\ntheme = 'keep' # future comment\n", "dark", "[future]\ntheme = 'keep' # future comment"},
		{"bom_crlf", "\xef\xbb\xbftheme = 'dark'\r\n# keep crlf\r\n[future]\r\nvalue = 42\r\n", "dark", "# keep crlf\r\n[future]\r\nvalue = 42\r\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.source)
			var warnings bytes.Buffer
			old := log.WarningLog.Writer()
			log.WarningLog.SetOutput(&warnings)
			t.Cleanup(func() { log.WarningLog.SetOutput(old) })
			ro, err := LoadConfigReadOnly()
			require.NoError(t, err)
			require.Equal(t, tc.want, ro.Config.Appearance)
			cfg, err := LoadConfig()
			require.NoError(t, err)
			data, err := os.ReadFile(path)
			require.Equal(t, tc.want, cfg.Appearance, string(data))
			require.NoError(t, err)
			require.Contains(t, string(data), tc.keep)
			if strings.HasPrefix(tc.source, "\xef\xbb\xbf") {
				require.True(t, bytes.HasPrefix(data, []byte("\xef\xbb\xbf")))
			}
			require.Equal(t, 1, strings.Count(warnings.String(), "migrated retired"))
			require.Contains(t, warnings.String(), "theme")
			require.Contains(t, warnings.String(), `appearance = "`+tc.want+`"`)
			warnings.Reset()
			_, err = LoadConfig()
			require.NoError(t, err)
			require.NotContains(t, warnings.String(), "migrated retired")
			next, err := os.ReadFile(path)
			require.NoError(t, err)
			require.Equal(t, data, next)
		})
	}
}

func TestPaletteRetirementJSONDoesNotActivateTOMLOnlyKeys(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(`{"theme":"nord","keys":{"quit":"Q"},"network":{"preview_listen_addr":"127.0.0.1:9998","require_token":true},"future_flag":true}`), 0600))
	first, err := LoadConfig()
	require.NoError(t, err)
	second, err := LoadConfig()
	require.NoError(t, err)
	require.Nil(t, first.KeymapOverrides())
	require.Nil(t, second.KeymapOverrides())
	require.Equal(t, first.PreviewListenAddr, second.PreviewListenAddr)
	require.Equal(t, first.RequireToken, second.RequireToken)
	require.Empty(t, second.PreviewListenAddr)
	require.False(t, second.RequireToken)
}

func TestPaletteRetirementJSONUnknownNested(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	require.NoError(t, os.WriteFile(filepath.Join(dir, ConfigFileName), []byte(`{"theme":"nord","docker":{"future_flag":42},"root_agent":{"future_flag":43}}`), 0600))
	_, err := LoadConfig()
	require.NoError(t, err)
	data, err := os.ReadFile(filepath.Join(dir, TomlConfigFileName))
	require.NoError(t, err)
	var shape map[string]any
	require.NoError(t, toml.Unmarshal(data, &shape))
	require.Equal(t, int64(42), shape["docker"].(map[string]any)["future_flag"])
	require.Contains(t, shape, "root_agent")
	require.Equal(t, int64(43), shape["root_agent"].(map[string]any)["future_flag"])
}

func TestPaletteRetirementConcurrentMaterialization(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", dir)
	path := filepath.Join(dir, TomlConfigFileName)
	materializeRaceHookForTest = func() { require.NoError(t, os.WriteFile(path, []byte("theme = 'light'\n"), 0600)) }
	t.Cleanup(func() { materializeRaceHookForTest = nil })
	cfg, err := LoadConfig()
	require.NoError(t, err)
	require.Equal(t, "light", cfg.Appearance)
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	var shape map[string]any
	require.NoError(t, toml.Unmarshal(data, &shape))
	require.NotContains(t, shape, "theme")
	require.Equal(t, "light", shape["appearance"])
}
