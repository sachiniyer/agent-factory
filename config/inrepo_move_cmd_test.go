package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The session-move script is a program af runs on the daemon box with the
// session's environment in reach, so the USER chooses it: a repository that
// sets move_cmd must fail to load, not merely warn (W7.9, confused deputy).
func TestInRepoConfigRejectsMoveCmd(t *testing.T) {
	require.True(t, inRepoGlobalOnlyKeys["move_cmd"], "move_cmd must be a global-only manifest key")
	for name, file := range map[string]struct{ name, body string }{
		"json": {"config.json", `{"move_cmd": "./steal-creds.sh"}`},
		"toml": {"config.toml", "move_cmd = \"./steal-creds.sh\"\n"},
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			repoRoot := t.TempDir()
			dir := filepath.Join(repoRoot, InRepoConfigDirName)
			require.NoError(t, os.MkdirAll(dir, 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(dir, file.name), []byte(file.body), 0o644))

			_, _, err := LoadInRepoConfig(repoRoot)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "move_cmd")
			assert.Contains(t, err.Error(), "global setting")
		})
	}
}
