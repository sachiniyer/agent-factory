package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestRecover_UnrebuildableWorktreeKeepsTheSurvivingPanes pins the order the
// rebuild teardown must respect (#5371): stale panes are torn down to clear the
// way for a REBUILT checkout, so they are torn down only when a rebuild can
// follow. A vanished worktree whose branch is also gone, with no recorded
// conversation to resume into a fresh rebuild, cannot be rebuilt by either
// path — killing its surviving agent first would destroy a running process for
// nothing and leave the row with neither a pane nor a worktree. Recover must
// refuse with the missing-worktree error instead (the shape the daemon's
// WORKTREE_MISSING_DETECTED diagnostic keys on), touching no pane and arming no
// teardown obligation. The control case — the same shape WITH a recorded
// conversation — rebuilds fresh, so the gate must not refuse a rebuild that
// can happen.
func TestRecover_UnrebuildableWorktreeKeepsTheSurvivingPanes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		conversation    bool
		wantRebuildable bool
	}{
		{name: "branch gone and nothing to resume: refuse, kill nothing", conversation: false, wantRebuildable: false},
		{name: "branch gone but a recorded conversation: fresh rebuild proceeds", conversation: true, wantRebuildable: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

			repoRoot := initTempGitRepo(t)
			gitOut(t, repoRoot, "config", "user.email", "test@test.com")
			gitOut(t, repoRoot, "config", "user.name", "test")
			gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
			// The branch is never created: the worktree AND its branch are gone.
			const branch = "af/unrebuildable"

			const agentName = "af_recover_unrebuildable"
			shellName := agentName + tmuxTabSeparator + shellTabName
			worktreePath := filepath.Join(t.TempDir(), "repo-unrebuildable")
			branchCreatedByUs := true
			agentTab := TabData{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName}
			if tc.conversation {
				agentTab.Conversation = &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          "019f386f-7206-7fc2-803b-f7045e07a243",
					CaptureKind: ConversationCaptureCodexRollout,
				}
			}
			data := InstanceData{
				Title:    "unrebuildable",
				Path:     repoRoot,
				Branch:   branch,
				Program:  tmux.ProgramCodex,
				Status:   Lost,
				TmuxName: agentName,
				Tabs:     []TabData{agentTab, {Name: shellTabName, Kind: TabKindShell, TmuxName: shellName}},
				Worktree: GitWorktreeData{
					RepoPath:          repoRoot,
					WorktreePath:      worktreePath,
					SessionName:       "unrebuildable",
					BranchName:        branch,
					BranchCreatedByUs: &branchCreatedByUs,
				},
			}

			// Both tmux sessions survived the deletion — the panes at stake.
			var newSessions int
			var spawns, kills []string
			inner := deadPaneAwareExec(map[string]bool{agentName: true, shellName: true},
				recordingExec(map[string]bool{agentName: true, shellName: true}, &newSessions, &spawns))
			cmdExec := cmd_test.MockCmdExec{
				RunFunc: func(cmd *exec.Cmd) error {
					if strings.Contains(cmd.String(), "kill-session") {
						kills = append(kills, cmd.String())
					}
					return inner.Run(cmd)
				},
				OutputFunc: inner.Output,
			}
			pty := persistPtyFactory{t: t, cmdExec: cmdExec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
			}
			t.Cleanup(func() { restoreTmuxSession = prev })

			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
			restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
			_, statErr := os.Stat(worktreePath)
			require.True(t, os.IsNotExist(statErr), "test setup should start with a missing worktree")

			err = restored.Recover()
			if tc.wantRebuildable {
				require.NoError(t, err, "a fresh rebuild that CAN happen must not be refused by the feasibility gate")
				require.DirExists(t, worktreePath)
				assert.Contains(t, strings.Join(kills, "\n"), agentName,
					"control: a rebuild that proceeds still tears the stale agent pane down first")
				return
			}

			var wtErr *WorktreeUnavailableError
			require.True(t, errors.As(err, &wtErr), "Recover must refuse with the missing-worktree error, got %v", err)
			require.ErrorIs(t, err, os.ErrNotExist,
				"the refusal keeps the not-exist cause the daemon's missing-worktree diagnostic requires")
			assert.Empty(t, kills, "no rebuild can follow, so no surviving pane may be killed")
			assert.Zero(t, newSessions, "nothing is respawned without a worktree")
			assert.False(t, restored.RebuildTeardownPending(),
				"no rebuild is attempted, so no teardown obligation is armed")
			assert.Equal(t, Lost, restored.GetStatus())
			_, statErr = os.Stat(worktreePath)
			assert.True(t, os.IsNotExist(statErr), "the worktree stays missing — nothing was rebuilt")
		})
	}
}
