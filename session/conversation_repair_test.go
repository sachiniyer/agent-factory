package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The #7120 review's operator-repair rules, at the session layer: a
// set-conversation repair stands — through the follow check, a restore, and a
// handoff's carry — until the runtime moves to a thread it did not hold when
// the repair was made.

const (
	repairHeldID = "01a1ccd0-1111-7222-8333-444455556666"
	repairNextID = "01a1cce0-1111-7222-8333-444455556666"
)

func operatorRecord(id string) AgentConversationData {
	return AgentConversationData{Agent: tmux.ProgramCodex, ID: id, CapturedAt: time.Now(), CaptureKind: ConversationCaptureOperator}
}

func heldAnswer(id string) AgentConversationData {
	if id == "" {
		return AgentConversationData{}
	}
	return AgentConversationData{Agent: tmux.ProgramCodex, ID: id, CaptureKind: ConversationCaptureCodexRollout}
}

// TestOperatorRepairStampedAtCommitStandsUntilTheRuntimeMoves: the daemon
// stamps what the runtime held when the repair landed; the follow may only
// replace the repair with a thread held after it.
func TestOperatorRepairStampedAtCommitStandsUntilTheRuntimeMoves(t *testing.T) {
	inst := retireTestInstance(t)
	token := inst.AgentRuntimeToken()
	repair := operatorRecord(heldTestOwnID)
	inst.SetAgentConversation(repair)
	inst.StampConversationRepair(repair.ID, repairHeldID)

	for check := 0; check < 3; check++ {
		require.False(t, inst.FollowAgentConversationForRuntime(token, repair, heldAnswer(repairHeldID)),
			"the thread the runtime already held at the repair never replaces it")
	}
	require.False(t, inst.FollowAgentConversationForRuntime(token, repair, heldAnswer("")),
		"a runtime holding nothing has nothing to move to")
	require.True(t, inst.FollowAgentConversationForRuntime(token, repair, heldAnswer(repairNextID)),
		"a thread the runtime moved to after the repair replaces it")
	require.Equal(t, repairNextID, inst.AgentConversation().ID)
	require.Equal(t, ConversationCaptureCodexRollout, inst.AgentConversation().CaptureKind,
		"once followed, the record is an ordinary capture again")
}

// TestOperatorRepairWithoutAStampTakesItsFirstObservationAsBaseline is the
// restart shape: the stamp is in memory only, so after a daemon restart (or a
// repair whose held query failed) the first check of the repair is its
// baseline and moves nothing.
func TestOperatorRepairWithoutAStampTakesItsFirstObservationAsBaseline(t *testing.T) {
	inst := retireTestInstance(t)
	token := inst.AgentRuntimeToken()
	repair := operatorRecord(heldTestOwnID)
	inst.SetAgentConversation(repair)

	require.False(t, inst.ConversationFollowPermitted(token, repair, heldAnswer(repairHeldID)),
		"the first observation after a restart is the repair's baseline")
	require.False(t, inst.FollowAgentConversationForRuntime(token, repair, heldAnswer(repairHeldID)))
	require.Equal(t, heldTestOwnID, inst.AgentConversation().ID)
	require.True(t, inst.FollowAgentConversationForRuntime(token, repair, heldAnswer(repairNextID)))
	require.Equal(t, repairNextID, inst.AgentConversation().ID)
}

// TestCapturedRecordStillFollowsTheRuntimeImmediately: the stickiness is the
// operator's, not every record's — a capture's own record follows the first
// time the runtime holds another thread.
func TestCapturedRecordStillFollowsTheRuntimeImmediately(t *testing.T) {
	inst := retireTestInstance(t)
	token := inst.AgentRuntimeToken()
	captured := inst.AgentConversation()
	require.NotEqual(t, ConversationCaptureOperator, captured.CaptureKind)
	require.True(t, inst.FollowAgentConversationForRuntime(token, captured, heldAnswer(repairNextID)))
	require.Equal(t, repairNextID, inst.AgentConversation().ID)
}

