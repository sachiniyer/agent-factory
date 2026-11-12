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
	// writtenInTheNewPane dates the user's write after the launch: a second
	// past the instant the launch recorded, so no clock or filesystem
	// granularity can put it at or before the launch.
	writtenInTheNewPane := func(t *testing.T, inst *Instance, path string) {
		t.Helper()
		replacedAt := inst.AgentConversation().RepairRuntimeReplacedAt
		require.False(t, replacedAt.IsZero(), "the launch must record that the repair's runtime was replaced")
		written := replacedAt.Add(time.Second)
		require.NoError(t, os.Chtimes(path, written, written))
	}
	for _, c := range cases {
		t.Run(c.name+": the follow takes the move", func(t *testing.T) {
			workDir, paths := setup(t)
			inst := staleResumeInstance(t, workDir, c.repair)
			inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
			writtenInTheNewPane(t, inst, paths[c.resumedIn])

			outcome := inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), inst.AgentConversation(), heldAnswer(c.resumedIn))
			require.Equal(t, ConversationFollowMoved, outcome, "a runtime launched after the repair held nothing at it")
			require.Equal(t, c.resumedIn, inst.AgentConversation().ID)
		})
		t.Run(c.name+": a blind restore refuses the repair", func(t *testing.T) {
			workDir, paths := setup(t)
			inst := staleResumeInstance(t, workDir, c.repair)
			inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
			writtenInTheNewPane(t, inst, paths[c.resumedIn])

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

// N-1 across a restart: the launch's record that the repair's runtime was
// replaced is part of the persisted record, with the stamp it leaves in
// place, so a daemon that restarts after af launched the new pane still
// reads its threads as moves rather than baselining one of them.
func TestOperatorRepairRestampedAtALaunchSurvivesADaemonRestart(t *testing.T) {
	for _, repair := range []AgentConversationData{
		operatorRecord(heldTestOwnID),
		operatorRecord(heldTestOwnID).WithRepairStamp(repairHeldID),
	} {
		inst := retireTestInstance(t)
		require.True(t, inst.SetAgentConversation(repair))
		inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
		launched := inst.AgentConversation()
		require.False(t, launched.RepairRuntimeReplacedAt.IsZero(), "the launch must mark the standing repair")

		restarted := reloadedAgentConversation(t, inst)
		conv := restarted.AgentConversation()
		require.Equal(t, ConversationCaptureOperator, conv.CaptureKind, "the repair itself still stands")
		require.True(t, conv.RepairRuntimeReplacedAt.Equal(launched.RepairRuntimeReplacedAt))
		require.Equal(t, repair.RepairStamped, conv.RepairStamped, "the old runtime's stamp is kept")
		require.Equal(t, repair.RepairHeldID, conv.RepairHeldID)
		require.Equal(t, repair.RepairHeldObserved, conv.RepairHeldObserved)
		for _, moved := range []string{repairOlderID, repairHeldID} {
			require.Equal(t, ConversationFollowMoved,
				restarted.FollowAgentConversationForRuntime(restarted.AgentRuntimeToken(), conv, heldAnswer(moved)),
				"the new runtime held nothing at the repair, so %s is a move", moved)
			restarted = reloadedAgentConversation(t, inst)
		}
	}
}

// N-3: what a baseline means. When the request could not see the pane, the
// follow's first look stands in for it, and from then on every other thread
// written to since the repair counts as a move, as it does for an observed
// repair. Before that look, a pre-repair thread the pane is writing to is
// still disregarded (an unobserved repair cannot tell the pane's thread from
// the others). The set-conversation help text says the same.
func TestOperatorRepairBaselineCountsThreadsWrittenSinceTheRepairAsMoves(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour))
	// H: stored before the repair, written to since.
	writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, repairedAt.Add(-time.Hour), repairedAt.Add(-time.Hour), now.Add(-time.Minute))
	unobserved := operatorRecordAt(heldTestOwnID, repairedAt)

	before := staleResumeInstance(t, workDir, unobserved)
	_, ok, decline := prepareExactResumeConversation(before, before.Program)
	require.True(t, ok, "before af has read the pane: refused %v", decline)

	inst := staleResumeInstance(t, workDir, unobserved)
	require.Equal(t, ConversationFollowBaselined,
		inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), unobserved, heldAnswer("")))
	baselined := inst.AgentConversation()
	require.True(t, baselined.RepairStamped)
	require.False(t, baselined.RepairHeldObserved, "a baseline is not the request's observation")
	require.Equal(t, repairedAt, baselined.CapturedAt, "the repair keeps its own date")

	after := staleResumeInstance(t, workDir, baselined)
	_, ok, _ = prepareExactResumeConversation(after, after.Program)
	require.False(t, ok, "once read, a thread written to since the repair that the pane is not on is a move")
}

