package session

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7216 review R2: a committed swap whose re-launch starts fresh on a newly
// minted id N (its committed conversation was superseded in an af-owned
// worktree) names N in its record and pending swap. That must be durable
// before the pane can write under N, through the respawn's persist hook: it
// used to reach disk only at ConfirmLive, after the spawn, so a daemon-only
// crash in that window delivered the mission into N while the record still
// named the committed id.

// remintedCommittedSwap is a committed fresh Claude swap whose re-plan minted
// a new conversation, with a tmux double recording the replacement's spawn.
func remintedCommittedSwap(t *testing.T) (*Instance, string, *[]string) {
	t.Helper()
	inst, target, worktree, committed := committedFreshClaudeSwapIn(t, exclusiveLayout)
	fileClaudeTranscript(t, target, worktree, committed, time.Now().Add(-time.Hour))
	fileClaudeTranscript(t, target, worktree, "7c1d2e3f-4a5b-4c6d-8e7f-901234567890", time.Now().Add(-time.Minute))

	var newSessions int
	spawns := &[]string{}
	exec := recordingExec(map[string]bool{}, &newSessions, spawns)
	ts := tmux.NewTmuxSessionWithDeps(inst.Title, tmux.ProgramClaude, persistPtyFactory{t: t, cmdExec: exec}, exec)
	inst.mu.Lock()
	inst.Tabs = []*Tab{newAgentTab(ts)}
	inst.mu.Unlock()

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.True(t, plan.reminted, "precondition: the re-plan minted a fresh conversation")
	require.NotEqual(t, committed, plan.conversation.ID)
	return inst, plan.conversation.ID, spawns
}

func TestCommittedSwapRemintIsDurableBeforeTheSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	inst, minted, spawns := remintedCommittedSwap(t)

	type write struct {
		pending, tab string
		spawned      int
	}
	var writes []write
	inst.SetRelocationSettleNotify(func(i *Instance) error {
		data := i.ToInstanceData()
		writes = append(writes, write{data.PendingAccountSwap.ConversationID, persistedConversationOf(data).ID, len(*spawns)})
		return nil
	})
	require.NoError(t, inst.RespawnForAccountSwap())

	require.NotEmpty(t, writes, "the re-planned record is written before the spawn")
	require.Equal(t, write{pending: minted, tab: minted, spawned: 0}, writes[0],
		"the durable record names the minted conversation before any pane starts")
	var agentSpawn string
	for _, spawn := range *spawns {
		if strings.Contains(spawn, "--session-id") {
			agentSpawn = spawn
		}
	}
	require.Contains(t, agentSpawn, "--session-id "+minted, "the replacement runs the conversation the record names")
}

func TestCommittedSwapRemintThatCannotBeRecordedSpawnsNothing(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	inst, _, spawns := remintedCommittedSwap(t)
	inst.SetRelocationSettleNotify(func(*Instance) error { return errors.New("disk is full") })

	require.ErrorContains(t, inst.RespawnForAccountSwap(), "disk is full")
	require.Empty(t, *spawns, "nothing starts that the durable record does not name")
}
