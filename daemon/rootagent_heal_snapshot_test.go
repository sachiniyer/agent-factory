package daemon

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// #7084: a root heal reaps the dead root's record — the only pointer to the
// conversation (#2616) and tab roster (#2628) it carries — and then creates the
// replacement. Since #7078 that create's own project-config reads refuse an
// unknown config, so a load spike between the reap and the create used to bring
// root back blank. The heal now reads everything the create needs before the
// reap, into one snapshot the create answers from.

// configReadingBackend stands in for the local backend's launch reads: its
// start reads the session's environment pass-through the way
// prepareCreateLaunch and Provision do (instance-scoped), and fails on any
// error that read returns.
type configReadingBackend struct {
	readyFakeBackend
}

func (b configReadingBackend) Provision(i *session.Instance, first bool) error {
	if _, err := session.SessionEnvPassthroughForInstance(i); err != nil {
		return err
	}
	return b.readyFakeBackend.Provision(i, first)
}

func (b configReadingBackend) Start(i *session.Instance, first bool) error {
	if _, err := session.SessionEnvPassthroughForInstance(i); err != nil {
		return err
	}
	return b.readyFakeBackend.Start(i, first)
}

// slowProjectConfigAfter puts a git in front of the real one whose
// checkout-identity probe — the read #6748 bounds with its 1s scan — sleeps past
// that budget once the returned switch has been thrown.
func slowProjectConfigAfter(t *testing.T) (slow func()) {
	t.Helper()
	realGit, err := exec.LookPath("git")
	require.NoError(t, err)
	flag := filepath.Join(t.TempDir(), "slow")
	bin := t.TempDir()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$#" -eq 4 ] && [ "$3" = rev-parse ] && [ "$4" = --git-common-dir ] && [ -e %q ]; then
	sleep 3
fi
exec %q "$@"
`, flag, realGit)
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte(script), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	return func() { _ = os.WriteFile(flag, nil, 0o600) }
}

func TestRootHealKeepsItsCarriedStateWhenTheConfigStopsAnsweringAfterTheReap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	claudeConfigDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeConfigDir)
	var (
		mu   sync.Mutex
		seen []session.InstanceOptions
	)
	t.Cleanup(session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		mu.Lock()
		seen = append(seen, opts)
		mu.Unlock()
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return configReadingBackend{readyFakeBackend{backend}}, nil
	}))
	repoPath := setupControlRepo(t)
	// Any registered project makes the personal-config lookup probe the
	// checkout (an empty registry settles "absent" without one, #7078).
	registerTestProject(t, setupControlRepo(t))
	writeRootClaudeTranscript(t, claudeConfigDir, repoPath, priorRootConversationID)
	slow := slowProjectConfigAfter(t)

	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	require.NoError(t, err)
	manager.ensureRootAgentsAndWait()
	first := findRootInstance(t, manager, repoPath)
	require.NotNil(t, first, "precondition: the first ensure creates the root")
	prior := seedRootConversation(t, first)

	prevHook := testHookRootAfterReap
	testHookRootAfterReap = slow // the load spike lands right after the reap
	t.Cleanup(func() { testHookRootAfterReap = prevHook })
	first.SetStatusForTest(session.Lost)
	manager.ensureRootAgentsAndWait()

	healed := findRootInstance(t, manager, repoPath)
	require.NotNil(t, healed, "the heal's create must not refuse on a config read after the reap")
	require.NotSame(t, first, healed)
	mu.Lock()
	last := seen[len(seen)-1]
	mu.Unlock()
	require.Equal(t, prior.ID, last.ResumeConversation.ID,
		"the replacement resumes the conversation the reaped root carried, not a blank one")
}
