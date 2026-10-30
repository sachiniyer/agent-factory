package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The set is closed: a value can never name a path off the daemon box or a
// credential file, whatever it is spelled as.
func TestNormalizeSandboxAgentConfigIsAClosedSet(t *testing.T) {
	got, err := NormalizeSandboxAgentConfig([]string{" claude:settings.json ", "codex:config.toml", "claude:settings.json"})
	require.NoError(t, err)
	assert.Equal(t, []string{"claude:settings.json", "codex:config.toml"}, got, "trimmed and de-duplicated, order kept")

	for _, bad := range []string{
		"claude:.credentials.json", "codex:auth.json", // a login is not personalization
		"claude:../../etc/passwd", "claude:/etc/passwd", "claude:settings.json/../x",
		"gemini:settings.json", "settings.json", "claude:", ":settings.json", "",
	} {
		_, err := NormalizeSandboxAgentConfig([]string{bad})
		assert.Error(t, err, "%q must be refused", bad)
	}
}

// A repository must never choose what af copies off the daemon box: a checked-in
// value is a load error, not a warning (the key is global-only).
func TestInRepoConfigRejectsSandboxAgentConfig(t *testing.T) {
	require.True(t, inRepoGlobalOnlyKeys["sandbox_agent_config"])
	for name, file := range map[string]struct{ name, body string }{
		"json": {"config.json", `{"sandbox_agent_config": ["claude:settings.json"]}`},
		"toml": {"config.toml", "sandbox_agent_config = [\"claude:settings.json\"]\n"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			repoRoot := t.TempDir()
			dir := filepath.Join(repoRoot, InRepoConfigDirName)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, file.name), []byte(file.body), 0o644))
			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "sandbox_agent_config")
			assert.Contains(t, err.Error(), "global setting")
		})
	}
}

// A global config with an entry outside the set fails to load, naming the key.
func TestGlobalConfigRejectsAnEntryOutsideTheSet(t *testing.T) {
	_, err := parseConfigTOML([]byte("sandbox_agent_config = [\"claude:.credentials.json\"]\n"), "global.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "sandbox_agent_config")
	cfg, err := parseConfigTOML([]byte("sandbox_agent_config = [\"claude:settings.json\"]\n"), "global.toml")
	require.NoError(t, err)
	assert.Equal(t, []string{"claude:settings.json"}, cfg.SandboxAgentConfig)
}
