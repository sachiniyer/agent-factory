package app

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// configFollowingHome is activeProjectHome built the way a launch with no
// --program flag builds it: the default program is derived from config, and the
// cache is seeded from the config loaded at that moment.
func configFollowingHome(t *testing.T) *home {
	t.Helper()
	h := activeProjectHome(t)
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	h.appConfig = cfg
	h.programChoice = newProgramChoice("", cfg)
	require.Equal(t, tmux.ProgramClaude, h.program, "precondition: the launch snapshot is the built-in default")
	return h
}

// TestNewSessionFormFollowsALiveGlobalDefaultProgramChange is #4889: `af config
// set default_program` applies live (#2480), so a form opened after it must
// default to the new value, not the one the TUI read at launch.
func TestNewSessionFormFollowsALiveGlobalDefaultProgramChange(t *testing.T) {
	h := configFollowingHome(t)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramAider, h.pendingProgram,
		"the form must default to the currently configured default_program, not the launch snapshot")
}

// TestNewSessionFormFollowsALiveProjectDefaultProgram pins the scope: the form
// resolves default_program the way `af config get` does for the project —
// project override > global > built-in — at the moment it opens.
func TestNewSessionFormFollowsALiveProjectDefaultProgram(t *testing.T) {
	h := configFollowingHome(t)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)
	writeInRepoConfig(t, h.repoRoot, "default_program = \"codex\"\n")

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramCodex, h.pendingProgram,
		"the project's own default_program outranks the global one")
}

// TestNewSessionFormKeepsTheLaunchProgramFlag is the other half: `af --program
// gemini` is an explicit choice for this run, and a config change must not
// override it.
func TestNewSessionFormKeepsTheLaunchProgramFlag(t *testing.T) {
	h := activeProjectHome(t)
	h.programChoice = newProgramChoice(tmux.ProgramGemini, h.appConfig)

	_, err := config.SetGlobalConfigValue("default_program", tmux.ProgramAider)
	require.NoError(t, err)

	_, _ = h.startNewInstance()
	require.Equal(t, stateNew, h.state)
	require.Equal(t, tmux.ProgramGemini, h.pendingProgram, "the --program flag outranks config")
}

// TestSwitchProjectMakesTheDefaultProgramFollowConfig: a launch --program flag
// has always been replaced by the incoming project's resolution on a switch, so
// from then on the default follows that project's live config.
func TestSwitchProjectMakesTheDefaultProgramFollowConfig(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) (daemon.SnapshotResponse, error) { return daemon.SnapshotResponse{}, nil }
	h.programChoice = newProgramChoice(tmux.ProgramGemini, h.appConfig)

	root := initTestGitRepo(t)
	h.switchProject(&config.RepoContext{Root: root, ID: config.RepoIDFromRoot(root)})
	require.True(t, h.programFollowsConfig)

	writeInRepoConfig(t, root, "default_program = \"codex\"\n")
	require.Equal(t, tmux.ProgramCodex, h.defaultProgram())
}