// TestExactResumeKeepsAStandingOperatorRepair: a repair to an OLDER thread
// than the one the pane last wrote is exactly what an operator does after a
// fresh restore, and the restore guard must resume it rather than refuse it as
// stale. A capture's record in the same store is still refused.
func TestExactResumeKeepsAStandingOperatorRepair(t *testing.T) {
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	repaired := writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl", workDir)
	writeCodexRolloutFileWithCwd(t, codexHome, "rollout-2026-11-11T14-42-50-"+repairHeldID+".jsonl", workDir)
	markRolloutOlder(t, repaired)

	repair := staleResumeInstance(t, workDir, operatorRecord(heldTestOwnID))
	assert.Contains(t, prepareResumeConversation(repair, repair.Program), "resume "+heldTestOwnID,
		"a standing operator repair is the conversation the next restore resumes")

	captured := staleResumeInstance(t, workDir, AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestOwnID, CaptureKind: ConversationCaptureCodexRollout})
	assert.Equal(t, captured.Program, prepareResumeConversation(captured, captured.Program),
		"a capture's own stale record is still refused")
}

// TestCarryKeepsAStandingOperatorRepair: the handoff's carry applies the same
// rule as the restore guard.
func TestCarryKeepsAStandingOperatorRepair(t *testing.T) {
	past := time.Now().Add(-time.Hour)
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	require.True(t, inst.SetAgentConversation(AgentConversationData{
		Agent: tmux.ProgramCodex, ID: carryTestID, CapturedAt: time.Now(), CaptureKind: ConversationCaptureOperator,
	}))
	source := filepath.Join(home, ".codex")
	recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID), codexRollout(inst.GetWorktreePath()))
	newer := writeCarryFile(t, source, codexRolloutRel(newerConversationID), codexRollout(inst.GetWorktreePath()))
	setModTime(t, recorded, past)
	setModTime(t, newer, past.Add(time.Minute))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry, "a standing operator repair is carried, not refused as stale")
}

// TestExactResumeRefusalNamesAnUnexpressibleRecord is the review's F6: an id
// that IS recorded but that the launch command cannot express (a recorded
// conversation of another agent) gets a reason too, so the fresh-start log
// never claims nothing was recorded — and it is not retired.
func TestExactResumeRefusalNamesAnUnexpressibleRecord(t *testing.T) {
	inst := retireTestInstance(t)
	inst.SetAgentConversation(AgentConversationData{Agent: tmux.ProgramClaude, ID: heldTestOwnID})

	refusal := inst.exactResumeRefusal("")
	require.Equal(t, heldTestOwnID, refusal.ID)
	require.Contains(t, refusal.Reason, "claude")
	require.False(t, refusal.retire, "an id the command cannot express is reported, not retired")
	require.NotContains(t, tmux.ExactResumeFreshStartMessage("af_x", refusal.ExactResumeRefusal), "no recorded conversation id")

	require.Equal(t, resumeRefusal{}, (&Instance{Tabs: []*Tab{{}}}).exactResumeRefusal(""),
		"nothing recorded is the one case with nothing to say")
}

// TestUnderDirectoryIdentityMatchesByInode: store containment falls back to
// (device, inode) ancestry, so a store reached under another spelling still
// contains its rollouts (#7120 review F7).
func TestUnderDirectoryIdentityMatchesByInode(t *testing.T) {
	store := t.TempDir()
	sessions := filepath.Join(store, "sessions")
	day := filepath.Join(sessions, "2026", "11", "11")
	require.NoError(t, os.MkdirAll(day, 0o755))
	rollout := filepath.Join(day, "rollout-2026-11-11T13-50-27-"+heldTestOwnID+".jsonl")
	require.NoError(t, os.WriteFile(rollout, []byte("{}\n"), 0o644))
	id, ok := fileIdentityOf(sessions)
	require.True(t, ok)

	require.True(t, underDirectoryIdentity(rollout, id))
	elsewhere := filepath.Join(t.TempDir(), "rollout.jsonl")
	require.NoError(t, os.WriteFile(elsewhere, []byte("{}\n"), 0o644))
	require.False(t, underDirectoryIdentity(elsewhere, id))
}