// N-5: disregards now settles a stamped window's written-since test, which
// needs only the name and mtime, before reading the rollout's header. That
// order cannot be seen in its answer, so this pins the answer for every
// window kind and branch, which the reordering must keep.
func TestOperatorRepairWindowVerdicts(t *testing.T) {
	now := time.Now()
	at := now.Add(-10 * time.Minute)
	pre, post := at.Add(-time.Hour), at.Add(time.Minute)
	codexHome := t.TempDir()
	cwd := t.TempDir()
	const otherPre, otherPost, undatable = "01a1cc00-7777-7222-8333-444455556666", "01a1cc00-8888-7222-8333-444455556666", "01a1cc00-9999-7222-8333-444455556666"
	paths := map[string]string{
		"H stored before":      writeStoredCodexRollout(t, codexHome, repairHeldID, cwd, pre, pre, pre),
		"X stored before":      writeStoredCodexRollout(t, codexHome, otherPre, cwd, pre, pre, pre),
		"X stored after":       writeStoredCodexRollout(t, codexHome, otherPost, cwd, pre, post, post),
		"X with no line stamp": writeStoredCodexRollout(t, codexHome, undatable, cwd, pre, pre, pre),
	}
	// The undatable rollout: rewrite its first line without the line stamp.
	require.NoError(t, os.WriteFile(paths["X with no line stamp"],
		[]byte(`{"type":"session_meta","payload":{"id":"`+undatable+`","cwd":"`+cwd+`","source":"cli"}}`+"\n"), 0o644))
	hStoredAfter := t.TempDir()
	paths["H stored after"] = writeStoredCodexRollout(t, hStoredAfter, repairHeldID, cwd, pre, post, post)
	// #7132 review F-5: the observed held thread with no datable first line.
	hUndatable := t.TempDir()
	paths["H with no line stamp"] = writeStoredCodexRollout(t, hUndatable, repairHeldID, cwd, pre, pre, pre)
	require.NoError(t, os.WriteFile(paths["H with no line stamp"],
		[]byte(`{"type":"session_meta","payload":{"id":"`+repairHeldID+`","cwd":"`+cwd+`","source":"cli"}}`+"\n"), 0o644))

	// The runtime the repair was made against is replaced between the
	// repair and post; between is a write the old runtime made before that.
	replacedAt, between := at.Add(30*time.Second), at.Add(10*time.Second)
	record := operatorRecordAt(heldTestOwnID, at)
	windows := map[string]operatorRepairWindow{
		"capture":             operatorRepairWindowOf(AgentConversationData{Agent: tmux.ProgramCodex, ID: heldTestOwnID, CapturedAt: at, CaptureKind: ConversationCaptureCodexRollout}, now),
		"unstamped":           operatorRepairWindowOf(record, now),
		"observed H":          operatorRepairWindowOf(record.WithRepairStamp(repairHeldID), now),
		"baseline H":          operatorRepairWindowOf(record.withRepairBaseline(repairHeldID), now),
		"observed H replaced": operatorRepairWindowOf(record.WithRepairStamp(repairHeldID).WithRepairRuntimeReplaced(replacedAt), now),
		"unstamped replaced":  operatorRepairWindowOf(record.WithRepairRuntimeReplaced(replacedAt), now),
	}
	cases := []struct {
		window, path string
		modified     time.Time
		want         bool
	}{
		{"capture", "X stored before", pre, false},
		{"unstamped", "X stored before", post, true},
		{"unstamped", "X stored after", post, false},
		{"unstamped", "X with no line stamp", pre, false},
		{"observed H", "H stored after", post, true},
		{"observed H", "H stored before", post, true},
		{"observed H", "X stored before", pre, true},
		{"observed H", "X stored before", post, false},
		{"observed H", "X stored after", post, false},
		{"observed H", "X with no line stamp", pre, false},
		{"baseline H", "H stored before", post, true},
		{"baseline H", "H stored after", post, false},
		{"baseline H", "X stored before", pre, true},
		{"baseline H", "X stored before", post, false},
		// F-5: the request classified H's header, so an undatable H is
		// still the repair's own; a baseline's is not.
		{"observed H", "H with no line stamp", post, true},
		{"baseline H", "H with no line stamp", pre, false},
		// F-1: before the replacement the old stamp judges; after it,
		// every write is a move.
		{"observed H replaced", "H stored before", between, true},
		{"observed H replaced", "H stored after", between, true},
		{"observed H replaced", "H stored before", post, false},
		{"observed H replaced", "X stored before", pre, true},
		{"observed H replaced", "X stored before", between, false},
		{"observed H replaced", "X stored before", post, false},
		{"unstamped replaced", "X stored before", between, true},
		{"unstamped replaced", "X stored before", post, false},
		{"unstamped replaced", "X stored after", between, false},
	}
	for _, c := range cases {
		got := windows[c.window].disregards(paths[c.path], c.modified)
		require.Equal(t, c.want, got, "%s window, %s, modified %s after the repair", c.window, c.path, c.modified.Sub(at))
	}
}

