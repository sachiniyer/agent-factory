package daemon

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestDeliverPrompt_DefersWhileTargetAttached is the #1586 core: an automated
// task delivery (DeferWhileAttached) into a session a TUI is attached
// full-screen to must NOT paste a prompt + Enter into the pane the user is
// typing in — which would append to and submit their half-typed message. It is
// deferred instead (status "deferred: target attached", nothing sent), and the
// same delivery lands normally once the user detaches.
func TestDeliverPrompt_DefersWhileTargetAttached(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	rec := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec}
	registerStarted(t, manager, repoID, repoPath, "captain", backend, true, session.Running)

	// A TUI attaches full-screen to the target: the daemon's pause-poll lease is
	// the "attached" signal (#1160) the defer reuses.
	manager.PauseStatusPoll(repoID, "captain")

	status, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:              "captain",
		RepoPath:           repoPath,
		Program:            "claude",
		Prompt:             "scheduled-event",
		DeferWhileAttached: true,
	})
	if err != nil {
		t.Fatalf("a deferred delivery must not error: %v", err)
	}
	if status != StatusDeferredAttached {
		t.Fatalf("status = %q, want %q", status, StatusDeferredAttached)
	}
	if got := rec.snapshot(); len(got) != 0 {
		t.Fatalf("a deferred delivery must NOT paste into the attached pane, got %v", got)
	}

	// On detach the very same delivery lands normally.
	manager.ResumeStatusPoll(repoID, "captain")
	status, err = manager.DeliverPrompt(DeliverPromptRequest{
		Title:              "captain",
		RepoPath:           repoPath,
		Program:            "claude",
		Prompt:             "scheduled-event",
		DeferWhileAttached: true,
	})
	if err != nil {
		t.Fatalf("post-detach delivery: %v", err)
	}
	if status != "sent" {
		t.Fatalf("post-detach status = %q, want \"sent\"", status)
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != "scheduled-event" {
		t.Fatalf("post-detach delivery must land exactly once, got %v", got)
	}
}

// TestDeliverPrompt_ManualSendDeliversWhileTargetAttached pins that the defer
// is scoped to automated deliveries: a manual send (DeferWhileAttached unset,
// as `af sessions send-prompt` leaves it) is an explicit user action and still
// lands immediately even while the target is attached.
func TestDeliverPrompt_ManualSendDeliversWhileTargetAttached(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	rec := &promptRecorder{}
	backend := recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec}
	registerStarted(t, manager, repoID, repoPath, "captain", backend, true, session.Running)

	manager.PauseStatusPoll(repoID, "captain")

	status, err := manager.DeliverPrompt(DeliverPromptRequest{
		Title:    "captain",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "manual-now",
		// DeferWhileAttached deliberately left false.
	})
	if err != nil {
		t.Fatalf("manual send: %v", err)
	}
	if status != "sent" {
		t.Fatalf("manual send status = %q, want \"sent\" (must not defer)", status)
	}
	if got := rec.snapshot(); len(got) != 1 || got[0] != "manual-now" {
		t.Fatalf("a manual send must land immediately even while attached, got %v", got)
	}
}

// TestRecordDeliveryResult_TargetBusyDoesNotAlarm pins that a deferral is
// neither a success nor a pipeline failure: it must not start the
// consecutive-failure clock (so a long attach never trips the delivery-failure
// alarm, #1238) and must not clear a genuine prior failure run.
func TestRecordDeliveryResult_TargetBusyDoesNotAlarm(t *testing.T) {
	w := &taskWatcher{}

	// A deferral on its own leaves the failure clock unset.
	w.recordDeliveryResult(time.Now(), errTargetBusy)
	if !w.deliverFailSince.IsZero() {
		t.Fatalf("a deferral must not start the failure clock; deliverFailSince=%v", w.deliverFailSince)
	}

	// A real failure starts the clock.
	w.recordDeliveryResult(time.Now(), errors.New("target unreachable (outage)"))
	since := w.deliverFailSince
	if since.IsZero() {
		t.Fatal("a real delivery failure must start the failure clock")
	}

	// A subsequent deferral must leave that real failure run untouched.
	w.recordDeliveryResult(time.Now(), errTargetBusy)
	if w.deliverFailSince != since {
		t.Fatalf("a deferral must not disturb a prior failure run: since %v -> %v", since, w.deliverFailSince)
	}

	// A success still clears the run.
	w.recordDeliveryResult(time.Now(), nil)
	if !w.deliverFailSince.IsZero() {
		t.Fatalf("a success must clear the failure clock; deliverFailSince=%v", w.deliverFailSince)
	}
}

// busyDeliver defers every delivery while attached (returning errTargetBusy),
// then records successes once detached — the attach→detach shape of #1586
// driven end to end through a real watcher.
type busyDeliver struct {
	mu       sync.Mutex
	attached atomic.Bool
	success  []string
}

func (d *busyDeliver) deliver(_, line string) error {
	if d.attached.Load() {
		return errTargetBusy
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.success = append(d.success, line)
	return nil
}

func (d *busyDeliver) delivered() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.success...)
}

// TestWatcher_DefersDeliveryWhileTargetAttached is the watch half of #1586:
// events emitted while a TUI is attached to the target are durably queued (never
// pasted into live typing), the deferral does not trip the delivery-failure
// alarm, and the whole backlog drains in emission order once the user detaches —
// no event lost, none reordered.
func TestWatcher_DefersDeliveryWhileTargetAttached(t *testing.T) {
	dir := t.TempDir()
	script := `echo e1; echo e2; echo e3; sleep 60`
	s, _ := newTestSupervisor(t, staticTasks(watchTask("ab158601", script, dir)))
	bd := &busyDeliver{}
	bd.attached.Store(true) // a TUI is attached to the target for now
	s.deliver = bd.deliver

	if err := s.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	// All three events fire while attached; nothing may be delivered — they queue.
	queueDir, _ := s.queueDir()
	waitUntil(t, 10*time.Second, "all events queued while the target is attached", func() bool {
		return newEventQueue(queueDir, "ab158601").pendingCount() == 3
	})
	if got := bd.delivered(); len(got) != 0 {
		t.Fatalf("no event may be delivered into an attached session, got %v", got)
	}

	// A deferral must not look like a delivery outage: the failure clock stays
	// unset, so the delivery-failure alarm never fires during a normal attach.
	s.mu.Lock()
	w := s.watchers["ab158601"]
	s.mu.Unlock()
	if w == nil {
		t.Fatal("watcher for ab158601 not registered")
	}
	w.mu.Lock()
	since := w.deliverFailSince
	w.mu.Unlock()
	if !since.IsZero() {
		t.Fatalf("a deferral must not start the delivery-failure clock; deliverFailSince=%v", since)
	}

	// The user detaches: the backlog drains in emission order.
	bd.attached.Store(false)
	waitUntil(t, 10*time.Second, "the backlog to drain in order after detach", func() bool {
		got := bd.delivered()
		return len(got) == 3 && got[0] == "e1" && got[1] == "e2" && got[2] == "e3"
	})
}
