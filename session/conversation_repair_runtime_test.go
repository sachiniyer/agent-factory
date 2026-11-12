package session

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7129: what an operator repair's stamp may say, and about which runtime.
// The stamp is "what the runtime held when the repair was made"; it describes
// one runtime, the one live when the request began.

const repairOlderID = "01a1cc00-6666-7222-8333-444455556666"

// N-1: a runtime af launches after an operator repair did not exist when the
// repair was made, so it held nothing then, and whatever it holds is a move.
// The #7125 review's sequence: a restore keeps the repair but starts a fresh
// runtime (a could-not-look refusal, or the load path, which keeps the record
// on every refusal); in that pane the user resumes a thread through Codex's
// picker and works there. Neither the follow nor a blind restore may then
// treat that thread as the one the repair overrode — whether the repair was
// never observed (the follow used to take the new runtime's first answer as
// its baseline) or was observed holding that very thread in the old runtime
// (the stamp named it, so the follow and the bound both exempted it).
func TestOperatorRepairStampDescribesOnlyTheRuntimeItWasMadeAgainst(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	setup := func(t *testing.T) (workDir string, paths map[string]string) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir = t.TempDir()
		paths = map[string]string{
			heldTestOwnID: writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour)),
			repairHeldID:  writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), repairedAt.Add(-time.Minute)),
			repairOlderID: writeStoredCodexRollout(t, codexHome, repairOlderID, workDir, now.Add(-150*time.Minute), now.Add(-150*time.Minute), now.Add(-100*time.Minute)),
		}
		return workDir, paths
	}
	// resumedIn is the thread the user opens and writes to in the new pane.
	cases := []struct {
		name      string
		repair    AgentConversationData
		resumedIn string
	}{
		{"never observed, the pane resumes an older thread", operatorRecordAt(heldTestOwnID, repairedAt), repairOlderID},
		{"observed, the new pane resumes the thread the old one held", operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID), repairHeldID},
	}
	for _, c := range cases {
		t.Run(c.name+": the follow takes the move", func(t *testing.T) {
			workDir, paths := setup(t)
			inst := staleResumeInstance(t, workDir, c.repair)
			inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
			require.NoError(t, os.Chtimes(paths[c.resumedIn], now.Add(-10*time.Second), now.Add(-10*time.Second)))

			outcome := inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), inst.AgentConversation(), heldAnswer(c.resumedIn))
			require.Equal(t, ConversationFollowMoved, outcome, "a runtime launched after the repair held nothing at it")
			require.Equal(t, c.resumedIn, inst.AgentConversation().ID)
		})
		t.Run(c.name+": a blind restore refuses the repair", func(t *testing.T) {
			workDir, paths := setup(t)
			inst := staleResumeInstance(t, workDir, c.repair)
			inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
			require.NoError(t, os.Chtimes(paths[c.resumedIn], now.Add(-10*time.Second), now.Add(-10*time.Second)))

			restore := staleResumeInstance(t, workDir, inst.AgentConversation())
			program, ok, _ := prepareExactResumeConversation(restore, restore.Program)
			require.False(t, ok, "resumed %q over the thread the user moved to in the new runtime", program)
		})
	}

	// Control: in the runtime the repair was made against, working on in the
	// thread it held is not a move (#7120's standing case).
	t.Run("control: the same runtime keeps working in its thread", func(t *testing.T) {
		workDir, paths := setup(t)
		inst := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
		require.NoError(t, os.Chtimes(paths[repairHeldID], now.Add(-10*time.Second), now.Add(-10*time.Second)))

		require.Equal(t, ConversationFollowUnchanged,
			inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), inst.AgentConversation(), heldAnswer(repairHeldID)))
		program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "refused: %v", decline)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
	// Control: a launch boundary leaves a capture's record alone.
	t.Run("control: a capture's record is not touched by a launch", func(t *testing.T) {
		workDir, _ := setup(t)
		capture := AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestOwnID, CapturedAt: repairedAt, CaptureKind: ConversationCaptureCodexRollout}
		inst := staleResumeInstance(t, workDir, capture)
		inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
		require.Equal(t, capture, inst.AgentConversation())
	})
}

// N-2: the thread set-conversation saw the runtime hold is the repair's own,
// however late its rollout's first line was stamped. A fresh pane's first
// persist can land while the held walk runs, after the instant the repair is
// dated by; the walk still classifies the complete header and stamps that
// thread, the follow then reads the repair as standing, and the bound must
// agree rather than refuse the repair as stale and retire it. A follow's later
// baseline gets no such exemption: it is no observation of the repair, so a
// thread that entered the store after the repair is still a move (#7125
// review R-1).
func TestOperatorRepairExemptsTheObservedHeldThreadHoweverLateItWasStored(t *testing.T) {
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	setup := func(t *testing.T) string {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))
		// F: constructed before the look, first written 20ms after it.
		writeStoredCodexRollout(t, codexHome, repairNextID, workDir, at.Add(-3*time.Minute), at.Add(20*time.Millisecond), at.Add(20*time.Millisecond))
		return workDir
	}

	t.Run("observed at the repair", func(t *testing.T) {
		workDir := setup(t)
		repair := operatorRecordAt(heldTestOwnID, at.Add(2*time.Second)).WithRepairObservation(repairNextID, at)
		live := staleResumeInstance(t, workDir, repair)
		require.Equal(t, ConversationFollowUnchanged,
			live.FollowAgentConversationForRuntime(live.AgentRuntimeToken(), repair, heldAnswer(repairNextID)))

		restore := staleResumeInstance(t, workDir, repair)
		program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
		require.True(t, ok, "the repair's own held thread made it stale: %v (retire=%v)", decline, restore.exactResumeRefusal(decline).retire)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
	t.Run("control: a later baseline is no observation", func(t *testing.T) {
		workDir := setup(t)
		unstamped := operatorRecordAt(heldTestOwnID, at)
		inst := staleResumeInstance(t, workDir, unstamped)
		require.Equal(t, ConversationFollowBaselined,
			inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), unstamped, heldAnswer(repairNextID)))

		restore := staleResumeInstance(t, workDir, inst.AgentConversation())
		_, ok, _ := prepareExactResumeConversation(restore, restore.Program)
		require.False(t, ok, "F entered the store after the repair, and the baseline that names it was taken after it too")
	})
}
