package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7130: Codex files a thread under more than one name. Besides the plain
// rollout-<ts>-<thread>.jsonl af reads, its opt-in rollout compression
// rewrites a rollout left unmodified for 7 days as ….jsonl.zst (keeping its
// mtime), and thread/revert writes a new immutable rollout
// rollout-<ts>-<thread>_<rollout>.jsonl. `codex resume <thread>` resumes all
// of them, so none may read as "missing" — the checked-and-wrong refusal that
// retires the record.

const (
	formsThreadID  = "01a1cc00-aaaa-7222-8333-444455556666"
	formsOtherID   = "01a1cc00-bbbb-7222-8333-444455556666"
	formsRevertID  = "01a1cc00-cccc-7222-8333-444455556666"
	formsRevert2ID = "01a1cc00-dddd-7222-8333-444455556666"
)

// writeCompressedCodexRollout lays out what Codex's compression worker
// publishes: the plain rollout's name plus .zst, zstd bytes af cannot read,
// and the plain file's mtime.
func writeCompressedCodexRollout(t *testing.T, codexHome, threadID, rolloutID string, minted, modified time.Time) string {
	t.Helper()
	local := minted.Local()
	name := "rollout-" + local.Format("2006-01-02T15-04-05") + "-" + threadID
	if rolloutID != "" {
		name += "_" + rolloutID
	}
	path := filepath.Join(codexRolloutDayDir(codexHome, local), name+".jsonl.zst")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte{0x28, 0xb5, 0x2f, 0xfd, 0x24, 0x10, 0x81, 0x00, 0x00}, 0o644))
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

// writeRevertCodexRollout lays out a thread/revert rollout: the thread's id
// stays in the name and in session_meta, and the new rollout id follows it.
func writeRevertCodexRollout(t *testing.T, codexHome, threadID, rolloutID, cwd string, minted, modified time.Time) string {
	t.Helper()
	local := minted.Local()
	path := filepath.Join(codexRolloutDayDir(codexHome, local),
		"rollout-"+local.Format("2006-01-02T15-04-05")+"-"+threadID+"_"+rolloutID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(storedCodexSessionMetaLine(t, threadID, cwd, minted, minted)), 0o644))
	require.NoError(t, os.Chtimes(path, modified, modified))
	return path
}

func codexCaptureRecord(id string) AgentConversationData {
	return AgentConversationData{Agent: tmux.ProgramCodex, ID: id, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout}
}

func TestRestoreResumesARecordedThreadCodexStoredCompressed(t *testing.T) {
	now := time.Now()
	cold := now.Add(-9 * 24 * time.Hour)
	setup := func(t *testing.T) (codexHome, workDir string) {
		codexHome = t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir = t.TempDir()
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", cold, cold)
		return codexHome, workDir
	}

	t.Run("a capture resumes it", func(t *testing.T) {
		_, workDir := setup(t)
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "refused a thread `codex resume` can still open: %v (retire=%v)", decline, inst.exactResumeRefusal(decline).retire)
		require.Contains(t, program, "resume "+formsThreadID)
	})
	t.Run("it is still judged against newer threads in the worktree", func(t *testing.T) {
		codexHome, workDir := setup(t)
		writeStoredCodexRollout(t, codexHome, formsOtherID, workDir, now.Add(-time.Hour), now.Add(-time.Hour), now.Add(-time.Minute))
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok)
		require.Equal(t, staleCarryReason, decline.reason, "the compressed rollout is dated by the mtime Codex kept")
	})
	t.Run("an operator repair cannot be re-checked, so it is kept", func(t *testing.T) {
		_, workDir := setup(t)
		inst := staleResumeInstance(t, workDir, operatorRecordAt(formsThreadID, now.Add(-time.Hour)).WithRepairStamp(""))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok, "af cannot read a compressed header to confirm the repair is this session's own")
		require.True(t, decline.unverified, "could-not-look, not missing: %v", decline)
		require.False(t, inst.exactResumeRefusal(decline).retire)
	})
}

func TestRestoreResumesARecordedThreadARevertRolloutHolds(t *testing.T) {
	now := time.Now()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	// The thread's original rollout went cold and was compressed; the user
	// later reverted it, and the revert rollout is where it lives now.
	writeCompressedCodexRollout(t, codexHome, formsThreadID, "", now.Add(-10*24*time.Hour), now.Add(-9*24*time.Hour))
	writeRevertCodexRollout(t, codexHome, formsThreadID, formsRevertID, workDir, now.Add(-time.Hour), now.Add(-time.Minute))
	// Another thread in the worktree, newer than the original, older than
	// the revert: the thread is dated by its newest rollout.
	writeStoredCodexRollout(t, codexHome, formsOtherID, workDir, now.Add(-2*time.Hour), now.Add(-2*time.Hour), now.Add(-30*time.Minute))

	inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
	program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
	require.True(t, ok, "refused a reverted thread `codex resume` can still open: %v", decline)
	require.Contains(t, program, "resume "+formsThreadID)
}

func TestRestoreStillRefusesWhatCodexCannotResume(t *testing.T) {
	now := time.Now()
	t.Run("control: no rollout of any form", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeCompressedCodexRollout(t, codexHome, formsOtherID, "", now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour))
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok)
		require.Contains(t, decline.reason, "missing")
		require.True(t, inst.exactResumeRefusal(decline).retire)
	})
	t.Run("control: a plain rollout beside its compressed copy is one rollout", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		minted := now.Add(-9 * 24 * time.Hour)
		writeStoredCodexRollout(t, codexHome, formsThreadID, workDir, minted, minted, minted)
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", minted, minted)
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "refused: %v", decline)
	})
	t.Run("two compressed rollouts claim the thread", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour))
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", now.Add(-20*24*time.Hour), now.Add(-20*24*time.Hour))
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok)
		require.Contains(t, decline.reason, "two rollouts record it", "ambiguous, not missing")
	})
}

// The carry does not copy these forms, but it must not call them missing:
// the handoff's stated reason is what the user reads.
func TestCarryDoesNotCallACompressedRolloutMissing(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, _ := carrySwapInstance(t, tmux.ProgramCodex)
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, formsThreadID)
	source := filepath.Join(home, ".codex")
	writeCompressedCodexRollout(t, source, formsThreadID, "", time.Now().Add(-9*24*time.Hour), time.Now().Add(-9*24*time.Hour))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.Nil(t, inst.accountSwapLaunch.carry)
	require.NotContains(t, inst.accountSwapLaunch.carryFallback, "missing")
	require.Contains(t, inst.accountSwapLaunch.carryFallback, "compressed")
}
