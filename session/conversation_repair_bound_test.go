package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7122: the round-2 review of #7120 found the operator-repair staleness
// exemption unbounded. It ended only when the follow check saw the pane move,
// so whenever the follow was blind — launch evidence missing or retired, a
// persistent ambiguity, a stuck freeze, a daemon restart, a remote row — a
// restore or a handoff's carry resumed the repaired thread R although the user
// had started a newer thread N in the worktree after the repair. These tests
// pin the bound as properties of the record and the store, with no follow
// check and no in-memory state involved.

// codexSessionMetaLine is the first rollout line as Codex's recorder writes
// it: the mint instant in UTC to the millisecond, marked Z, on both the line
// and its session_meta payload.
func codexSessionMetaLine(t *testing.T, id, cwd string, minted time.Time, source any) string {
	t.Helper()
	stamp := minted.UTC().Format("2006-01-02T15:04:05.000Z")
	payload := map[string]any{"id": id, "timestamp": stamp, "cwd": cwd, "originator": "codex_cli_rs"}
	if source != nil {
		payload["source"] = source
	}
	data, err := json.Marshal(map[string]any{"timestamp": stamp, "type": "session_meta", "payload": payload})
	require.NoError(t, err)
	return string(data) + "\n"
}

// writeMintedCodexRollout writes a rollout the way the recorder lays one out:
// the filename and day directory carry the recorder's LOCAL wall clock with no
// offset (named), the header carries the mint instant in UTC (minted), and the
// file's modification time is its last append (modified).
func writeMintedCodexRollout(t *testing.T, codexHome, id, cwd string, named, minted, modified time.Time, source any) string {
	t.Helper()
	local := named.Local()
	path := filepath.Join(codexRolloutDayDir(codexHome, local),
		"rollout-"+local.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(codexSessionMetaLine(t, id, cwd, minted, source)), 0o644))
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

func operatorRecordAt(id string, at time.Time) AgentConversationData {
	return AgentConversationData{Agent: tmux.ProgramCodex, ID: id, CapturedAt: at, CaptureKind: ConversationCaptureOperator}
}

// Property 1, restore: the exemption disregards only threads that existed when
// the repair was made. A same-worktree top-level thread minted after it makes
// the repair stale exactly as it would a capture's record — with nothing in
// memory to say the pane moved, so a restart window and a blind follow are the
// same case.
func TestExactResumeBoundsAnOperatorRepairToThreadsThatExistedAtTheRepair(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	setup := func(t *testing.T) (codexHome, workDir string) {
		codexHome = t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir = t.TempDir()
		// R, the thread the operator chose: old, and not written since.
		writeMintedCodexRollout(t, codexHome, heldTestOwnID, workDir,
			now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), "cli")
		// H, the thread the pane was on at the repair, still being appended to.
		writeMintedCodexRollout(t, codexHome, repairHeldID, workDir,
			repairedAt.Add(-5*time.Minute), repairedAt.Add(-5*time.Minute), now.Add(-time.Minute), "cli")
		return codexHome, workDir
	}

	t.Run("a thread that existed at the repair never makes it stale", func(t *testing.T) {
		_, workDir := setup(t)
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
		program, ok, reason := prepareExactResumeConversation(repair, repair.Program)
		require.True(t, ok, "refused: %s", reason.reason)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})

	t.Run("a thread minted after the repair makes it stale", func(t *testing.T) {
		codexHome, workDir := setup(t)
		writeMintedCodexRollout(t, codexHome, repairNextID, workDir,
			repairedAt.Add(5*time.Minute), repairedAt.Add(5*time.Minute), now.Add(-30*time.Second), "cli")
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
		_, ok, reason := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok, "a thread the user started after the repair must not be silently skipped")
		require.Equal(t, staleCarryReason, reason.reason)
	})

	t.Run("the mint time is the header's UTC instant, not the filename's local stamp", func(t *testing.T) {
		codexHome, workDir := setup(t)
		// A filename stamp that reads as long before the repair in any zone.
		// Codex writes it from its own local clock with no offset, so af
		// cannot read an instant from it; the header is what dates N.
		writeMintedCodexRollout(t, codexHome, repairNextID, workDir,
			repairedAt.Add(-48*time.Hour), repairedAt.Add(5*time.Minute), now.Add(-30*time.Second), "cli")
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
		_, ok, reason := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok)
		require.Equal(t, staleCarryReason, reason.reason)
	})

	t.Run("an operator kind af cannot date gets no exemption", func(t *testing.T) {
		for name, capturedAt := range map[string]time.Time{
			"no capture time":              {},
			"a capture time in the future": now.Add(time.Hour),
		} {
			t.Run(name, func(t *testing.T) {
				_, workDir := setup(t)
				repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, capturedAt))
				_, ok, reason := prepareExactResumeConversation(repair, repair.Program)
				require.False(t, ok, "a hand-edited operator kind must be judged like any record")
				require.Equal(t, staleCarryReason, reason.reason)
			})
		}
	})
}

