package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7212 (continuation of #7223): claudeProjectCandidates resolves the launch
// dir's symlink spelling on the same poll/load/restore/limit paths whose
// directory listings fsprobe bounds. The resolution is an unbounded
// per-component lstat walk no timeout exists for, so a launch dir on a wedged
// mount (NFS, FUSE, a dead sshfs) holds the call forever — before the bounded
// listing ever runs. It is now bounded (fsprobe.EvalSymlinks), and a resolution
// that does not answer within the bound is "could not look": the resolved
// candidate is dropped and only the recorded spelling is returned.

// wedgeLaunchDirEvalSymlinks makes every symlink resolution block until the
// test ends, the way a wedged launch-dir mount does, and bounds af's
// resolution at 50ms. A test that needs the real resolver for setup must do
// that setup before calling this helper.
func wedgeLaunchDirEvalSymlinks(t *testing.T) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	restore := fsprobe.SetEvalSymlinksForTest(func(path string) (string, error) {
		<-release
		return path, nil
	})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		// Let any flight still blocked in the resolver finish before the real
		// resolver comes back.
		time.Sleep(50 * time.Millisecond)
		restore()
		restoreTimeout()
	})
}

// A wedged launch dir must not hold the candidates beyond the bound: the
// resolution times out and only the recorded spelling is returned, since a
// resolution that did not happen says nothing about the kernel's spelling.
func TestClaudeProjectCandidatesDoesNotStallOnWedgedLaunchDir(t *testing.T) {
	dir := t.TempDir()
	wedgeLaunchDirEvalSymlinks(t)

	got := withinBound(t, func() []string { return claudeProjectCandidates(dir) })
	require.Len(t, got, 1, "a resolution that did not happen keeps only the recorded spelling")
	require.Equal(t, filepath.Join("projects", claudeProjectName(dir)), got[0])
}

// A wedged launch dir on the restore's judge path must not stall the restore:
// the resolution times out, the judge resumes the recorded id as before, and a
// look that did not happen retires nothing.
func TestClaudeRestoreJudgeDoesNotStallOnWedgedLaunchDir(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recorded, _ := claudeRestoreWorktree(t)
	writeCarryFile(t, configDir, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	// Wedge after setup: claudeRestoreWorktree needs the real resolver to
	// learn the recorded spelling the carry file is filed under.
	wedgeLaunchDirEvalSymlinks(t)

	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "claude")
		return exactResumeAnswer{program, ok, decline}
	})
	require.True(t, got.ok, "a launch dir af could not resolve says nothing about the id: the restore resumes it as before")
	require.Equal(t, "claude --resume "+carryTestID, got.program)
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired on a look that did not happen")
}

// A wedged launch dir on the poll's inspect path must not stall the inspect:
// the resolution times out, the inspect reads only the recorded spelling, and
// a recorded conversation filed under it is still found — or, with no
// transcript there, the empty (resume-the-newest, found-none) state returns
// without waiting on the mount.
func TestInspectClaudeProjectConversationsDoesNotStallOnWedgedLaunchDir(t *testing.T) {
	configDir := t.TempDir()
	real := filepath.Join(testguard.CanonicalTempDir(t), "real-inspect")
	require.NoError(t, os.MkdirAll(real, 0o700))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link-inspect")
	require.NoError(t, os.Symlink(real, link))
	projects := claudeProjectCandidates(link)
	require.Len(t, projects, 2)
	recordedProject, resolvedProject := projects[0], projects[1]
	// File the recorded conversation under the RECORDED spelling, which the
	// wedged resolution falls back to.
	const recordedID = "64ea06ed-ed87-4b12-bc2d-2a2102a81ba5"
	setModTime(t, writeCarryFile(t, configDir, filepath.Join(recordedProject, recordedID+".jsonl"), "{}\n"), time.Now().Add(-time.Hour))
	_ = resolvedProject
	wedgeLaunchDirEvalSymlinks(t)

	recorded := AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID}
	resumable := withinBound(t, func() bool {
		return ConversationResumableHere("CLAUDE_CONFIG_DIR="+configDir+" claude", link, recorded)
	})
	require.True(t, resumable, "a transcript filed under the recorded spelling is found even when the resolution times out")
}

