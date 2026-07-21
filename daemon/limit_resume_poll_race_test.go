package daemon

import (
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// limitRacePollBackend is a FakeBackend that reproduces the #2135 interleaving
// exactly: its pane capture returns a fixed usage-limit banner every idle tick,
// and it runs a one-shot hook AFTER the content has been captured but BEFORE the
// poll acts on it. The hook is where the racing resume lands, so the poll's
// decision is provably made from PRE-resume content.
//
// Driving the race from inside the capture (rather than from a second goroutine)
// makes it deterministic: the poll goroutine is the one that runs the resume, so
// there is no sleep, no barrier, and no flake.
type limitRacePollBackend struct {
	*session.FakeBackend
	mu           sync.Mutex
	content      string
	afterCapture func()
	sentPrompts  []string
}

func (b *limitRacePollBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	b.mu.Lock()
	content := b.content
	hook := b.afterCapture
	b.afterCapture = nil // one-shot: later ticks are ordinary polls
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return false, false, content
}

func (b *limitRacePollBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.sentPrompts = append(b.sentPrompts, prompt)
	return nil
}

func (b *limitRacePollBackend) prompts() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sentPrompts...)
}

func (b *limitRacePollBackend) setHook(fn func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.afterCapture = fn
}

// TestResumeFromLimit_ConcurrentPollCannotRevertResume is the #2135 regression: a
// poll tick that captured its pane content BEFORE a resume must not re-park the
// session at the usage-limit wall using that stale content.
//
// The interleaving: the poll snapshots a pane still showing the limit banner, the
// resume then clears the limit, re-delivers the prompt and persists LiveRunning,
// and only then does the poll run the detector over its stale capture. Before the
// fix that detector hit and called SetLimitReached, reverting the session to
// LiveLimitReached in memory; persistPollChange then flushed it to DISK too,
// because the reset time had changed even though the liveness compare read
// unchanged (LimitReached → LimitReached). The user was shown a limit-blocked
// session that was in fact working, and a daemon restart reloaded the wrong state.
func TestResumeFromLimit_ConcurrentPollCannotRevertResume(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"

	// Park it at the wall with a reset time that differs from the one the banner
	// parses to, so the poll's re-detection changes the reset time — the exact
	// shape that made persistPollChange flush the reverted state to disk.
	inst.SetLimitReached(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	manager.persistInstance(repoID, inst)

	// The resume lands between the poll's capture and the poll's decision.
	backend.setHook(func() {
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	})

	manager.RefreshStatuses()

	if got := backend.prompts(); len(got) != 1 || got[0] != "finish the migration" {
		t.Fatalf("delivered prompts = %v, want exactly the stored prompt (the resume must have landed)", got)
	}
	if inst.LimitReached() {
		t.Errorf("in-memory liveness = %v, want LiveRunning: a poll deciding from PRE-resume content must not re-park the session (#2135)", inst.GetLiveness())
	}
	if got := inst.GetLiveness(); got != session.LiveRunning {
		t.Errorf("in-memory liveness = %v, want LiveRunning (#2135)", got)
	}
	if got, ok := inst.LimitResetAt(); ok || !got.IsZero() {
		t.Errorf("in-memory reset time = (%v, %v), want (zero, false) after a successful resume (#2135)", got, ok)
	}
	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveRunning {
		t.Errorf("persisted liveness = %v, want LiveRunning: the stale poll must not reach disk (#2135)", got)
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.IsZero() {
		t.Errorf("persisted reset time = %v, want zero (#2135)", got)
	}
}

// TestRefreshStatuses_GenuineLimitHitAfterResumeStillParks is the
// over-suppression control for #2135: the fix drops only a decision made from
// content captured before a transition, never limit detection in general. A LATER
// tick — fresh capture, no racing resume — that still sees the banner must park
// the session again, in memory and on disk.
//
// This is what rules out a "suppress limit detection for N seconds after a
// resume" fix: an agent that immediately walks back into the wall would go
// undetected for the whole window.
func TestRefreshStatuses_GenuineLimitHitAfterResumeStillParks(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"
	inst.SetLimitReached(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	manager.persistInstance(repoID, inst)

	backend.setHook(func() {
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	})
	manager.RefreshStatuses()
	if inst.LimitReached() {
		t.Fatalf("precondition: the resumed session must not be limit-blocked, got %v", inst.GetLiveness())
	}

	// Next tick: no resume in flight, the pane still shows the banner. This is a
	// genuine observation and must park the session.
	manager.RefreshStatuses()

	if !inst.LimitReached() {
		t.Fatalf("in-memory liveness = %v, want LiveLimitReached: a fresh limit observation must still park the session", inst.GetLiveness())
	}
	if _, ok := inst.LimitResetAt(); !ok {
		t.Error("a parseable reset time must be stored for the badge")
	}
	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveLimitReached {
		t.Errorf("persisted liveness = %v, want LiveLimitReached", got)
	}
}

// TestPersistPollChange_ResumeDuringWriteWindowIsNotOverwritten is the second
// half of #2135, and it survives the first: even when the poll's limit detection
// is GENUINE — fresh content, nothing stale about it — the write itself happens
// later, after a repo start lock a session create can hold for seconds. A resume
// landing in that window cleared the limit and persisted LiveRunning; the poll
// then flushed the payload it had decided from and put the limit-blocked row back
// on disk, where a daemon restart would reload it.
//
// The poll must persist what is TRUE at write time, not the intermediate it
// decided from.
func TestPersistPollChange_ResumeDuringWriteWindowIsNotOverwritten(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Prompt = "finish the migration"

	// The poll's own decision, made from content that is genuinely current: park
	// the session at the wall. This is what it is about to write.
	before := inst.GetLiveness()
	beforeReset, _ := inst.LimitResetAt()
	inst.SetLimitReached(time.Date(2026, 7, 20, 18, 0, 0, 0, time.UTC))

	// The resume lands in the write window: after the poll read its payload, before
	// it takes the repo start lock.
	prev := testHookPollBeforePersistLock
	t.Cleanup(func() { testHookPollBeforePersistLock = prev })
	once := false
	testHookPollBeforePersistLock = func() {
		if once {
			return
		}
		once = true
		if err := manager.resumeFromLimit(ResumeFromLimitRequest{Title: "limited", RepoID: repoID}); err != nil {
			t.Errorf("resumeFromLimit: %v", err)
		}
	}

	manager.persistPollChange(repoID, inst, before, beforeReset, false)

	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveRunning {
		t.Errorf("persisted liveness = %v, want LiveRunning: the poll must not overwrite a resume that landed while it waited for the write lock (#2135)", got)
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.IsZero() {
		t.Errorf("persisted reset time = %v, want zero (#2135)", got)
	}
}

// TestRefreshStatuses_OrdinaryPollUnaffectedByEpochGuard is the plain-path
// control: an idle session with no limit banner and nothing racing it still
// settles Ready and still persists that transition. The #2135 guard must be
// invisible to the ordinary poll.
func TestRefreshStatuses_OrdinaryPollUnaffectedByEpochGuard(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitRacePollBackend{FakeBackend: session.NewFakeBackend(), content: "$ "}
	inst := registerStarted(t, manager, repoID, repoPath, "idle", backend, true, session.Running)

	manager.RefreshStatuses()

	if got := inst.GetLiveness(); got != session.LiveReady {
		t.Fatalf("in-memory liveness = %v, want LiveReady", got)
	}
	if got := persistedLiveness(t, repoID, "idle"); got != session.LiveReady {
		t.Fatalf("persisted liveness = %v, want LiveReady", got)
	}
}
