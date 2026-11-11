package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

// #7087 review, P3: an in-sandbox adoption rebuild starts the post-worktree
// hooks itself, before Provision's environment refresh ever runs. Those hooks
// get the session's pass-through list and scope id like any other run, not
// the default-deny boundary's bare environment.
func TestAdoptionRebuildRunsItsHooksWithTheSessionEnvironment(t *testing.T) {
	const passName = "AF_ADOPT_HOOK_PASS"
	t.Setenv(passName, "present")
	inst, gw, _ := hostedWorkspace(t)
	inst.ID = "adopt-hook-env-id"
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.SessionEnvPassthrough = []string{passName}
	require.NoError(t, config.SaveConfig(cfg))

	marker := filepath.Join(t.TempDir(), "hook-env")
	configDir, err := config.GetConfigDir()
	require.NoError(t, err)
	repoCfgDir := filepath.Join(configDir, "repos", config.RepoIDFromRoot(inst.Path))
	require.NoError(t, os.MkdirAll(repoCfgDir, 0o755))
	hook := `printf '%s' "$` + passName + `" > ` + shellquote.Quote(marker)
	cfgData, err := json.Marshal(&config.RepoConfig{PostWorktreeCommands: []string{hook}})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(repoCfgDir, config.RepoConfigFileName), cfgData, 0o644))

	require.NoError(t, os.RemoveAll(gw.GetWorktreePath()))
	require.NoError(t, inst.adoptExistingWorktree())
	require.NotNil(t, inst.gitWorktree)
	<-inst.gitWorktree.HooksDone()

	data, err := os.ReadFile(marker)
	require.NoError(t, err, "precondition: the rebuild ran its hook")
	require.Equal(t, "present", string(data), "the rebuild's hook ran with the session's pass-through list")
}