// Codex P1 on #7268: a resolution that did not answer is an incomplete search
// for the conversation's copies, never a complete one. Claude went on
// appending under the resolved spelling after an older copy was left under
// the recorded one; an account swap that searched only the recorded spelling
// would carry the older copy and lose the latest turns. The plan refuses
// instead: the stated fresh start, or a move require_carry defers.
func TestClaudeCarryRefusesWhenTheLaunchDirSpellingDoesNotResolve(t *testing.T) {
	home := carryAmbientProviderHomes(t)
	inst, recorded, resolved := symlinkedClaudeCarryInstance(t)
	recordOutgoingConversation(t, inst, tmux.ProgramClaude, carryTestID)
	source := filepath.Join(home, ".claude")
	cwd := inst.GetWorktreePath()
	past := time.Now().Add(-time.Hour)
	setModTime(t, writeCarryFile(t, source, filepath.Join(recorded, carryTestID+".jsonl"), claudeRecordLine(t, cwd)), past)
	// The copy Claude is appending to: every byte of the older one, then the
	// latest turns.
	setModTime(t, writeCarryFile(t, source, filepath.Join(resolved, carryTestID+".jsonl"),
		claudeRecordLine(t, cwd)+claudeRecordLine(t, cwd)), past.Add(time.Minute))
	require.Empty(t, inst.AccountSwapCarryRefusal(), "precondition: with the spelling resolved, the newer copy carries")
	wedgeLaunchDirEvalSymlinks(t)

	refusal := withinBound(t, inst.AccountSwapCarryRefusal)
	require.Equal(t, carryRefusalCode(carrySpellingsUnresolvedReason), refusal,
		"require_carry's check-ahead defers the move on an incomplete search")
	require.NoError(t, withinBound(t, func() error { return inst.ValidateAccountSwap("work") }))
	if carry := inst.accountSwapLaunch.carry; carry != nil {
		t.Fatalf("an incomplete search carried %s, which may be missing the latest turns", carry.transcript)
	}
	require.Equal(t, carrySpellingsUnresolvedReason, inst.accountSwapLaunch.carryFallback,
		"the swap states why it starts fresh")
}

// The spelling list says whether it is complete: a resolution that answers, or
// fails with an answer (nothing at the path), is a complete search; one that
// did not answer within the bound is not.
func TestClaudeProjectSpellingsReportAnIncompleteSearch(t *testing.T) {
	real := filepath.Join(testguard.CanonicalTempDir(t), "real-spellings")
	require.NoError(t, os.MkdirAll(real, 0o700))
	link := filepath.Join(testguard.CanonicalTempDir(t), "link-spellings")
	require.NoError(t, os.Symlink(real, link))

	projects, complete := claudeProjectSpellings(link)
	require.True(t, complete)
	require.Len(t, projects, 2)
	projects, complete = claudeProjectSpellings(filepath.Join(real, "gone"))
	require.True(t, complete, "a path with nothing there has no other spelling: the search is complete")
	require.Len(t, projects, 1)

	wedgeLaunchDirEvalSymlinks(t)
	projects, complete = claudeProjectSpellings(link)
	require.False(t, complete, "a resolution that did not answer is an incomplete search")
	require.Equal(t, []string{filepath.Join("projects", claudeProjectName(link))}, projects)
}

// The ownership check's recorded side resolves the spellings of the directory
// a transcript records. When they do not resolve within the bound, the names it
// files under are incomplete, and ownership is unknown: never someone else's
// conversation for want of a name, nor this worktree's for want of one.
func TestClaudeTranscriptOwnershipIsUnknownWhenTheRecordedSpellingDoesNotResolve(t *testing.T) {
	recordedDir := filepath.Join(testguard.CanonicalTempDir(t), "recorded")
	require.NoError(t, os.MkdirAll(recordedDir, 0o700))
	path := filepath.Join(t.TempDir(), carryTestID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(claudeRecordLine(t, recordedDir)), 0o600))
	transcript := claudeTranscript{dir: filepath.Dir(path), name: filepath.Base(path)}
	require.Equal(t, claudeOwnedHere, claudeTranscriptOwnership(transcript, recordedDir), "precondition")

	wedgeLaunchDirEvalSymlinks(t)
	facts := withinBound(t, func() claudeRecordedDir { return claudeRecordedDirFacts(recordedDir) })
	require.True(t, facts.unresolved, "a spelling that did not resolve is reported")

	prev := claudeRecordedDirFacts
	claudeRecordedDirFacts = func(string) claudeRecordedDir { return facts }
	t.Cleanup(func() { claudeRecordedDirFacts = prev })
	require.Equal(t, claudeOwnershipUnknown, claudeTranscriptOwnership(transcript, recordedDir))
}
