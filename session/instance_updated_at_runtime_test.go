package session

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpdatedAtRuntimeMutations(t *testing.T) {
	clock := time.Date(2026, 9, 5, 1, 0, 0, 0, time.UTC)
	oldClock := instanceNow
	instanceNow = func() time.Time { return clock }
	t.Cleanup(func() { instanceNow = oldClock })
	cases := map[string]func(*testing.T, *Instance){
		"shell spawn": func(t *testing.T, i *Instance) { _, err := i.AddShellTab(); require.NoError(t, err) },
		"process spawn": func(t *testing.T, i *Instance) {
			_, err := i.AddProcessTab("echo hello", "job")
			require.NoError(t, err)
		},
		"shell attachment": func(t *testing.T, i *Instance) {
			_, err := i.AttachShellTab("shell", "af_updated_at__shell", "shell")
			require.NoError(t, err)
		},
		"carried roster": func(t *testing.T, i *Instance) {
			i.carriedTabs = []TabData{{Kind: TabKindAgent}, {Kind: TabKindWeb, Name: "web", URL: "https://example.com"}}
			i.restoreCarriedTabs()
		},
		"reconciled roster": func(t *testing.T, i *Instance) {
			require.True(t, i.appendReconciledTab("web", "web", &Tab{ID: "web", Name: "web", Kind: TabKindWeb}))
		},
		"runtime binding": func(t *testing.T, i *Instance) {
			require.NoError(t, i.bindProvisionResult(ProvisionResult{Backend: newInertSandboxBackend("docker")}))
		},
		"archive roster": func(t *testing.T, i *Instance) {
			i.Tabs = append(i.Tabs, &Tab{Kind: TabKindShell})
			i.mu.Lock()
			defer i.mu.Unlock()
			(teardownArchive{}).finalize(i, nil, nil)
		},
		"kill finalizer": func(t *testing.T, i *Instance) {
			i.mu.Lock()
			defer i.mu.Unlock()
			(teardownKill{}).finalize(i, nil, i.gitWorktree)
		},
		"detach finalizer": func(t *testing.T, i *Instance) {
			i.mu.Lock()
			defer i.mu.Unlock()
			clearClosedTmuxRefs(i, []closedTab{{id: i.Tabs[0].ID, ts: i.Tabs[0].tmux}})
		},
		"recover fence":     func(t *testing.T, i *Instance) { i.liveness = LiveLost; require.NoError(t, i.BeginRecoverFence()) },
		"end recover fence": func(t *testing.T, i *Instance) { i.inFlightOp = OpRestoring; require.True(t, i.EndRecoverFence()) },
		"resume fence": func(t *testing.T, i *Instance) {
			i.liveness = LiveLimitReached
			require.NoError(t, i.BeginLimitResume())
		},
		"end resume fence": func(t *testing.T, i *Instance) { i.inFlightOp = OpRespawning; require.True(t, i.EndLimitResume()) },
		"archive restore fence": func(t *testing.T, i *Instance) {
			i.liveness = LiveArchived
			require.NoError(t, i.BeginArchivedRestoreFence())
		},
		"end archive restore fence": func(t *testing.T, i *Instance) {
			i.inFlightOp = OpRestoring
			require.True(t, i.EndArchivedRestoreFence())
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			extra := []string{}
			if name == "shell attachment" {
				extra = append(extra, "af_updated_at__shell")
			}
			i := startedMockInstance(t, "af_updated_at", extra...)
			i.UpdatedAt = clock
			before := clock
			clock = clock.Add(time.Minute)
			mutate(t, i)
			require.Equal(t, clock, i.UpdatedAt)
			require.True(t, i.UpdatedAt.After(before))
		})
	}
}

func TestUpdatedAtReattachPreservesTimestamp(t *testing.T) {
	i := startedMockInstance(t, "af_updated_at_reattach")
	at := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	i.UpdatedAt = at
	i.started = false
	require.NoError(t, i.Start(false))
	require.True(t, i.Started())
	require.Equal(t, at, i.UpdatedAt)
}
