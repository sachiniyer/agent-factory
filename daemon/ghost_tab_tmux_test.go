package daemon

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// createRealTmuxSession starts a detached session named `name` on the test's
// private tmux server. Its pane runs a plain sleeper, whose default SIGHUP
// disposition is to terminate — so `tmux kill-session` reaps it cleanly and a
// killed session leaves no survivor, exactly the outcome ghostCleanup must
// achieve for EVERY tab.
func createRealTmuxSession(t *testing.T, name string) {
	t.Helper()
	out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "sleep 300").CombinedOutput()
	require.NoError(t, err, "tmux new-session %q: %s", name, out)
}

// liveTmuxSessions returns the set of session names on the test's private tmux
// server. A server with no sessions left (tmux exits non-zero with "no server
// running") is reported as the empty set, not an error.
func liveTmuxSessions(t *testing.T) map[string]bool {
	t.Helper()
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").CombinedOutput()
	live := map[string]bool{}
	if err != nil {
		return live // no server / no sessions → nothing alive
	}
	for _, name := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if name = strings.TrimSpace(name); name != "" {
			live[name] = true
		}
	}
	return live
}

// TestGhostCleanup_KillsEveryTabTmux is the #2007 regression. Killing a ghost
// session — one with no live Instance in memory, only its persisted record —
// must reap the tmux sessions of its shell and process tabs, not just the legacy
// agent-tab name. Before the fix, ghostCleanup killed only data.TmuxName, so a
// multi-tab ghost's `<agent>__shell` and `<agent>__btop` tmux sessions leaked
// onto the server with no cleanup path short of `af reset`.
//
// It queries the test's PRIVATE tmux server (testguard.IsolateTmux), so nothing
// it creates can touch or leak onto a real one.
func TestGhostCleanup_KillsEveryTabTmux(t *testing.T) {
	testguard.IsolateTmux(t) // a private tmux server, killed when the test ends

	const (
		agentName = "af_ghost2007_agent"
		shellName = "af_ghost2007_agent__shell"
		btopName  = "af_ghost2007_agent__btop"
	)
	all := []string{agentName, shellName, btopName}
	for _, name := range all {
		createRealTmuxSession(t, name)
	}
	// Precondition: all three sessions are live before the ghost is cleaned up.
	before := liveTmuxSessions(t)
	for _, name := range all {
		require.Truef(t, before[name], "precondition: %q should be live before ghostCleanup", name)
	}

	// A ghost record in the post-#953 shape: the agent tab is in the legacy
	// TmuxName field AND all three tabs are in the persisted Tabs list. The
	// Worktree is left empty so ghostCleanupWorktree is a no-op and only the tmux
	// teardown under test runs.
	data := &session.InstanceData{
		Title:    "ghost2007",
		Program:  "claude",
		TmuxName: agentName,
		Tabs: []session.TabData{
			{Name: "agent", Kind: session.TabKindAgent, TmuxName: agentName},
			{Name: "shell", Kind: session.TabKindShell, TmuxName: shellName},
			{Name: "btop", Kind: session.TabKindProcess, TmuxName: btopName},
		},
	}

	require.NoError(t, ghostCleanup(data, "ghost2007"))

	after := liveTmuxSessions(t)
	require.Falsef(t, after[shellName], "#2007: shell tab tmux session %q leaked after ghost kill", shellName)
	require.Falsef(t, after[btopName], "#2007: process tab tmux session %q leaked after ghost kill", btopName)
	require.Falsef(t, after[agentName], "agent tab tmux session %q should also be gone", agentName)
}
