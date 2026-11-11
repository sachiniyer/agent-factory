package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7077 review: a swap is admitted against a readable project config, then
// executed after the outgoing agent has stopped. If the execution re-read the
// config and that read could not complete, the swap would fail with no agent
// running — the refusal arriving after the point where refusing kept one alive.
// Execution must apply the pass-through list admission already resolved.

// unreadableAfterAdmission makes every later instance-scoped config read for
// inst go unanswered, the way a box under load answers it, and counts the reads.
func unreadableAfterAdmission(inst *Instance) *atomic.Int32 {
	var reads atomic.Int32
	rc := NewRestoreContext()
	rc.repoFromPath = func(path string) (*config.RepoContext, error) {
		return &config.RepoContext{Root: path, IdentityRoot: path, ID: "id-frozen-env"}, nil
	}
	rc.resolveConfig = func(*config.RepoContext) (*config.ResolvedConfig, error) {
		reads.Add(1)
		return nil, unansweredProbe("/repo")
	}
	inst.restoreCtx.Store(rc)
	return &reads
}

func TestHandoffSwapAppliesThePassthroughItsPreflightResolved(t *testing.T) {
	globalAiderHome(t)
	aider := filepath.Join(t.TempDir(), "aider")
	require.NoError(t, os.WriteFile(aider, []byte("#!/bin/sh\n"), 0o755))
	cfg, err := config.LoadConfig()
	require.NoError(t, err)
	cfg.ProgramOverrides[tmux.ProgramAider] = aider // preflight checks it exists
	cfg.SessionEnvPassthrough = []string{"PROJECT_TOKEN"}
	require.NoError(t, config.SaveConfig(cfg))

	ptyFactory := &recordingPtyFactory{t: t}
	killed := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "kill-session"):
				killed = true
			case strings.Contains(joined, "has-session"):
				if killed && len(ptyFactory.cmds) == 0 {
					return errors.New("session absent after close")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			if strings.Contains(strings.Join(c.Args, " "), "display-message") {
				return nil, errors.New("pane pid unavailable")
			}
			return nil, nil
		},
	}
	repoRoot := initTempGitRepo(t)
	worktreePath := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktreePath, "frozen-env", "frozen-env-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("frozen-env", tmux.ProgramClaude, ptyFactory, cmdExec)
	backend := &LocalBackend{}
	inst := &Instance{
		ID: "frozen-env-id", Title: "frozen-env", Path: repoRoot, Program: tmux.ProgramClaude,
		backend: backend, Tabs: []*Tab{newAgentTab(ts)}, gitWorktree: gw, started: true, liveness: LiveRunning,
	}

	plan, err := backend.PrepareAgentSwap(inst, tmux.ProgramAider)
	require.NoError(t, err, "admission reads a readable config")
	require.True(t, plan.passthroughFrozen)
	require.Contains(t, plan.passthrough, "PROJECT_TOKEN", "the plan freezes the list admission resolved")

	reads := unreadableAfterAdmission(inst)
	require.NoError(t, backend.SwapAgent(inst, plan),
		"an admitted swap must not fail after teardown on a config read it already answered")
	require.True(t, killed, "precondition: the outgoing agent was stopped")
	require.NotEmpty(t, ptyFactory.cmds, "the replacement agent started")
	require.Zero(t, reads.Load(), "execution applies the frozen list instead of re-reading config")
	require.Nil(t, inst.frozenEnvPassthrough.Load(), "the freeze ends with the swap")
}

func TestAccountSwapRespawnAppliesThePassthroughItsAdmissionResolved(t *testing.T) {
	inst, _, newSessions, _ := flaggedPaneSwapInstanceForRespawn(t)

	require.NoError(t, inst.ValidateAccountSwap("work"), "admission reads a readable config")
	require.True(t, inst.accountSwapLaunch.passthroughFrozen, "the recorded plan freezes its pass-through list")
	_, err := inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	reads := unreadableAfterAdmission(inst)
	require.NoError(t, inst.RespawnForAccountSwap(),
		"the replacement pane set must not fail after teardown on a config read admission already answered")
	require.Greater(t, *newSessions, 0, "the replacement agent started")
	require.Zero(t, reads.Load(), "the respawn applies the frozen list instead of re-reading config")
	require.Nil(t, inst.frozenEnvPassthrough.Load(), "the freeze ends with the respawn")
}

// Outside a prepared plan nothing is frozen: an ordinary environment read still
// refuses on an unreadable config rather than answering a stale list.
func TestEnvPassthroughFreezeIsScopedToThePlan(t *testing.T) {
	inst, attempts := launchInstanceResolving(t, func(int32) (*config.ResolvedConfig, error) {
		return nil, unansweredProbe("/repo")
	})
	noRetryPause(t)

	release := inst.freezeEnvPassthrough([]string{"FROZEN_NAME"})
	names, err := SessionEnvPassthroughForInstance(inst)
	require.NoError(t, err)
	require.Equal(t, []string{"FROZEN_NAME"}, names)
	require.Zero(t, attempts.Load())

	release()
	_, err = SessionEnvPassthroughForInstance(inst)
	require.True(t, isProjectConfigUnknown(err), "after the plan, reads resolve — and refuse — as before")
}
