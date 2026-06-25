package session

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// countingExec is nameKeyedExec with a new-session counter. Sessions named in
// `alive` report existing immediately; others are absent until their
// new-session call (which bumps *newSessions), so a test can assert exactly
// whether a missing session was re-spawned during restore.
func countingExec(alive map[string]bool, newSessions *int) cmd_test.MockCmdExec {
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
				*newSessions++
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

// deadInstanceData builds a persisted record for a local instance with the given
// status, an agent + shell tab, and non-empty worktree data — the exact shape
// (worktree present) that lets TmuxSession.Restore re-spawn a missing session.
func deadInstanceData(t *testing.T, status Status, agentName, shellName string) InstanceData {
	t.Helper()
	const repoPath = "/tmp/dead-respawn-repo"
	return InstanceData{
		Title:    "dead-respawn",
		Path:     repoPath,
		Program:  "claude",
		Status:   status,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:     repoPath,
			WorktreePath: filepath.Join(t.TempDir(), "wt"), // non-empty: enables re-spawn
			SessionName:  "dead-respawn",
			BranchName:   "dead-branch",
		},
	}
}

// TestDeadInstance_NotRespawnedOnLoad is the #970 regression: an instance
// persisted as Dead (its tmux session was killed out from under it) must reload
// as a Dead corpse and must NOT re-spawn any tmux session — neither the agent
// session (TmuxSession.Restore's #386 re-spawn-when-missing path) nor the shell
// tab (setupTabs). It must still load as started=true so the daemon's
// SaveInstances checkpoint (which skips !Started instances) keeps the corpse on
// disk and the user can still kill it.
func TestDeadInstance_NotRespawnedOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	// Isolate config reads from the developer's real ~/.agent-factory (see
	// tab_persist_test.go for the full rationale).
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_dead_agent"
	shellName := agentName + shellTmuxSuffix

	// Both sessions are GONE (the kill that produced Dead): a Restore would hit
	// the missing-session branch and re-spawn via new-session.
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Dead, agentName, shellName))
	require.NoError(t, err)

	assert.Equal(t, 0, newSessions,
		"a Dead instance must NOT re-spawn any tmux session on load (#970)")
	assert.Equal(t, Dead, restored.GetStatus(), "the corpse must stay Dead")
	assert.True(t, restored.Started(),
		"a Dead instance must load started=true so SaveInstances keeps it on disk")
	assert.False(t, restored.TabAlive(0),
		"the Dead agent session must not exist server-side after load")
}

// TestLiveInstance_RespawnsMissingSessionOnLoad guards the seam from the other
// side: the #970 fix must NOT regress the #386/#930 re-spawn-across-reboot path.
// A non-Dead (Running) instance whose tmux server died across a reboot must
// still re-spawn its missing sessions on load.
func TestLiveInstance_RespawnsMissingSessionOnLoad(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_live_agent"
	shellName := agentName + shellTmuxSuffix

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	defer func() { restoreTmuxSession = prev }()

	restored, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)

	assert.Greater(t, newSessions, 0,
		"a live instance with a missing session must still re-spawn on load (#386/#930)")
	assert.True(t, restored.Started())
}
