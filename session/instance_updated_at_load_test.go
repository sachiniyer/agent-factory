package session

import (
	"fmt"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestUpdatedAtLoadRuntimeBoundary(t *testing.T) {
	for _, existing := range []bool{true, false} {
		name := "respawn"
		if existing {
			name = "reattach"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
			before := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
			now := before.Add(time.Hour)
			clockCalls := 0
			oldClock := instanceNow
			instanceNow = func() time.Time { clockCalls++; return now }
			t.Cleanup(func() { instanceNow = oldClock })

			repo := initTempGitRepo(t)
			const tmuxName = "af_updated_at_load"
			gw, err := git.NewGitWorktreeFromStorage(repo, repo, tmuxName, "main", "", true, false)
			require.NoError(t, err)
			cmdExec := nameKeyedExec(map[string]bool{tmuxName: existing})
			ts := tmux.NewTmuxSessionFromSanitizedNameWithDeps(tmuxName, "claude", persistPtyFactory{t: t, cmdExec: cmdExec}, cmdExec)
			i := &Instance{Title: "updated-at-load", Path: repo, Program: "claude", backend: &LocalBackend{},
				liveness: LiveReady, CreatedAt: before, UpdatedAt: before, gitWorktree: gw, Tabs: []*Tab{newAgentTab(ts)}}

			// There is no delivery/churn evidence whose clearing could hide a missing
			// runtime timestamp update, and Start(false) reuses this exact tab binding.
			require.NoError(t, i.Start(false))
			require.True(t, i.Started())
			require.Len(t, i.Tabs, 1)
			require.Same(t, ts, i.Tabs[0].tmux)
			require.True(t, i.lastPromptAttemptAt.IsZero())
			require.Empty(t, i.lastPromptDeliveryStatus)
			require.True(t, i.lastPaneChurnAt.IsZero())
			require.Equal(t, !existing, i.ConsumeLoadRuntimeReplacement())
			if existing {
				require.Equal(t, before, i.ToInstanceData().UpdatedAt)
				require.Zero(t, clockCalls, "reattachment is reconstruction, not activity")
				require.Zero(t, i.agentRuntimeGeneration)
			} else {
				require.Equal(t, now, i.ToInstanceData().UpdatedAt)
				require.Equal(t, 1, clockCalls, "the runtime replacement must touch even with no idle evidence")
				require.Equal(t, uint64(1), i.agentRuntimeGeneration)
			}
		})
	}
}

func TestUpdatedAtLoadSandboxLoss(t *testing.T) {
	created := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
	now := created.Add(2 * time.Hour)
	oldClock := instanceNow
	instanceNow = func() time.Time { return now }
	t.Cleanup(func() { instanceNow = oldClock })
	for _, backend := range []string{"docker", "ssh", "sandbox", "remote"} {
		for _, live := range []Liveness{LiveRunning, LiveReady, LiveLost, LiveArchived} {
			for _, stored := range []time.Time{created.Add(time.Hour), {}} {
				t.Run(fmt.Sprintf("%s/%v/zero=%t", backend, live, stored.IsZero()), func(t *testing.T) {
					loaded, err := FromInstanceData(InstanceData{Title: "sandbox", BackendType: backend, Liveness: live, CreatedAt: created, UpdatedAt: stored})
					require.NoError(t, err)
					want := stored
					if want.IsZero() {
						want = created
					}
					wantLive := live
					if live != LiveLost && live != LiveArchived {
						want = now
						wantLive = LiveLost
					}
					require.Equal(t, wantLive, loaded.liveness)
					require.Equal(t, want, loaded.ToInstanceData().UpdatedAt)
				})
			}
		}
	}
}

func TestUpdatedAtLoadSiblingRuntimeBoundary(t *testing.T) {
	for _, kind := range []TabKind{TabKindShell, TabKindProcess} {
		for _, existing := range []bool{true, false} {
			t.Run(fmt.Sprintf("%v/existing=%t", kind, existing), func(t *testing.T) {
				t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
				before := time.Date(2026, 8, 7, 14, 50, 6, 0, time.UTC)
				now := before.Add(time.Hour)
				oldClock := instanceNow
				instanceNow = func() time.Time { return now }
				t.Cleanup(func() { instanceNow = oldClock })
				repo := initTempGitRepo(t)
				const agentName = "af_updated_at_sibling_agent"
				const siblingName = "af_updated_at_sibling"
				gw, err := git.NewGitWorktreeFromStorage(repo, repo, agentName, "main", "", true, false)
				require.NoError(t, err)
				cmdExec := nameKeyedExec(map[string]bool{agentName: true, siblingName: existing})
				factory := persistPtyFactory{t: t, cmdExec: cmdExec}
				agent := tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", factory, cmdExec)
				sibling := tmux.NewTmuxSessionFromSanitizedNameWithDeps(siblingName, "sh", factory, cmdExec)
				tab := &Tab{ID: "sibling", Name: "sibling", Kind: kind, Command: "sh", tmux: sibling}
				i := &Instance{Title: "updated-at-sibling", Path: repo, Program: "claude", backend: &LocalBackend{},
					liveness: LiveReady, CreatedAt: before, UpdatedAt: before, gitWorktree: gw, Tabs: []*Tab{newAgentTab(agent), tab}}
				require.NoError(t, i.Start(false))
				require.Len(t, i.Tabs, 2)
				require.Same(t, sibling, i.Tabs[1].tmux, "respawn reuses the persisted binding")
				require.Zero(t, i.agentRuntimeGeneration)
				require.False(t, i.ConsumeLoadRuntimeReplacement())
				want := before
				if !existing {
					want = now
				}
				require.Equal(t, want, i.ToInstanceData().UpdatedAt)
			})
		}
	}
}
