package session

import (
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func accountSwapTestInstance(program string) *Instance {
	return &Instance{
		ID:         "swap-id",
		Title:      "swap",
		Path:       "/repo",
		Program:    program,
		backend:    &LocalBackend{},
		started:    true,
		liveness:   LiveLimitReached,
		inFlightOp: OpRespawning,
		Tabs:       []*Tab{newAgentTab(tmux.NewTmuxSession("swap", program))},
	}
}

func TestValidateAccountSwapRefusesConversationSelectors(t *testing.T) {
	for _, tc := range []struct {
		program string
		arg     string
	}{
		{"claude --continue", "--continue"},
		{"claude --resume old-chat", "--resume old-chat"},
		{"claude -r=old-chat", "-r=old-chat"},
		{"claude --session-id old-chat", "--session-id old-chat"},
		{"codex resume old-chat", "resume old-chat"},
		{"codex exec resume --last --model gpt-5", "resume --last"},
	} {
		t.Run(tc.program, func(t *testing.T) {
			err := accountSwapTestInstance(tc.program).ValidateAccountSwap("work")
			require.ErrorContains(t, err, "fresh conversation")
			require.ErrorContains(t, err, tc.arg, "the refusal must name the user-pinned selector")
		})
	}
}

func TestSelectAccountAutomaticallyClearsPriorConversationAndCapture(t *testing.T) {
	inst := accountSwapTestInstance("codex")
	prior := AgentConversationData{Agent: tmux.ProgramCodex, ID: "old-rollout"}
	require.True(t, inst.SetAgentConversation(prior))
	oldRuntime := inst.AgentRuntimeToken()

	_, err := inst.SelectAccountAutomatically("ambient", "work")
	require.NoError(t, err)
	require.True(t, inst.AgentConversation().Empty(),
		"the old account's conversation identity must not survive the durable account boundary")
	require.False(t, inst.SetAgentConversationForRuntime(oldRuntime,
		AgentConversationData{Agent: tmux.ProgramCodex, ID: "late-old-rollout"}),
		"an asynchronous capture from the stopped runtime must not restore its conversation")
}

func TestValidateAccountSwapRefusesSiblingIdentityOverride(t *testing.T) {
	inst := accountSwapTestInstance("codex")
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess,
		tmux: tmux.NewTmuxSession("build", "CODEX_HOME=/other make"),
	})

	err := inst.ValidateAccountSwap("work")
	require.ErrorContains(t, err, `tab "build"`)
	require.ErrorContains(t, err, "overrides the account directory")
}

func TestStopForAccountSwapStopsEveryCredentialBearingPane(t *testing.T) {
	var mu sync.Mutex
	var killed []string
	inner := nameKeyedExec(map[string]bool{
		"af_swap":        true,
		"af_swap__shell": true,
		"af_swap__build": true,
	})
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				mu.Lock()
				killed = append(killed, strings.Join(cmd.Args, " "))
				mu.Unlock()
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	inst := lostInstanceForRecover(t, "af_swap", "af_swap__shell", cmdExec)
	inst.mu.Lock()
	inst.Tabs = append(inst.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindProcess, Command: "make",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps("af_swap__build", "make", nil, cmdExec),
	})
	inst.mu.Unlock()
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())

	require.NoError(t, inst.StopForAccountSwap())
	mu.Lock()
	joined := strings.Join(killed, "\n")
	mu.Unlock()
	for _, name := range []string{"af_swap", "af_swap__shell", "af_swap__build"} {
		require.Contains(t, joined, name, "every credential-bearing pane must be stopped before identity commit")
	}
}

func TestPendingAccountSwapFencesArchiveAndHandoffButAllowsDelivery(t *testing.T) {
	newPending := func() *Instance {
		inst := accountSwapTestInstance("claude")
		_, err := inst.SelectAccountAutomatically("ambient", "work")
		require.NoError(t, err)
		inst.inFlightOp = OpNone
		return inst
	}

	archive := newPending()
	require.Equal(t, LifecycleActionNone, archive.LifecycleAction())
	require.ErrorContains(t, archive.Transition(BeginArchive()), "account swap")

	handoff := newPending()
	require.ErrorContains(t, handoff.ValidateRuntimeAction(RuntimeActionHandoff), "account swap")
	require.NoError(t, handoff.ValidateRuntimeAction(RuntimeActionResumeLimit),
		"the pending notice must remain deliverable")
}
