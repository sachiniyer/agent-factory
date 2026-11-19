package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/log/logtest"
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

// wedgeStoreListing is the store-listing half of launchDirWedge: a directory
// listing under root holds the test's flight clock out past the probe bound
// and then waits until the test ends, the way a transcript store on a wedged
// mount does. Its bound is the clock, never a shrunken probeTimeout, so every
// other flight in the test answers whenever its work finishes (#7593's fix,
// applied to the listing seam the committed re-launch judges, #7596).
func wedgeStoreListing(t *testing.T, root string) {
	t.Helper()
	wait := launchDirWedge(t, root)
	t.Cleanup(fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		wait(dir)
		return os.ReadDir(dir)
	}))
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
	require.Equal(t, blindResumeUnverifiedReason, got.decline.notice,
		"the resume is named as never verified, on its own channel (#7600)")
	require.Empty(t, got.decline.reason, "a blind resume is no refusal")
}

// A blind verdict on a nameable store gets one more look inside the first
// look's deadline (#7600): a listing that only blipped — its error unlatched —
// answers on the retry, and the restore resumes verified, carrying no notice.
func TestRecordedClaudeDeclineRetriesInsideItsBound(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, recorded, _ := claudeRestoreWorktree(t)
	writeCarryFile(t, configDir, filepath.Join(recorded, carryTestID+".jsonl"), "{}\n")
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	var failed sync.Map // each project dir's first listing refuses, as a blip does
	var calls atomic.Int32
	t.Cleanup(fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		if strings.HasPrefix(dir, configDir) {
			calls.Add(1)
			if _, seen := failed.LoadOrStore(dir, struct{}{}); !seen {
				return nil, os.ErrPermission
			}
		}
		return os.ReadDir(dir)
	}))

	program, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.True(t, ok, "the retried listing reads the filed transcript")
	require.Equal(t, "claude --resume "+carryTestID, program)
	// The two spellings blipped in the first look, so a verdict that read the
	// transcript asked again — without the retry the answer is blind here too.
	require.GreaterOrEqual(t, calls.Load(), int32(3), "the blipped verdict looked again")
	require.Empty(t, decline.notice, "a proved resume carries no blind-resume notice")
	require.Empty(t, decline.reason, "a proved resume is no refusal")
}

// The retried listing has no bound of its own — it gets what is left of the
// first look's deadline (#7602's shape, #7600): a store that errs only after
// most of the bound cannot make the restore's two judges cost two bounds. The
// blind verdict that remains names the resume unverified, never a fresh id.
func TestRecordedClaudeDeclineRejudgesInsideTheFirstLooksBound(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, _, _ := claudeRestoreWorktree(t)
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	const bound = 800 * time.Millisecond
	t.Cleanup(fsprobe.SetProbeTimeoutForTest(bound))
	var calls atomic.Int32
	t.Cleanup(fsprobe.SetReadDirForTest(func(dir string) ([]os.DirEntry, error) {
		if strings.HasPrefix(dir, configDir) {
			calls.Add(1)
			time.Sleep(bound - 150*time.Millisecond) // errs only after most of the bound
			return nil, os.ErrPermission
		}
		return os.ReadDir(dir)
	}))

	started := time.Now()
	program, ok, decline := prepareExactResumeConversation(inst, "claude")
	elapsed := time.Since(started)
	require.True(t, ok, "a still-blind verdict resumes the recorded id as before: %s", program)
	require.Equal(t, "claude --resume "+carryTestID, program)
	require.GreaterOrEqual(t, calls.Load(), int32(3), "the retried listing ran")
	require.Less(t, elapsed, bound*9/4,
		"the two judges shared one look's bound — two spellings — : %v for a %v flight bound", elapsed, bound)
	require.Equal(t, blindResumeUnverifiedReason, decline.notice,
		"the retry cut at the shared deadline names the resume unverified")
}

// A store af cannot even name says nothing at all: the resume proceeds as it
// always has, and the notice says its store could not be named — never "could
// not be listed", which would claim a look that never happened (#7600).
func TestRecordedClaudeDeclineNamesAnUnnameableStore(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	workDir, _, _ := claudeRestoreWorktree(t)
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	inst.Account = "ghost" // a deregistered account leaves the store unnameable

	program, ok, decline := prepareExactResumeConversation(inst, "claude")
	require.True(t, ok, "a store af cannot name still resumes as before: %s", program)
	require.Equal(t, "claude --resume "+carryTestID, program)
	require.Equal(t, blindUnnameableResumeReason, decline.notice)
	require.Empty(t, decline.reason, "an unnameable store is no refusal")
}

// A pinned first launch's blind resume is named the way a restore's is
// (#7600): the launch still lands --resume, and the notice says af could not
// verify it first.
func TestPinnedLaunchNamesABlindResume(t *testing.T) {
	configDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	workDir, _, _ := claudeRestoreWorktree(t)
	inst := claudeRestoreInstance(t, workDir, carryTestID)
	inst.Tabs = []*Tab{{}}
	inst.SetResumeRecordedConversation(AgentConversationData{Agent: tmux.ProgramClaude, ID: carryTestID, CaptureKind: ConversationCaptureInjected})
	wedgeStoreListings(t, configDir)
	var warned logtest.Buffer // a retried flight may still log on its goroutine
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warned)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })

	program, _, startedOn := planCreateConversation(inst, "claude", "0e0e0e0e-1111-4222-8333-444444444444")
	require.False(t, startedOn, "a blind store's resume is no start on the id")
	require.Equal(t, "claude --resume "+carryTestID, program,
		"a store af could not look at resumes the pinned conversation as before")
	require.Contains(t, warned.String(), "without verifying it first, because "+blindResumeUnverifiedReason,
		"the launch's log names the blind resume")
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
