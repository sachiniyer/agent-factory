package session

import (
	"os"
	"path/filepath"
	"strings"
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
		// #7134 review F-5: af never resumes it, so Codex never writes it
		// back out; the refusal says how to get it back.
		require.Contains(t, decline.reason, "zstd -d --rm", "the refusal must say how to make it resumable")
		require.Contains(t, decline.reason, "af sessions set-conversation")
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
	t.Run("a name Codex's resolver rejects is not the thread", func(t *testing.T) {
		codexHome := t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		workDir := t.TempDir()
		dir := codexRolloutDayDir(codexHome, now.Add(-9*24*time.Hour))
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "rollout-not-a-timestamp-"+formsThreadID+".jsonl.zst"), []byte{0x28, 0xb5, 0x2f, 0xfd}, 0o644))
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok, "`codex resume` skips a name without its exact timestamp")
		require.Contains(t, decline.reason, "missing")
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

// The forms are recognised for the recorded thread only: the plain walk the
// staleness check reads its candidates from still names plain rollouts alone.
func TestCodexRolloutOtherForms(t *testing.T) {
	codexHome := t.TempDir()
	cwd := t.TempDir()
	minted := time.Now().Add(-9 * 24 * time.Hour)
	plain := writeStoredCodexRollout(t, codexHome, formsThreadID, cwd, minted, minted, minted)
	compressed := writeCompressedCodexRollout(t, codexHome, formsThreadID, "", minted, minted)
	revert := writeRevertCodexRollout(t, codexHome, formsThreadID, formsRevertID, cwd, minted, minted)
	revertCompressed := writeCompressedCodexRollout(t, codexHome, formsThreadID, formsRevert2ID, minted, minted)
	other := writeCompressedCodexRollout(t, codexHome, formsOtherID, "", minted, minted)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(plain), "rollout-compress-x.tmp"), nil, 0o644))
	// Neither a symlink nor a name without Codex's exact timestamp is a file
	// Codex resumes (#7134 review F-4).
	elsewhere := writeCompressedCodexRollout(t, t.TempDir(), formsThreadID, formsRevert2ID, minted, minted)
	require.NoError(t, os.Symlink(elsewhere, filepath.Join(filepath.Dir(plain), "rollout-2026-01-01T00-00-00-"+formsThreadID+"_"+formsOtherID+".jsonl.zst")))
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(plain), "rollout-x-"+formsThreadID+".jsonl.zst"), nil, 0o644))

	plains, forms, err := codexRolloutForms(codexHome, formsThreadID)
	require.NoError(t, err)
	require.Equal(t, []string{plain}, plains)
	require.ElementsMatch(t, []string{compressed, revert, revertCompressed}, forms)

	files, err := codexRolloutFiles(codexHome)
	require.NoError(t, err)
	require.Contains(t, files, plain)
	for _, path := range []string{compressed, revert, revertCompressed, other} {
		require.NotContains(t, files, path, "the staleness walk must not take %s as a candidate", filepath.Base(path))
	}
	require.True(t, codexRolloutCompressed(compressed))
	require.True(t, codexRolloutCompressed(revertCompressed))
	require.False(t, codexRolloutCompressed(revert))
	require.False(t, codexRolloutCompressed(plain))
}

// Control: a compressed rollout is never a staleness candidate — af cannot
// read its header to say whose it is, and in a shared home every other cold
// thread is one, so counting them would refuse every long-idle restore as
// could-not-look.
func TestRestoreDoesNotJudgeByACompressedThread(t *testing.T) {
	now := time.Now()
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	workDir := t.TempDir()
	recorded := now.Add(-20 * 24 * time.Hour)
	writeStoredCodexRollout(t, codexHome, formsThreadID, workDir, recorded, recorded, recorded)
	writeCompressedCodexRollout(t, codexHome, formsOtherID, "", now.Add(-10*24*time.Hour), now.Add(-9*24*time.Hour))

	inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
	_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
	require.True(t, ok, "refused: %v", decline)
}

// #7134 review F-1, accepted: a newer thread the user moved to that Codex has
// also compressed is no staleness candidate — af cannot read its header to
// say it is this worktree's — so a compressed recorded thread is resumed over
// it. Before #7130 the record read as missing and the restore started fresh.
// It needs Codex's opt-in compression and a follow that missed the move;
// closing it needs a zstd decoder. A newer plain thread is still seen.
func TestAcceptedGapANewerThreadCodexCompressedIsNotSeenOverACompressedRecord(t *testing.T) {
	now := time.Now()
	for _, c := range []struct {
		name       string
		compressed bool
		resumes    bool
	}{
		{"the thread the user moved to is compressed too: accepted, resumed", true, true},
		{"control: the thread the user moved to is plain: stale", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			codexHome := t.TempDir()
			t.Setenv("CODEX_HOME", codexHome)
			workDir := t.TempDir()
			writeCompressedCodexRollout(t, codexHome, formsThreadID, "", now.Add(-21*24*time.Hour), now.Add(-20*24*time.Hour))
			moved := now.Add(-10 * 24 * time.Hour)
			if c.compressed {
				writeCompressedCodexRollout(t, codexHome, formsOtherID, "", moved, moved)
			} else {
				writeStoredCodexRollout(t, codexHome, formsOtherID, workDir, moved, moved, moved)
			}
			inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
			_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
			require.Equal(t, c.resumes, ok, "decline: %v", decline)
		})
	}
}