// Property 1, carry: a handoff applies the same bound. And the operator kind
// was only ever a Codex repair: a Claude record carrying it gets no exemption.
func TestCarryBoundsAnOperatorRepairToThreadsThatExistedAtTheRepair(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	carry := func(t *testing.T, newerMinted time.Time) *Instance {
		t.Helper()
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		require.True(t, inst.SetAgentConversation(operatorRecordAt(carryTestID, repairedAt)))
		source := filepath.Join(home, ".codex")
		dir := inst.GetWorktreePath()
		recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID),
			codexSessionMetaLine(t, carryTestID, dir, now.Add(-2*time.Hour), "cli"))
		newer := writeCarryFile(t, source, codexRolloutRel(newerConversationID),
			codexSessionMetaLine(t, newerConversationID, dir, newerMinted, "cli"))
		setModTime(t, recorded, now.Add(-time.Hour))
		setModTime(t, newer, now.Add(-time.Minute))
		require.NoError(t, inst.ValidateAccountSwap("work"))
		return inst
	}

	t.Run("a thread that existed at the repair", func(t *testing.T) {
		inst := carry(t, repairedAt.Add(-5*time.Minute))
		require.NotNil(t, inst.accountSwapLaunch.carry, "refused: %s", inst.accountSwapLaunch.carryFallback)
	})
	t.Run("a thread minted after the repair", func(t *testing.T) {
		inst := carry(t, repairedAt.Add(5*time.Minute))
		require.Nil(t, inst.accountSwapLaunch.carry,
			"a handoff must not carry the repaired thread past one the user started after the repair")
		require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
	})
	t.Run("a claude record carrying the operator kind", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramClaude)
		require.True(t, inst.SetAgentConversation(AgentConversationData{
			Agent: tmux.ProgramClaude, ID: carryTestID, CapturedAt: repairedAt, CaptureKind: ConversationCaptureOperator,
		}))
		source := filepath.Join(home, ".claude")
		recorded := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), carryTestID), "{}\n")
		newer := writeCarryFile(t, source, claudeTranscriptRel(inst.GetWorktreePath(), newerConversationID), "{}\n")
		setModTime(t, recorded, now.Add(-time.Hour))
		setModTime(t, newer, now.Add(-time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry, "set-conversation never writes a Claude repair, so the kind proves nothing")
		require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
	})
}

// reloadedAgentConversation is the record a daemon restart reads back: the
// row as persisted, through JSON, onto a fresh instance with no memory of the
// one that wrote it.
func reloadedAgentConversation(t *testing.T, inst *Instance) *Instance {
	t.Helper()
	raw, err := json.Marshal(inst.ToInstanceData())
	require.NoError(t, err)
	var back InstanceData
	require.NoError(t, json.Unmarshal(raw, &back))
	restarted := retireTestInstance(t)
	restarted.Tabs[0].Conversation = persistedConversationOf(back)
	return restarted
}

