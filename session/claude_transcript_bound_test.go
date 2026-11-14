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

	state := withinBound(t, func() ClaudeProjectConversationState {
		state, err := InspectClaudeProjectConversations("CLAUDE_CONFIG_DIR="+configDir+" claude", link,
			AgentConversationData{Agent: tmux.ProgramClaude, ID: recordedID})
		require.NoError(t, err)
		return state
	})
	require.True(t, state.RecordedExists, "a transcript filed under the recorded spelling is found even when the resolution times out")
	require.Equal(t, recordedID, state.Resume.ID)
}
