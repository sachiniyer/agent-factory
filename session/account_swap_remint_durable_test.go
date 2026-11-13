package session

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
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
	require.True(t, inst.pendingAccountSwapUnwrittenFor("work"), "precondition: the re-plan minted a fresh conversation")
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

// #7217 review R1, probe A: the pre-spawn write of a re-minted conversation
// fails once, and memory keeps the minted id. The retry re-plans on that id,
// which is unfiled and so starts on it with no fresh re-mint; the record is
// still the one disk does not hold, and it must be written before the spawn.
func TestARemintRetryAfterAFailedWriteStillWritesBeforeTheSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	inst, minted, spawns := remintedCommittedSwap(t)
	inst.SetRelocationSettleNotify(func(*Instance) error { return errors.New("disk is full") })
	require.ErrorContains(t, inst.RespawnForAccountSwap(), "disk is full")
	require.Empty(t, *spawns)
	require.Equal(t, minted, inst.ToInstanceData().PendingAccountSwap.ConversationID,
		"precondition: memory still names the minted id after the failed write")

	var writes []string
	inst.SetRelocationSettleNotify(func(i *Instance) error {
		if len(*spawns) == 0 {
			writes = append(writes, i.ToInstanceData().PendingAccountSwap.ConversationID)
		}
		return nil
	})
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.RespawnForAccountSwap())
	require.Equal(t, []string{minted}, writes, "the retry writes the minted id before its spawn")
	require.NotEmpty(t, *spawns)
}

// #7217 review R1, probe B: the ordinary route for a carry that launched and
// died. revalidateGoneAccountSwap demotes the carry to a fresh id F
// (AbandonCarriedConversationAfterFailedLaunch) and then re-plans
// (ValidateAccountSwap), and the re-plan must not lose the demotion's
// obligation to reach disk before the spawn.
func TestACarryDemotedOnRevalidateIsWrittenBeforeTheSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	home := carryAmbientProviderHomes(t)
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramClaude, "work")

	const agentName = "af_remint_carry_demote"
	const processName = agentName + "__build"
	var newSessions int
	executor := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName, executor)
	processSiblingForSwap(restored)
	restored.mu.Lock()
	restored.Tabs = append(restored.Tabs, &Tab{
		ID: "build", Name: "build", Kind: TabKindShell, Command: "/bin/sh",
		tmux: tmux.NewTmuxSessionFromSanitizedNameWithDeps(processName, "/bin/sh",
			failAccountSwapProcessPty{t: t, cmdExec: executor, name: processName}, executor),
	})
	restored.mu.Unlock()
	restored.Path = initTempGitRepo(t)
	recordOutgoingConversation(t, restored, tmux.ProgramClaude, carryTestID)
	writeCarryFile(t, filepath.Join(home, ".claude"), claudeTranscriptRel(restored.GetWorktreePath(), carryTestID), "{}\n")
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err := restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	require.Error(t, restored.RespawnForAccountSwap(), "precondition: the carried launch dies")
	require.True(t, restored.ToInstanceData().PendingAccountSwap.CarriedLaunchStarted)

	// revalidateGoneAccountSwap, daemon/account_swap.go.
	require.NoError(t, restored.AbandonCarriedConversationAfterFailedLaunch("work"))
	require.NoError(t, restored.ValidateAccountSwap("work"))
	fresh := restored.ToInstanceData().PendingAccountSwap.ConversationID
	require.NotEmpty(t, fresh, "precondition: the demotion names a fresh conversation")

	before := newSessions
	var written []*AccountSwapData
	restored.SetRelocationSettleNotify(func(i *Instance) error {
		if newSessions == before {
			written = append(written, i.ToInstanceData().PendingAccountSwap)
		}
		return nil
	})
	_ = restored.RespawnForAccountSwap()
	require.NotEmpty(t, written, "the demoted record is written before the replacement's first new-session")
	require.Equal(t, fresh, written[0].ConversationID)
	require.Empty(t, written[0].CarriedConversationID, "the durable record no longer names the abandoned carry")
}