// onRecordedCodexRolloutLocated sets the guard's test seam for one test.
func onRecordedCodexRolloutLocated(t *testing.T, hook func(path string)) {
	t.Helper()
	recordedCodexRolloutLocatedHook = hook
	t.Cleanup(func() { recordedCodexRolloutLocatedHook = nil })
}

// #7134 review F-3: a store in which the thread exists at every instant never
// reads as missing. Codex moves a thread between forms by publishing the new
// file before removing the old one, so af can catch it between its two walks,
// or after it chose a file and before it dated it.
func TestAThreadCodexMovesBetweenFormsWhileAfLooksIsNotMissing(t *testing.T) {
	now := time.Now()
	cold := now.Add(-9 * 24 * time.Hour)
	setup := func(t *testing.T) (codexHome, workDir string) {
		codexHome = t.TempDir()
		t.Setenv("CODEX_HOME", codexHome)
		return codexHome, t.TempDir()
	}
	// move publishes to and then removes from, keeping the mtime, as
	// Codex's compression and write-back both do.
	move := func(t *testing.T, from, to string) {
		t.Helper()
		info, err := os.Stat(from)
		require.NoError(t, err)
		data, err := os.ReadFile(from)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(to, data, 0o644))
		require.NoError(t, os.Chtimes(to, info.ModTime(), info.ModTime()))
		require.NoError(t, os.Remove(from))
	}

	t.Run("a plain rollout that appeared after the store walk is found", func(t *testing.T) {
		codexHome, workDir := setup(t)
		plain := writeStoredCodexRollout(t, codexHome, formsThreadID, workDir, cold, cold, cold)
		path, decline := recordedCodexRolloutInAnotherForm(codexHome, formsThreadID)
		require.Empty(t, decline.reason, "read as %v (retire=%v)", decline, decline.retiresRecord(codexCaptureRecord(formsThreadID)))
		require.Equal(t, plain, path)
	})
	t.Run("the compressed rollout af chose is written back out before af dates it", func(t *testing.T) {
		codexHome, workDir := setup(t)
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", cold, cold)
		onRecordedCodexRolloutLocated(t, func(path string) {
			if strings.HasSuffix(path, ".zst") {
				move(t, path, strings.TrimSuffix(path, ".zst"))
			}
		})
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "refused: %v (retire=%v)", decline, inst.exactResumeRefusal(decline).retire)
		require.Contains(t, program, "resume "+formsThreadID)
	})
	t.Run("the plain rollout af chose is compressed before af dates it", func(t *testing.T) {
		codexHome, workDir := setup(t)
		writeStoredCodexRollout(t, codexHome, formsThreadID, workDir, cold, cold, cold)
		onRecordedCodexRolloutLocated(t, func(path string) {
			if strings.HasSuffix(path, ".jsonl") {
				move(t, path, path+".zst")
			}
		})
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		program, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.True(t, ok, "refused: %v (retire=%v)", decline, inst.exactResumeRefusal(decline).retire)
		require.Contains(t, program, "resume "+formsThreadID)
	})
	t.Run("control: a thread removed from every form is missing", func(t *testing.T) {
		codexHome, workDir := setup(t)
		writeCompressedCodexRollout(t, codexHome, formsThreadID, "", cold, cold)
		looks := 0
		onRecordedCodexRolloutLocated(t, func(path string) {
			looks++
			require.NoError(t, os.Remove(path))
		})
		inst := staleResumeInstance(t, workDir, codexCaptureRecord(formsThreadID))
		_, ok, decline := prepareExactResumeConversation(inst, inst.Program)
		require.False(t, ok)
		require.Equal(t, codexRolloutMissingReason, decline.reason)
		require.Equal(t, 1, looks, "the second look finds no form of the thread at all")
	})
}

// #7134 review F-4: the carry's reason names only what it saw. A symlinked
// rollout is no form Codex resumes, and a store it could not walk is not one
// it found empty.
func TestCarryOtherFormsReasonNamesOnlyWhatItSaw(t *testing.T) {
	minted := time.Now().Add(-9 * 24 * time.Hour)
	t.Run("a symlinked compressed rollout is not called compressed", func(t *testing.T) {
		codexHome := t.TempDir()
		elsewhere := writeCompressedCodexRollout(t, t.TempDir(), formsThreadID, "", minted, minted)
		dir := codexRolloutDayDir(codexHome, minted.Local())
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Symlink(elsewhere, filepath.Join(dir, filepath.Base(elsewhere))))
		require.Empty(t, codexCarryOtherFormsReason(codexHome, formsThreadID))
	})
	t.Run("a store af could not walk says so", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("permission bits do not constrain root")
		}
		codexHome := t.TempDir()
		dir := codexRolloutDayDir(codexHome, minted.Local())
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.Chmod(dir, 0))
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		require.Contains(t, codexCarryOtherFormsReason(codexHome, formsThreadID), "could not check")
	})
}