// #7132 review F-1: a launch boundary ends what the stamp says about the NEW
// runtime, not what it says about the old one. Writes the old pane made
// before af launched the new one — to the thread it held, or, for a repair
// the request could not observe, to any thread already in the store — are
// judged exactly as they were before the launch. The review's sequences: a
// restore keeps the repair across a could-not-look refusal and starts a fresh
// pane that sits idle, or resumes R and a second restore runs before the
// follow has read the new pane. Either way the next restore resumes R, as it
// would have before the launch; it must not read the old pane's own work as a
// move and retire the repair.
func TestOperatorRepairKeepsTheOldRuntimesOwnWritesAcrossALaunch(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	repairs := []struct {
		name   string
		repair AgentConversationData
	}{
		{"observed", operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID)},
		{"never observed", operatorRecordAt(heldTestOwnID, repairedAt)},
	}
	looks := []struct {
		name string
		look func(*Instance)
	}{
		{"the follow reads an idle fresh pane", func(inst *Instance) {
			require.Equal(t, ConversationFollowUnchanged,
				inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), inst.AgentConversation(), heldAnswer("")))
		}},
		{"the follow has not read the new pane", func(*Instance) {}},
	}
	for _, r := range repairs {
		for _, l := range looks {
			t.Run(r.name+", "+l.name, func(t *testing.T) {
				codexHome := t.TempDir()
				t.Setenv("CODEX_HOME", codexHome)
				workDir := t.TempDir()
				writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour))
				// H: the old pane's thread, stored before the repair. The old
				// pane kept working in it for 20 minutes after the repair,
				// then died.
				writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, repairedAt.Add(-time.Hour), repairedAt.Add(-time.Hour), repairedAt.Add(20*time.Minute))

				before := staleResumeInstance(t, workDir, r.repair)
				_, ok, decline := prepareExactResumeConversation(before, before.Program)
				require.True(t, ok, "fixture: before any launch the repair resumes R; refused %v", decline)

				inst := staleResumeInstance(t, workDir, r.repair)
				inst.setRuntimeLaunch(tmux.ProgramCodex, nil)
				l.look(inst)

				restore := staleResumeInstance(t, workDir, inst.AgentConversation())
				program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
				require.True(t, ok, "the old pane's own writes before the launch ended the repair: %v (retire=%v)",
					decline, restore.exactResumeRefusal(decline).retire)
				require.Contains(t, program, "resume "+heldTestOwnID)
			})
		}
	}
}

// #7132 review F-2: a reattach that finds another process behind the
// session's tmux name is evidence of a runtime af did not launch, so it ends
// the stamp the way an af launch does. Otherwise the next launch decides with
// the old runtime's stamp — its resume decision runs before that launch
// records itself — and resumes R over the thread the replacement works in.
// A probe that could not answer is no such evidence: the pane may still be
// the runtime the repair was made against.
func TestOperatorRepairStampEndsWhenAReattachFindsAnotherProcess(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-30 * time.Minute)
	agent := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-29-00-"+heldTestOwnID+".jsonl", t.TempDir()))
	other := startRolloutHolder(t, writeCodexRolloutFileWithCwd(t, t.TempDir(),
		"rollout-2026-11-11T14-30-00-"+heldTestSiblingID+".jsonl", t.TempDir()))
	for _, c := range []struct {
		name    string
		answer  func() ([]byte, error)
		resumes bool
	}{
		{"another process roots the pane", paneRootedAt(other), false},
		{"tmux reports no pane at all", func() ([]byte, error) { return nil, nil }, false},
		{"control: the probe could not answer", paneProbeUnanswerable, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := t.TempDir()
			writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour))
			held := writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, repairedAt.Add(-time.Hour), repairedAt.Add(-time.Hour), repairedAt.Add(-time.Minute))

			inst, panes := reattachInstance(t, agent, other)
			panes.set(c.answer)
			inst.Tabs[0].Conversation = operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID)
			inst.settleReattachVerification(inst.Tabs[0].tmux)
			// From here on, whatever process is behind the name works in H.
			// A second past the settle, so no clock or filesystem granularity
			// can put the write at or before it.
			written := time.Now().Add(time.Second)
			require.NoError(t, os.Chtimes(held, written, written))

			restore := staleResumeInstance(t, workDir, inst.AgentConversation())
			program, ok, decline := prepareExactResumeConversation(restore, restore.Program)
			if c.resumes {
				require.True(t, ok, "refused: %v", decline)
				require.Contains(t, program, "resume "+heldTestOwnID)
				return
			}
			require.False(t, ok, "resumed %q over the thread a process af did not launch works in", program)
		})
	}
}
