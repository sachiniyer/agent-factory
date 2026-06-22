package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// persistPtyFactory is a tmux.PtyFactory returning a real temp file as the PTY,
// so the attach path in Restore can open and close it without crashing. Like
// MockPtyFactory it forwards the command (e.g. new-session, attach-session) to
// the mock executor so session-existence bookkeeping fires.
type persistPtyFactory struct {
	t       *testing.T
	cmdExec cmd_test.MockCmdExec
}

func (p persistPtyFactory) Start(cmd *exec.Cmd) (*os.File, error) {
	f, err := os.CreateTemp(p.t.TempDir(), "pty-")
	if err == nil {
		_ = p.cmdExec.Run(cmd)
	}
	return f, err
}
func (p persistPtyFactory) Close() {}

// nameKeyedExec is a tmux mock that tracks session existence per session name,
// so an instance's agent and shell sessions are independent. Sessions named in
// `alive` report existing immediately (the reconnect path); others come into
// existence after their new-session.
func nameKeyedExec(alive map[string]bool) cmd_test.MockCmdExec {
	existing := map[string]bool{}
	for k, v := range alive {
		existing[k] = v
	}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return cmd.Args[i+1]
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return assertNoSession
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				delete(existing, n)
				return nil
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			return []byte("content"), nil
		},
	}
}

var assertNoSession = &noSessionErr{}

type noSessionErr struct{}

func (*noSessionErr) Error() string { return "session does not exist" }

// TestRestartSurvival_AgentAndShellTabsReconnect is the headline #930 test: an
// instance with agent+shell tabs is persisted, written to disk through Storage,
// and reloaded with a fresh Storage. BOTH tabs must be restored and reconnect to
// their EXACT persisted tmux sessions — an af/daemon restart must leave every
// tab active and reconnectable, like the agent session is today.
func TestRestartSurvival_AgentAndShellTabsReconnect(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const repoPath = "/tmp/restart-survival-repo"
	const agentName = "af_abc123_restart"
	shellName := agentName + shellTmuxSuffix

	origExec := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
	pty := persistPtyFactory{t: t, cmdExec: origExec}

	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), "restart",
		"restart-branch", "", false, true)
	require.NoError(t, err)

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, origExec)
	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps(shellName, "/bin/sh", pty, origExec)
	inst := &Instance{
		Title:       "restart",
		Path:        repoPath,
		Program:     "claude",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
		Tabs:        []*Tab{newAgentTab(agentTs), newShellTab(shellTs)},
	}

	// Persist: ToInstanceData must serialize both tabs (and keep the legacy
	// single TmuxName from the agent tab for rollback safety).
	data := inst.ToInstanceData()
	require.Len(t, data.Tabs, 2, "both tabs must be serialized")
	assert.Equal(t, TabKindAgent, data.Tabs[0].Kind)
	assert.Equal(t, agentName, data.Tabs[0].TmuxName)
	assert.Equal(t, TabKindShell, data.Tabs[1].Kind)
	assert.Equal(t, shellName, data.Tabs[1].TmuxName)
	assert.Equal(t, agentName, data.TmuxName, "legacy TmuxName must still be written from the agent tab")

	// Write to disk through Storage, then reload with a fresh Storage. Restore
	// uses mock-backed sessions for the exact persisted names so reconnection is
	// hermetic.
	repoID := config.RepoIDFromRoot(repoPath)
	ms := newMockStorage()
	saveStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	require.NoError(t, saveStore.SaveInstances([]*Instance{inst}))

	restoreExec := nameKeyedExec(map[string]bool{agentName: true, shellName: true})
	restorePty := persistPtyFactory{t: t, cmdExec: restoreExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, restorePty, restoreExec)
	}
	defer func() { restoreTmuxSession = prev }()

	loadStore, err := NewStorage(ms, repoID)
	require.NoError(t, err)
	loaded, err := loadStore.LoadInstances()
	require.NoError(t, err)
	require.Len(t, loaded, 1, "the persisted instance must reload")

	restored := loaded[0]
	tabs := restored.GetTabs()
	require.Len(t, tabs, 2, "both tabs must be restored after reload")

	assert.Equal(t, TabKindAgent, tabs[0].Kind)
	assert.Equal(t, agentName, tabs[0].tmux.SanitizedName(),
		"agent tab must reconnect to its exact persisted tmux session")
	assert.Equal(t, TabKindShell, tabs[1].Kind)
	assert.Equal(t, shellName, tabs[1].tmux.SanitizedName(),
		"shell tab must reconnect to its exact persisted tmux session")
	assert.True(t, restored.ShellTabAlive(),
		"the restored shell tab session must be live (reconnected) after restart")
}

// TestRestartSurvival_BackCompatSynthesizesTabs covers the upgrade path: an OLD
// instances.json (written before #930 PR 2, no Tabs field) must load into
// [agent, shell]. The agent tab keeps the EXACT legacy tmux name (so an existing
// live agent session survives the upgrade); the shell tab is created fresh.
func TestRestartSurvival_BackCompatSynthesizesTabs(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	const repoPath = "/tmp/backcompat-repo"
	const legacyName = "af_legacy_agent" // no repo hash form, exact legacy name
	shellName := legacyName + shellTmuxSuffix

	// Legacy record: agent reconnects (alive), shell is fresh-created (absent).
	restoreExec := nameKeyedExec(map[string]bool{legacyName: true})
	restorePty := persistPtyFactory{t: t, cmdExec: restoreExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, restorePty, restoreExec)
	}
	defer func() { restoreTmuxSession = prev }()

	data := InstanceData{
		Title:    "legacy",
		Path:     repoPath,
		Program:  "claude",
		Status:   Running,
		TmuxName: legacyName, // legacy single-session field; NO Tabs
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(t.TempDir(), "wt"),
			SessionName:  "legacy",
			BranchName:   "legacy-branch",
		},
	}
	require.Empty(t, data.Tabs, "fixture must be a pre-#930 record with no Tabs")

	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	tabs := restored.GetTabs()
	require.Len(t, tabs, 2, "legacy record must synthesize [agent, shell]")
	assert.Equal(t, TabKindAgent, tabs[0].Kind)
	assert.Equal(t, legacyName, tabs[0].tmux.SanitizedName(),
		"agent tab must keep the EXACT legacy tmux name so a live agent session survives the upgrade")
	assert.Equal(t, TabKindShell, tabs[1].Kind)
	assert.Equal(t, shellName, tabs[1].tmux.SanitizedName(),
		"shell tab must be created fresh with the derived __shell name")
	assert.True(t, restored.ShellTabAlive(), "the synthesized shell tab must be live")
}
