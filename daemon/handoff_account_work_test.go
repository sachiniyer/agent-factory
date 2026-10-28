package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestHandoffAccountSameAgentDeliversExistingWork(t *testing.T) {
	for _, goal := range []string{"finish migration", ""} {
		t.Run("goal="+goal, func(t *testing.T) {
			m, repo, inst, backend := newAutoResumeManager(t, "", true, goal, time.Now().Add(time.Hour))
			configureLimitAccountCandidate(t, m, "personal")
			inst.ClearLimitReached()
			handoffBoundaryWorktree(t, inst)
			tip := handoffBoundaryCommit(t, inst, "unpushed migration work")
			require.NoError(t, os.WriteFile(filepath.Join(inst.Path, "partial.txt"), []byte("unfinished migration\n"), 0600))
			resp, err := m.HandoffSession(HandoffSessionRequest{Title: inst.Title, RepoID: repo, Account: "personal"})
			require.NoError(t, err)
			require.Equal(t, tip, resp.HeadSHA)
			_, _, prompts := backend.snapshot()
			require.Len(t, prompts, 1)
			require.Contains(t, prompts[0], "Work already done on branch main")
			require.Contains(t, prompts[0], tip)
			require.Contains(t, prompts[0], "1 commit, 1 uncommitted file")
			require.Contains(t, prompts[0], "git diff HEAD")
			require.Contains(t, prompts[0], "Do not start over")
			require.NotContains(t, prompts[0], "It was being done by claude")
			if goal != "" {
				require.Contains(t, prompts[0], goal)
			}
		})
	}
}
