package daemon

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestDeliverPrompt_ReemergingRootDefersWhileAttached pins the re-emerging-root
// half of #1638. The re-emerging-root delivery path (deliverToReemergingRoot)
// waits for a momentarily-absent root to be re-materialized, then sends. A TUI
// can attach to root during that wait — PauseStatusPoll leases by (repoID,
// title) even before the session exists — so the path must re-check the defer
// lease before sending, exactly like the fast "exists" path. Before the fix it
// sent unconditionally, pasting an automated prompt into the attached pane.
func TestDeliverPrompt_ReemergingRootDefersWhileAttached(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	// The repo is opted into a root agent, so the ensure loop owns "root" and a
	// momentarily-absent root routes through deliverToReemergingRoot.
	manager, err := NewManager(rootTestConfig(repoPath, config.RootAgentConfig{}))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	// A TUI attaches full-screen to root before it exists — the pause-poll lease
	// is keyed by (repoID, title) and needs no live session.
	manager.PauseStatusPoll(repo.ID, session.RootSessionTitle)

	// A short while into the delivery's wait, the ensure loop re-materializes root
	// in place. The delivery must then DEFER (attached), not send.
	go func() {
		time.Sleep(50 * time.Millisecond)
		if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
			Title:         session.RootSessionTitle,
			RepoPath:      repoPath,
			Program:       "claude",
			InPlace:       true,
			allowReserved: true,
		}); err != nil {
			t.Errorf("background root (re-)create: %v", err)
		}
	}()

	status, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:              session.RootSessionTitle,
		RepoPath:           repoPath,
		Program:            "claude",
		Prompt:             "monitor-event",
		DeferWhileAttached: true,
	})
	if err != nil {
		t.Fatalf("a deferred re-emerging-root delivery must not error: %v", err)
	}
	if status != StatusDeferredAttached {
		t.Fatalf("status = %q, want %q — the re-emerging-root path must honor DeferWhileAttached", status, StatusDeferredAttached)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a deferred delivery must NOT paste into the attached root pane, got %v", got)
	}
}

// TestDeliverPrompt_ConcurrentCreateDefersWhileAttached pins the
// concurrent-create half of #1638. When an outside creator reserves the title
// between DeliverPrompt's existence check and its own reserveCreate, delivery
// waits for the session to materialize (waitForTargetSession) then sends. A TUI
// can attach during that wait, so the retry path must re-check the defer lease
// before sending. Before the fix it sent unconditionally.
func TestDeliverPrompt_ConcurrentCreateDefersWhileAttached(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	rec := installRecordingBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	key := daemonInstanceKey(repo.ID, "captain")

	// An outside creator has RESERVED the title but not yet finished creating the
	// instance: DeliverPrompt's existence check sees no instance (so it is not the
	// fast "exists" path), reserveCreate then rejects the duplicate as a retryable
	// concurrent-create, and delivery drops into waitForTargetSession.
	manager.mu.Lock()
	manager.reservedTitles[key] = struct{}{}
	manager.mu.Unlock()

	// A TUI attaches BEFORE the session exists.
	manager.PauseStatusPoll(repo.ID, "captain")

	var wg sync.WaitGroup
	var status string
	var deliverErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, deliverErr = manager.DeliverPrompt(DeliverPromptRequest{
			Title:              "captain",
			RepoPath:           repoPath,
			Program:            "claude",
			Prompt:             "scheduled-event",
			DeferWhileAttached: true,
		})
	}()

	// Give the delivery goroutine time to enter waitForTargetSession, then let the
	// outside creator "finish": register the instance (recording backend) and drop
	// the reservation, so waitForTargetSession observes the session and returns.
	time.Sleep(100 * time.Millisecond)

	inst, err := session.NewInstance(session.InstanceOptions{Title: "captain", Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec})
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repo.ID, "captain", repoPath)
	manager.mu.Lock()
	manager.instances[key] = inst
	delete(manager.reservedTitles, key)
	manager.mu.Unlock()

	wg.Wait()

	if deliverErr != nil {
		t.Fatalf("a deferred delivery must not error: %v", deliverErr)
	}
	if status != StatusDeferredAttached {
		t.Fatalf("status = %q, want %q — the concurrent-create retry must honor DeferWhileAttached", status, StatusDeferredAttached)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a deferred delivery must NOT paste into the attached session, got %v", got)
	}
}
