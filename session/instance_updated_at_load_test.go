package session

import (
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
