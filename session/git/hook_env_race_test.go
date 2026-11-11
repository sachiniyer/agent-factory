package git

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// #7083 review: a tab spawn can refresh a live worktree's hook environment
// (#7080) while its git commands and hook runner read it. Run under -race,
// this fails if the writes or the reads stop going through hookEnvMu.
func TestHookEnvironmentRefreshesWhileGitCommandsRead(t *testing.T) {
	gw, err := NewGitWorktreeFromStorage(t.TempDir(), t.TempDir(), "hook-env-race", "main", "", true, false)
	require.NoError(t, err)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for k := 0; k < 4; k++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 50; n++ {
				require.NoError(t, gw.SetHookEnvironment([]string{"AF_HOOK_ENV_RACE"}))
				gw.SetHookScopeSessionID("session-id")
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			for n := 0; n < 50; n++ {
				_ = gw.gitCommandEnvironment()
				_ = gw.HookEnvironment()
				_ = gw.hookScopeID()
			}
		}()
	}
	close(start)
	wg.Wait()
	require.Equal(t, []string{"AF_HOOK_ENV_RACE"}, gw.HookEnvironment())
}
