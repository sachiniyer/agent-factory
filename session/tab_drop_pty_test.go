package session

import (
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The #1539 attach-PTY leak these tests once guarded (dropping a tab had to close
// the tab's `tmux attach-session` ptmx or its fd leaked) was retired in #1592
// Phase 2 PR7: a tab holds no attach PTY anymore (the local runtime's data plane
// is the daemon's clientless broker), so a drop has no fd to release. The
// enduring contract the drop paths must still honour is pinned here: dropping a
// tab removes it from the local view and NEVER runs `tmux kill-session` (the
// daemon owns teardown, #960).

// newDropTabTestInstance builds a started local instance with an agent tab and a
// shell tab, plus a recorder of every tmux command run, so a test can assert the
// drop path removed the tab without killing its session.
func newDropTabTestInstance(t *testing.T) (*Instance, *[]string, *sync.Mutex) {
	t.Helper()
	log.Initialize(false)
	t.Cleanup(log.Close)

	var mu sync.Mutex
	var ran []string
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			mu.Lock()
			ran = append(ran, strings.Join(c.Args, " "))
			mu.Unlock()
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("output"), nil },
	}

	agentTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_drop_agent", "claude", nil, cmdExec)
	shellTs := tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_drop_agent__shell", "/bin/sh", nil, cmdExec)
	shellTab := newShellTab(shellTs)
	shellTab.Name = "shell"
	inst := &Instance{
		Title:   "drop",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(agentTs), shellTab},
	}
	return inst, &ran, &mu
}

func assertDropRanNoKill(t *testing.T, ran *[]string, mu *sync.Mutex) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	for _, c := range *ran {
		assert.NotContains(t, c, "kill-session",
			"dropping a tab must not kill its tmux session — the daemon owns teardown (#960); ran: %v", *ran)
	}
}

// TestDropClosedTabRemovesTabWithoutKill guards the DropClosedTab path: dropping a
// daemon-closed tab from the local view removes it and never kills the session.
func TestDropClosedTabRemovesTabWithoutKill(t *testing.T) {
	inst, ran, mu := newDropTabTestInstance(t)

	require.NoError(t, inst.DropClosedTab(1))
	require.Len(t, inst.GetTabs(), 1, "the closed tab must be dropped from the local view")

	assertDropRanNoKill(t, ran, mu)
}

// TestDropTabByNameRemovesTabWithoutKill is the same guard for the dropTabByName
// path (used by ReconcileTabsFromData when the daemon drops a tab out-of-band).
func TestDropTabByNameRemovesTabWithoutKill(t *testing.T) {
	inst, ran, mu := newDropTabTestInstance(t)

	require.True(t, inst.dropTabByName("shell"), "the named tab must be found and dropped")
	require.Len(t, inst.GetTabs(), 1, "the dropped tab must leave the local view")

	assertDropRanNoKill(t, ran, mu)
}
