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

// The #7125 review's findings on the operator-repair bound. What makes a
// thread one the operator chose over is that its rollout was already in the
// store, and the pane on it, when the repair was made — not when Codex
// constructed the thread object.

// writeStoredCodexRollout lays a rollout out the way Codex's recorder writes a
// new thread (codex-rs rollout/src/recorder.rs): the clock reading taken when
// the thread is CONSTRUCTED names the file and day directory (local time) and
// becomes the session_meta payload's timestamp (UTC), but the file is created,
// and the session_meta line written with its own now_utc() timestamp, only at
// the first persist — the thread's first prompt (written). modified is the
// file's last append.
func writeStoredCodexRollout(t *testing.T, codexHome, id, cwd string, constructed, written, modified time.Time) string {
	t.Helper()
	local := constructed.Local()
	path := filepath.Join(codexRolloutDayDir(codexHome, local),
		"rollout-"+local.Format("2006-01-02T15-04-05")+"-"+id+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(storedCodexSessionMetaLine(t, id, cwd, constructed, written)), 0o644))
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

func storedCodexSessionMetaLine(t *testing.T, id, cwd string, constructed, written time.Time) string {
	t.Helper()
	stamp := func(at time.Time) string { return at.UTC().Format("2006-01-02T15:04:05.000Z") }
	data, err := json.Marshal(map[string]any{
		"timestamp": stamp(written),
		"type":      "session_meta",
		"payload": map[string]any{
			"id": id, "timestamp": stamp(constructed), "cwd": cwd,
			"originator": "codex_cli_rs", "source": "cli",
		},
	})
	require.NoError(t, err)
	return string(data) + "\n"
}

// R-1 (P2): the #7101 aftermath order. A lost-restore starts Codex fresh,
// which constructs thread F at once but writes nothing; the operator repairs
// to R while the pane holds nothing; the user's first prompt then writes F's
// rollout. F's construction stamp predates the repair, but F entered the
// store after it, so it is a thread the operator never chose over: with no
// follow to record it, the restore and the carry must refuse R, not resume
// it over F.
func TestOperatorRepairBoundDatesAThreadByWhenItsRolloutWasWritten(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	constructed := repairedAt.Add(-3 * time.Minute)
	firstPrompt := repairedAt.Add(5 * time.Minute)

	t.Run("restore", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))
		writeStoredCodexRollout(t, codexHome, repairNextID, workDir, constructed, firstPrompt, now.Add(-30*time.Second))

		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(""))
		_, ok, _ := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok, "F entered the store after the repair; resuming R over it continues a thread the user left")
	})
	t.Run("restore control: the thread the pane held at the repair", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))
		writeStoredCodexRollout(t, codexHome, repairNextID, workDir, constructed, repairedAt.Add(-time.Minute), now.Add(-30*time.Second))

		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairNextID))
		program, ok, decline := prepareExactResumeConversation(repair, repair.Program)
		require.True(t, ok, "refused: %v", decline)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
	t.Run("carry", func(t *testing.T) {
		home := carryAmbientProviderHomes(t)
		inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
		require.True(t, inst.SetAgentConversation(operatorRecordAt(carryTestID, repairedAt).WithRepairStamp("")))
		source := filepath.Join(home, ".codex")
		dir := inst.GetWorktreePath()
		recorded := writeCarryFile(t, source, codexRolloutRel(carryTestID),
			storedCodexSessionMetaLine(t, carryTestID, dir, now.Add(-2*time.Hour), now.Add(-2*time.Hour)))
		newer := writeCarryFile(t, source, codexRolloutRel(newerConversationID),
			storedCodexSessionMetaLine(t, newerConversationID, dir, constructed, firstPrompt))
		setModTime(t, recorded, now.Add(-time.Hour))
		setModTime(t, newer, now.Add(-time.Minute))

		require.NoError(t, inst.ValidateAccountSwap("work"))
		require.Nil(t, inst.accountSwapLaunch.carry, "a handoff must not carry R past the thread the first prompt wrote")
		require.Equal(t, staleCarryReason, inst.accountSwapLaunch.carryFallback)
	})
}