// Property 2: what the runtime held when the repair was observed is part of
// the persisted record, so a move the pane makes while the daemon is down is
// still a move after the restart — not the post-restart baseline (the review's
// probe B, #7120 F-C).
func TestOperatorRepairObservationSurvivesADaemonRestart(t *testing.T) {
	inst := retireTestInstance(t)
	inst.SetAgentConversation(operatorRecord(heldTestOwnID))
	// The repair's first observation: the pane holds H, the thread it was on.
	inst.FollowAgentConversationForRuntime(inst.AgentRuntimeToken(), inst.AgentConversation(), heldAnswer(repairHeldID))
	require.Equal(t, heldTestOwnID, inst.AgentConversation().ID, "the thread held at the repair never replaces it")

	restarted := reloadedAgentConversation(t, inst)
	require.Equal(t, heldTestOwnID, restarted.AgentConversation().ID)
	require.Equal(t, ConversationCaptureOperator, restarted.AgentConversation().CaptureKind)

	// While the daemon was down the user ran /new: the pane now holds N.
	token := restarted.AgentRuntimeToken()
	require.True(t, restarted.ConversationFollowPermitted(token, restarted.AgentConversation(), heldAnswer(repairNextID)),
		"a move made across a restart is still a move")
	restarted.FollowAgentConversationForRuntime(token, restarted.AgentConversation(), heldAnswer(repairNextID))
	require.Equal(t, repairNextID, restarted.AgentConversation().ID,
		"the follow records the thread the pane moved to, rather than taking it as the repair's baseline")
	require.Equal(t, ConversationCaptureCodexRollout, restarted.AgentConversation().CaptureKind)

	// And the thread held at the repair still stands as "not a move" after a restart.
	again := retireTestInstance(t)
	again.SetAgentConversation(operatorRecord(heldTestOwnID))
	again.FollowAgentConversationForRuntime(again.AgentRuntimeToken(), again.AgentConversation(), heldAnswer(repairHeldID))
	again = reloadedAgentConversation(t, again)
	require.False(t, again.ConversationFollowPermitted(again.AgentRuntimeToken(), again.AgentConversation(), heldAnswer(repairHeldID)))
}

// Property 3: at resume time the exempt branch re-checks what set-conversation
// proved — a top-level thread launched in this session's directory — so an
// operator kind that arrived any other way (carried in, hand-edited) gets no
// free pass. It applies on restore and on carry alike.
func TestOperatorRepairProvenanceIsRecheckedAtResume(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	for name, tc := range map[string]struct {
		cwd    func(workDir string) string
		source any
	}{
		"launched in another directory": {cwd: func(string) string { return t.TempDir() }, source: "cli"},
		"a sub-agent thread":            {cwd: func(dir string) string { return dir }, source: map[string]any{"subagent": "review"}},
	} {
		t.Run("restore: "+name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := t.TempDir()
			writeMintedCodexRollout(t, codexHome, heldTestOwnID, tc.cwd(workDir),
				now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), tc.source)
			repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
			_, ok, reason := prepareExactResumeConversation(repair, repair.Program)
			require.False(t, ok, "an operator record whose rollout is not this session's own must not be resumed")
			require.NotEmpty(t, reason)
		})
		t.Run("carry: "+name, func(t *testing.T) {
			home := carryAmbientProviderHomes(t)
			inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
			require.True(t, inst.SetAgentConversation(operatorRecordAt(carryTestID, repairedAt)))
			writeCarryFile(t, filepath.Join(home, ".codex"), codexRolloutRel(carryTestID),
				codexSessionMetaLine(t, carryTestID, tc.cwd(inst.GetWorktreePath()), now.Add(-2*time.Hour), tc.source))
			require.NoError(t, inst.ValidateAccountSwap("work"))
			require.Nil(t, inst.accountSwapLaunch.carry, "an operator record whose rollout is not this session's own must not be carried")
			require.NotEmpty(t, inst.accountSwapLaunch.carryFallback)
		})
	}
}

// P3 (#7120 review F-G): a refusal that only says af could not look — here a
// store that will not enumerate — is no evidence about an operator's repair,
// so the fresh start it causes must not retire it. A capture's record keeps
// the existing rule.
func TestCouldNotVerifyRefusalKeepsAnOperatorRepair(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits do not constrain root")
	}
	now := time.Now()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	writeMintedCodexRollout(t, codexHome, heldTestOwnID, workDir,
		now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour), "cli")
	blocked := codexRolloutDayDir(codexHome, now.AddDate(0, 0, -3))
	require.NoError(t, os.MkdirAll(blocked, 0o755))
	require.NoError(t, os.Chmod(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, now.Add(-10*time.Minute)))
	_, ok, reason := prepareExactResumeConversation(repair, repair.Program)
	require.False(t, ok)
	require.NotEmpty(t, reason)
	require.False(t, repair.exactResumeRefusal(reason).retire,
		"a store af could not read says nothing about the repaired id")

	captured := staleResumeInstance(t, workDir, AgentConversationData{
		Agent: tmux.ProgramCodex, ID: heldTestOwnID, CapturedAt: now, CaptureKind: ConversationCaptureCodexRollout,
	})
	_, ok, reason = prepareExactResumeConversation(captured, captured.Program)
	require.False(t, ok)
	require.True(t, captured.exactResumeRefusal(reason).retire, "a capture's refused record is still retired")
}

