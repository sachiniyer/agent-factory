package git

import (
	"os"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/systemdunit"
)

// #7087 review, P3: TestHookEnvironmentRefreshesWhileGitCommandsRead checks the
// accessors, not their callers. This drives the call sites that hand the hook
// environment on — runHooks (the hook run's pass-through and scope id) and
// hookScopePrefixes (the scope sweep) — against concurrent refreshes, so under
// -race it fails if either reads the fields directly again.
func TestHookRunCallSitesReadTheEnvironmentUnderItsLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	// The daemon gate hookScopePrefixes reads the scope id behind.
	t.Setenv(systemdunit.DaemonMarkerEnv, systemdunit.DaemonUnitName)
	t.Setenv("SYSTEMD_EXEC_PID", strconv.Itoa(os.Getpid()))
	gw, err := NewGitWorktreeFromStorage(t.TempDir(), t.TempDir(), "hook-callsite-race", "main", "", false, false)
	require.NoError(t, err)

	// The refreshes keep coming for as long as the call sites read, so every
	// read overlaps a write: a refresher that finished first would order every
	// later read after it and hide an unlocked one from the race detector.
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := gw.SetHookEnvironment([]string{"AF_HOOK_CALLSITE_RACE"}); err != nil {
				t.Error(err)
				return
			}
			gw.SetHookScopeSessionID("session-id")
		}
	}()
	for n := 0; n < 40; n++ {
		_ = gw.hookScopePrefixes()
		<-gw.runHooks()
	}
	close(stop)
	wg.Wait()
}
