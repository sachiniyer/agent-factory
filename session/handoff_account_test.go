package session

import (
	"strings"
	"testing"
	"time"

	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

func TestHandoffAccountPreservesCustomProgram(t *testing.T) {
	inst := handoffTestInstance(t, "claude")
	inst.Program = "claude --model opus"
	inst.Account = "work"
	require.NoError(t, inst.BeginManualAccountSwap())
	entry, err := inst.SelectAccountForHandoff("work", "personal", "claude", "claude", false, HandoffReasonManual, "tip", "continue")
	require.NoError(t, err)
	require.Equal(t, "claude --model opus", inst.AgentProgram())
	require.Equal(t, "claude --model opus", inst.ToInstanceData().Program)
	require.Equal(t, "claude", entry.From.Agent)
	require.Equal(t, "claude", entry.To)
	require.Equal(t, "work", entry.FromAccount)
	require.Equal(t, "personal", entry.ToAccount)
	require.Len(t, inst.Handoffs(), 1)
	require.NoError(t, inst.RevertHandoff(entry))
	require.Equal(t, "claude --model opus", inst.AgentProgram())
}

func TestPendingManualAccountSwapDeliveryEvidenceIsMissionScoped(t *testing.T) {
	inst := &Instance{
		liveness: LiveRunning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
	}
	require.NoError(t, inst.RecordPendingManualAccountSwapMissionDelivery(
		"work", "personal", PromptCouldNotConfirm,
	))
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed())

	inst.RecordPromptAttempt(PromptNotDelivered, time.Now())
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"an unrelated session prompt must not authorize redelivery of the handoff mission")
	require.Equal(t, PromptCouldNotConfirm, inst.ToInstanceData().PendingAccountSwap.MissionDeliveryStatus)
}

func TestStartupUnknownManualAccountSwapDoesNotExposeRetry(t *testing.T) {
	inst := &Instance{
		liveness:            LiveRunning,
		startupStateUnknown: true,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
	require.True(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"startup uncertainty must not erase the mission's ambiguous verdict")
	require.False(t, inst.CanRetryPendingManualAccountSwapDelivery(),
		"an unknown replacement runtime must stay inert instead of advertising a delivery retry")
}

func TestUnavailableManualAccountSwapDoesNotExposeRetry(t *testing.T) {
	for _, tc := range []struct {
		name       string
		liveness   Liveness
		inFlightOp InFlightOp
		userKilled bool
	}{
		{name: "retry in progress", liveness: LiveRunning, inFlightOp: OpRespawning},
		{name: "kill tombstone", liveness: LiveReady, userKilled: true},
		{name: "lost", liveness: LiveLost},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst := &Instance{
				liveness:   tc.liveness,
				inFlightOp: tc.inFlightOp,
				userKilled: tc.userKilled,
				pendingAccountSwap: &AccountSwapData{
					Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
					MissionDeliveryStatus: PromptCouldNotConfirm,
				},
			}
			require.False(t, inst.CanRetryPendingManualAccountSwapDelivery())
		})
	}
}

func TestRestoreLegacyAccountSwapDoesNotTrustGenericDeliveryEvidence(t *testing.T) {
	data := InstanceData{
		PendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
		LastPromptAttemptAt:      time.Now(),
		LastPromptDeliveryStatus: PromptNotDelivered,
	}
	restored := data.restoreMissingAccountSwapMissionEvidence()
	require.Equal(t, PromptCouldNotConfirm, restored.PendingAccountSwap.MissionDeliveryStatus,
		"session-wide non-delivery may belong to another prompt and cannot authorize mission redelivery")
	require.Empty(t, data.PendingAccountSwap.MissionDeliveryStatus,
		"migration must not mutate the caller's checkpoint")
}

func TestRestoreAccountSwapMissingMissionEvidenceFailsClosed(t *testing.T) {
	data := InstanceData{
		PendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
		},
	}
	restored := data.restoreMissingAccountSwapMissionEvidence()
	require.Equal(t, PromptCouldNotConfirm, restored.PendingAccountSwap.MissionDeliveryStatus,
		"missing durable evidence after replacement startup must not authorize mission redelivery")
}

func TestParkManualAccountSwapRecordsMissionNonDelivery(t *testing.T) {
	inst := &Instance{
		Program:    "claude",
		Account:    "personal",
		liveness:   LiveRunning,
		inFlightOp: OpRespawning,
		pendingAccountSwap: &AccountSwapData{
			Manual: true, From: "work", To: "personal", ReplacementPanesStarted: true,
			MissionDeliveryStatus: PromptCouldNotConfirm,
		},
	}
	require.NoError(t, inst.ParkManualAccountSwapAtLimit(time.Now().Add(time.Hour)))
	require.Equal(t, PromptNotDelivered, inst.ToInstanceData().PendingAccountSwap.MissionDeliveryStatus)
	require.False(t, inst.PendingManualAccountSwapDeliveryUnconfirmed(),
		"an incoming limit observed before submission must retain scheduled recovery")
}

// A committed AUTOMATIC swap's retry resolves the account in the namespace the
// commit recorded — for a legacy pending record that namespace is the durable
// accountAgent pin, not a fresh resolution under overrides that may have moved
// since (#4430 review round 6). With claude→codex at commit and claude→gemini
// after, resolving fresh would look "personal" up in gemini's registry — a
// different account of the same name — while the pin keeps codex's.
func TestValidateAccountSwap_CommittedAutoRetryUsesThePinnedNamespace(t *testing.T) {
	saveProgramOverrides(t, map[string]string{tmux.ProgramClaude: tmux.ProgramGemini})
	inst := accountSwapTestInstance(tmux.ProgramClaude)
	inst.Tabs = []*Tab{newAgentTab(tmux.NewTmuxSession("swap", tmux.ProgramCodex))}
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	registerAccount(t, tmux.ProgramCodex, "personal")
	// The committed transaction: the identity checkpoint already moved the
	// session to "personal" inside codex's registry, and the durable pin is the
	// only namespace record — the pending entry is the legacy shape with none.
	inst.Account, inst.accountAutoSelected = "personal", true
	inst.accountAgent = tmux.ProgramCodex
	inst.pendingAccountSwap = &AccountSwapData{From: "work", To: "personal"}

	require.NoError(t, inst.ValidateAccountSwap("personal"),
		"the retry must resolve personal inside the pinned codex namespace")
	require.NotNil(t, inst.accountSwapLaunch)
	require.True(t, strings.HasPrefix(inst.accountSwapLaunch.program, tmux.ProgramCodex),
		"the retry launches the committed codex command, got %q", inst.accountSwapLaunch.program)
}