// A rollout is dated by when it entered the store — its session_meta line's
// own stamp, an instant — and never by a construction date alone (#7125
// review R-1).
func TestCodexRolloutStoredAt(t *testing.T) {
	stored := time.Date(2026, 11, 11, 21, 50, 10, 432_000_000, time.UTC)
	write := func(t *testing.T, line string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "rollout-2026-11-11T13-50-10-"+heldTestOwnID+".jsonl")
		require.NoError(t, os.WriteFile(path, []byte(line+"\n"), 0o644))
		return path
	}
	meta := func(lineStamp, payloadStamp string) string {
		line := `{"type":"session_meta","payload":{"cwd":"/w"`
		if payloadStamp != "" {
			line += `,"timestamp":"` + payloadStamp + `"`
		}
		line += `}`
		if lineStamp != "" {
			line += `,"timestamp":"` + lineStamp + `"`
		}
		return line + `}`
	}

	t.Run("the line's UTC stamp, not the earlier construction stamp", func(t *testing.T) {
		got, ok := codexRolloutStoredAt(write(t, meta("2026-11-11T21:50:10.432Z", "2026-11-11T21:40:00.000Z")))
		require.True(t, ok)
		require.True(t, got.Equal(stored), "got %s", got.UTC())
	})
	t.Run("an offset stamp is the same instant", func(t *testing.T) {
		got, ok := codexRolloutStoredAt(write(t, meta("2026-11-11T13:50:10.432-08:00", "")))
		require.True(t, ok)
		require.True(t, got.Equal(stored))
	})
	t.Run("a payload stamped after its line is never under-dated", func(t *testing.T) {
		got, ok := codexRolloutStoredAt(write(t, meta("2026-11-11T21:40:00.000Z", "2026-11-11T21:50:10.432Z")))
		require.True(t, ok)
		require.True(t, got.Equal(stored))
	})
	for name, line := range map[string]string{
		"a construction stamp alone":        meta("", "2026-11-11T21:50:10.432Z"),
		"a line stamp with no zone":         meta("2026-11-11T13:50:10.432", "2026-11-11T21:50:10.432Z"),
		"a UUIDv7 id and no stamp at all":   meta("", ""),
		"a header that is not session_meta": `{"type":"event_msg","timestamp":"2026-11-11T21:50:10.432Z"}`,
	} {
		t.Run(name+" cannot date it", func(t *testing.T) {
			_, ok := codexRolloutStoredAt(write(t, line))
			require.False(t, ok)
		})
	}
	t.Run("a rollout stored in the repair's own millisecond is not disregarded", func(t *testing.T) {
		path := write(t, meta("2026-11-11T21:50:10.432Z", ""))
		window := operatorRepairWindow{at: stored.Add(500 * time.Microsecond)}
		require.False(t, window.disregards(path, stored))
		window.at = stored.Add(time.Millisecond)
		require.True(t, window.disregards(path, stored))
	})
}

// R-2: a repair stamped from an observation is dated no later than it.
func TestWithRepairObservationDatesTheRepairNoLaterThanTheLook(t *testing.T) {
	validated := time.Now()
	looked := validated.Add(-3 * time.Second)
	stamped := operatorRecordAt(heldTestOwnID, validated).WithRepairObservation(repairHeldID, looked)
	require.True(t, stamped.CapturedAt.Equal(looked))
	require.Equal(t, repairHeldID, stamped.RepairHeldID)
	later := operatorRecordAt(heldTestOwnID, validated).WithRepairObservation(repairHeldID, validated.Add(time.Second))
	require.True(t, later.CapturedAt.Equal(validated), "an observation never dates the repair later than it was validated")
}

// The repair a set-conversation writes carries the observation with it, and
// the record a follow writes when it moves carries none.
func TestWithRepairStamp(t *testing.T) {
	stamped := operatorRecord(heldTestOwnID).WithRepairStamp(" 01A1CCD0-1111-7222-8333-444455556666 ")
	require.True(t, stamped.RepairStamped)
	require.Equal(t, repairHeldID, stamped.RepairHeldID)
	nothing := operatorRecord(heldTestOwnID).WithRepairStamp("")
	require.True(t, nothing.RepairStamped, "holding nothing is an observation too")
	require.Empty(t, nothing.RepairHeldID)
	require.False(t, AgentConversationData{RepairStamped: true}.Empty())
}
