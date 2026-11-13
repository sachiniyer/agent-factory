package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// #7212: the restore and load paths, and the poll's conversation capture, list
// the agent's conversation store: Claude's ~/.claude/projects/<spelling>, and
// Codex's sessions tree. A store on a wedged mount (NFS, FUSE, a dead sshfs)
// never answers that listing, and an unbounded one stalled the poll goroutine
// with it. No listing may block past a bound; one that does not answer counts
// as "could not look", which resumes and retires nothing.

// wedgeStoreListings makes every directory listing under root block until the
// test ends, the way a wedged mount does, and bounds af's listings at 50ms.
func wedgeStoreListings(t *testing.T, root string) {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	restoreRead := fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		if strings.HasPrefix(dir, root) {
			<-release
		}
		return os.ReadDir(dir)
	})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		once.Do(func() { close(release) })
		// Let any flight still blocked in the reader finish before the real
		// reader comes back.
		time.Sleep(50 * time.Millisecond)
		restoreRead()
		restoreTimeout()
	})
}

// withinBound runs judge and fails the test if it is still running after a
// bound far beyond fsprobe's: a stalled listing would hold it forever.
func withinBound[T any](t *testing.T, judge func() T) T {
	t.Helper()
	done := make(chan T, 1)
	go func() { done <- judge() }()
	select {
	case got := <-done:
		return got
	case <-time.After(3 * time.Second):
		t.Fatal("a conversation-store listing on a wedged mount stalled the caller")
		var zero T
		return zero
	}
}

type exactResumeAnswer struct {
	program string
	ok      bool
	decline resumeDecline
}

func TestRestoreGuardDoesNotStallOnAWedgedClaudeStore(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recorded, _ := claudeRestoreWorktree(t)
	writeCarryFile(t, configDir, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	wedgeStoreListings(t, configDir)

	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "claude")
		return exactResumeAnswer{program, ok, decline}
	})
	require.True(t, got.ok, "a store af could not look at says nothing about the id: the restore resumes it as before")
	require.Equal(t, "claude --resume "+carryTestID, got.program)
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "nothing is retired on a look that did not happen")
}

// codexRestoreInstance is a Codex session whose agent tab recorded id, in a
// worktree af created for it, writing to the CODEX_HOME the test set.
func codexRestoreInstance(t *testing.T, id string) *Instance {
	t.Helper()
	repo, workDir := t.TempDir(), t.TempDir()
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, workDir, "t", "main", "", false, true)
	require.NoError(t, err)
	return &Instance{
		Title: "t", Program: tmux.ProgramCodex, Path: repo, gitWorktree: gw,
		Tabs: []*Tab{{Conversation: AgentConversationData{
			Agent: tmux.ProgramCodex, ID: id, CapturedAt: time.Now(), CaptureKind: ConversationCaptureCodexRollout,
		}}},
	}
}

func TestRestoreGuardDoesNotStallOnAWedgedCodexStore(t *testing.T) {
	const id = "019a0000-0000-7000-8000-000000000042"
	codexHome := t.TempDir()
	t.Setenv("CODEX_HOME", codexHome)
	writeCodexRolloutFile(t, codexHome, "rollout-2026-11-12T10-17-35-"+id+".jsonl")
	inst := codexRestoreInstance(t, id)
	wedgeStoreListings(t, codexHome)

	got := withinBound(t, func() exactResumeAnswer {
		program, ok, decline := prepareExactResumeConversation(inst, "codex")
		return exactResumeAnswer{program, ok, decline}
	})
	require.False(t, got.ok)
	require.True(t, got.decline.unverified, "a listing that did not answer is the unverified decline: %+v", got.decline)
	require.False(t, got.decline.retiresRecord(inst.AgentConversation()), "an unverified decline retires nothing")
}

// The poll's conversation capture walks the same tree on every arm.
func TestCodexCaptureScanDoesNotStallOnAWedgedStore(t *testing.T) {
	codexHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(codexHome, "sessions"), 0o755))
	wedgeStoreListings(t, codexHome)

	err := withinBound(t, func() error {
		_, err := codexRolloutFilesSince(codexHome, time.Now())
		return err
	})
	require.ErrorIs(t, err, fsprobe.ErrProbeTimedOut, "the scan reports that it could not look, never an empty store")
}