// R-3: a repair that knows which thread the pane held (its stamp) exempts that
// thread, and the threads nobody has written since — not a thread that
// already existed but that the pane moved to after the repair (Codex's resume
// picker). Only a repair with no observation keeps the looser rule, because it
// cannot tell the pane's own thread from the others.
func TestOperatorRepairBoundSeesAMoveToAnOlderThread(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)
	const olderID = "01a1cc00-1111-7222-8333-444455556666"
	setup := func(t *testing.T, olderModified time.Time) string {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-3*time.Hour), now.Add(-3*time.Hour), now.Add(-2*time.Hour))
		// H, the pane's thread at the repair, written to since.
		writeStoredCodexRollout(t, codexHome, repairHeldID, workDir, repairedAt.Add(-time.Hour), repairedAt.Add(-time.Hour), now.Add(-5*time.Minute))
		// X, older than the repair.
		writeStoredCodexRollout(t, codexHome, olderID, workDir, now.Add(-150*time.Minute), now.Add(-150*time.Minute), olderModified)
		return workDir
	}

	t.Run("resumed after the repair", func(t *testing.T) {
		workDir := setup(t, now.Add(-20*time.Second))
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
		_, ok, _ := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok, "the pane moved to X after the repair; resuming R would leave it")
	})
	t.Run("untouched since the repair", func(t *testing.T) {
		workDir := setup(t, repairedAt.Add(-30*time.Minute))
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
		program, ok, decline := prepareExactResumeConversation(repair, repair.Program)
		require.True(t, ok, "refused: %v", decline)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
	t.Run("a repair with no observation", func(t *testing.T) {
		workDir := setup(t, now.Add(-20*time.Second))
		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt))
		program, ok, decline := prepareExactResumeConversation(repair, repair.Program)
		require.True(t, ok, "refused: %v", decline)
		require.Contains(t, program, "resume "+heldTestOwnID)
	})
}

// R-4: whether a refusal retires an operator repair is decided by what kind
// of refusal it is, not by its wording. A newer rollout whose header af cannot
// classify yet (Codex mid-write) is "af could not look" and keeps the repair;
// two rollouts claiming the repaired id is "af looked and the id is
// ambiguous" and retires it, though the two used to share one reason string.
func TestOperatorRepairRefusalIsTypedNotWorded(t *testing.T) {
	now := time.Now()
	repairedAt := now.Add(-10 * time.Minute)

	t.Run("an unclassifiable newer rollout keeps the repair", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))
		day := codexRolloutDayDir(codexHome, now)
		require.NoError(t, os.MkdirAll(day, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(day, "rollout-"+now.Format("2006-01-02T15-04-05")+"-"+repairNextID+".jsonl"),
			[]byte(`{"timestamp":"`), 0o644))

		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
		_, ok, decline := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok)
		require.NotEmpty(t, decline)
		require.False(t, repair.exactResumeRefusal(decline).retire, "a header Codex has not finished writing says nothing about the repaired id")
	})
	t.Run("a duplicated repaired rollout retires it", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-time.Hour))
		writeStoredCodexRollout(t, codexHome, heldTestOwnID, workDir, now.Add(-30*time.Hour), now.Add(-30*time.Hour), now.Add(-time.Hour))

		repair := staleResumeInstance(t, workDir, operatorRecordAt(heldTestOwnID, repairedAt).WithRepairStamp(repairHeldID))
		_, ok, decline := prepareExactResumeConversation(repair, repair.Program)
		require.False(t, ok)
		require.True(t, repair.exactResumeRefusal(decline).retire, "an id two rollouts claim cannot be resumed, now or later")
	})
}
