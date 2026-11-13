package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7215: a handoff's incoming record must be durable before the incoming
// runtime can write anything. The daemon writes it from the plan's
// WithBeforeStart step, so the local backend must run that step after the
// outgoing pane is confirmed stopped and before the incoming one starts, and
// a step error must start nothing. The record it writes must name the incoming
// agent and its conversation, and claim no runtime: the outgoing launch it
// would otherwise carry is gone.

// beforeStartSwap is a codex session handing off to Claude through the real
// local plan and a tmux double that records whether the outgoing pane was
// stopped and which panes started.
type beforeStartSwap struct {
	inst    *Instance
	plan    AgentSwapPlan
	ptys    *recordingPtyFactory
	stopped *bool
}

func newBeforeStartSwap(t *testing.T) beforeStartSwap {
	t.Helper()
	agentHome(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	t.Setenv("CODEX_HOME", t.TempDir())
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	bin := t.TempDir()
	cfg := config.DefaultConfig()
	if cfg.ProgramOverrides == nil {
		cfg.ProgramOverrides = map[string]string{}
	}
	for _, agent := range []string{tmux.ProgramClaude, tmux.ProgramCodex} {
		stub := filepath.Join(bin, agent)
		require.NoError(t, os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o755))
		cfg.ProgramOverrides[agent] = stub
	}
	require.NoError(t, config.SaveConfig(cfg))

	ptys := &recordingPtyFactory{t: t}
	stopped := false
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "kill-session"):
				stopped = true
			case strings.Contains(joined, "has-session"):
				if stopped && len(ptys.cmds) == 0 {
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
	worktree := t.TempDir()
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktree, "before-start", "before-start-branch", "", false, false)
	require.NoError(t, err)
	ts := tmux.NewTmuxSessionWithDeps("before-start", tmux.ProgramCodex, ptys, cmdExec)
	backend := &LocalBackend{}
	inst := &Instance{
		ID: "before-start-id", Title: "before-start", Path: repoRoot, Program: tmux.ProgramCodex,
		backend: backend, Tabs: []*Tab{newAgentTab(ts)}, gitWorktree: gw, started: true, liveness: LiveRunning,
		// The outgoing runtime's launch claim, which the stop makes stale.
		runtimeProgram: tmux.ProgramCodex, runtimePID: 4242, runtimeStartID: 99,
	}
	require.True(t, inst.SetAgentConversation(AgentConversationData{
		Agent: tmux.ProgramCodex, ID: "019a0000-0000-7000-8000-000000000001", CaptureKind: ConversationCaptureCodexRollout,
	}))

	plan, err := backend.PrepareAgentSwap(inst, tmux.ProgramClaude)
	require.NoError(t, err)
	require.True(t, plan.conversation.HasID(), "precondition: a handoff to Claude names its conversation")
	require.NoError(t, inst.Transition(BeginHandoff()))
	_, err = inst.RecordHandoffSwap(tmux.ProgramClaude, plan.EffectiveAgent(), HandoffReasonManual, "", false)
	require.NoError(t, err)
	return beforeStartSwap{inst: inst, plan: plan, ptys: ptys, stopped: &stopped}
}

func TestHandoffRecordBeforeStartNamesTheIncomingAgentOnceTheOutgoingStopped(t *testing.T) {
	f := newBeforeStartSwap(t)
	var atStep InstanceData
	ran := false
	plan := f.plan.WithBeforeStart(func() error {
		ran = true
		require.True(t, *f.stopped, "the step runs only after the outgoing pane is confirmed stopped")
		require.Empty(t, f.ptys.cmds, "the step runs before the incoming pane starts")
		atStep = f.inst.HandoffCheckpointBeforeStart()
		return nil
	})

	_, err := f.inst.SwapAgent(plan)
	require.NoError(t, err)
	require.True(t, ran, "the local backend runs the step on every swap")
	require.NotEmpty(t, f.ptys.cmds, "the incoming pane starts after the step")

	require.Equal(t, tmux.ProgramClaude, atStep.Program)
	require.Equal(t, f.plan.conversation.ID, persistedConversationOf(atStep).ID,
		"the record names the conversation the incoming pane starts on")
	require.Empty(t, atStep.RuntimeProgram, "the outgoing runtime's launch claim is gone with it")
	require.Zero(t, atStep.RuntimePID)
	require.Zero(t, atStep.RuntimeStartID)

	// A crash past the start loads this record. With no launch claim recorded,
	// load's reattach has nothing to distrust and keeps the live pane as
	// Program's agent (LocalBackend.launch's claim-gated verification), and
	// the agent resolver, given no runtime evidence, answers Program too.
	require.Equal(t, tmux.ProgramClaude, tmux.DetectAgentFromCommand(atStep.Program))
}

func TestHandoffRecordBeforeStartFailureStartsNothing(t *testing.T) {
	f := newBeforeStartSwap(t)
	plan := f.plan.WithBeforeStart(func() error { return errors.New("disk is full") })

	_, err := f.inst.SwapAgent(plan)
	require.ErrorContains(t, err, "disk is full")
	require.True(t, *f.stopped, "precondition: the outgoing pane was stopped")
	require.Empty(t, f.ptys.cmds, "a refused step must start no incoming pane")
}
