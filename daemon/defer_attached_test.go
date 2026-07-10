package daemon

import (
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
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

// TestRecordDeliveryResult_TargetBusyClearsFailureAlarm pins that a deferral is
// not a delivery failure: it never starts the consecutive-failure clock, and it
// CLEARS a stale earlier failure run (Greptile). Otherwise a real failure that
// stamped deliverFailSince, followed by the target staying attached (only
// deferrals from then on), would leave the delivery-failure alarm (#1238) stuck
// on a stale timestamp that nothing ever resets.
func TestRecordDeliveryResult_TargetBusyClearsFailureAlarm(t *testing.T) {
	w := &taskWatcher{}

	// A deferral on its own leaves the failure clock unset.
	w.recordDeliveryResult(time.Now(), errTargetBusy)
	if !w.deliverFailSince.IsZero() {
		t.Fatalf("a deferral must not start the failure clock; deliverFailSince=%v", w.deliverFailSince)
	}

	// A real failure starts the clock.
	w.recordDeliveryResult(time.Now(), errors.New("target unreachable (outage)"))
	if w.deliverFailSince.IsZero() {
		t.Fatal("a real delivery failure must start the failure clock")
	}

	// A subsequent deferral must CLEAR that stale failure run so the alarm goes
	// quiet while the target is attached (a real problem re-stamps on the next
	// live attempt after detach).
	w.recordDeliveryResult(time.Now(), errTargetBusy)
	if !w.deliverFailSince.IsZero() {
		t.Fatalf("a deferral must clear a stale failure run; deliverFailSince=%v", w.deliverFailSince)
	}
	if w.deliverFailCount != 0 || w.deliverFailErr != "" {
		t.Fatalf("a deferral must reset the failure count/error; count=%d err=%q", w.deliverFailCount, w.deliverFailErr)
	}
}

// TestReleaseEventSlot_RefundsDeferredRateSlot pins Greptile #3: a deferral
// delivers nothing, so refunding its reserved rate slot must leave the target's
// per-minute budget exactly as it was — otherwise the live attempt and the
// drainer's replay would each spend a slot and could starve real deliveries.
func TestReleaseEventSlot_RefundsDeferredRateSlot(t *testing.T) {
	s := newWatcherSupervisor()
	s.eventsPerMinute = 10
	w := &taskWatcher{sup: s}

	if !w.tryReserveEventSlot() {
		t.Fatal("first reservation should succeed")
	}
	if got := len(w.eventTimes); got != 1 {
		t.Fatalf("reserved slots = %d, want 1", got)
	}
	// A deferral refunds the slot it reserved.
	w.releaseEventSlot()
	if got := len(w.eventTimes); got != 0 {
		t.Fatalf("a refunded deferral must leave 0 slots spent, got %d", got)
	}
	// Refund is safe to over-call (never panics / goes negative).
	w.releaseEventSlot()
	if got := len(w.eventTimes); got != 0 {
		t.Fatalf("refunding an empty window must stay at 0, got %d", got)
	}
}

// TestDeliverCronTaskPrompt_CatchesUpOnDetach pins Greptile #1: cron has no
// durable queue, so a deferred occurrence must be caught up when the target
// detaches rather than silently skipped. The delivery is held while attached
// and lands ("sent") on the first attempt after detach.
func TestDeliverCronTaskPrompt_CatchesUpOnDetach(t *testing.T) {
	origPoll := cronDeferPollInterval
	cronDeferPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { cronDeferPollInterval = origPoll })

	var attached atomic.Bool
	attached.Store(true)
	var attempts atomic.Int32
	firstAttempt := make(chan struct{})
	var once sync.Once
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		attempts.Add(1)
		if req.DeferWhileAttached && attached.Load() {
			once.Do(func() { close(firstAttempt) })
			return StatusDeferredAttached, nil
		}
		return "sent", nil
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tsk := &task.Task{ID: "aa158601", TargetSession: "captain", ProjectPath: t.TempDir(), Prompt: "cron-event"}

	done := make(chan struct{})
	var status string
	var err error
	go func() {
		status, err = deliverCronTaskPrompt(tsk, tsk.Prompt)
		close(done)
	}()

	// Wait until it has actually made a deferred attempt while attached (avoids a
	// goroutine-start race), then confirm it is parked — held, not skipped.
	select {
	case <-firstAttempt:
	case <-time.After(5 * time.Second):
		t.Fatal("cron delivery never made its first (deferred) attempt")
	}
	select {
	case <-done:
		t.Fatalf("cron delivery resolved (%q) while the target was still attached; it must wait, not skip", status)
	case <-time.After(30 * time.Millisecond):
	}

	// On detach the next attempt delivers the caught-up occurrence.
	attached.Store(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cron delivery did not catch up after detach")
	}
	if err != nil {
		t.Fatalf("cron catch-up: %v", err)
	}
	if status != "sent" {
		t.Fatalf("post-detach cron status = %q, want \"sent\"", status)
	}
	if attempts.Load() < 2 {
		t.Fatalf("expected at least one held attempt then a catch-up, got %d attempts", attempts.Load())
	}
}

// TestRunTask_CronFiresCoalesceDuringDefer pins the DOCUMENTED design choice at
// taskrun.go's non-blocking flock: a cron firing more often than an attach lasts
// delivers exactly ONE prompt on detach, not one per skipped occurrence. While
// the first fire is parked waiting for detach (holding the per-task lock), every
// overlapping fire hits LOCK_NB, is rejected, and exits without queuing — so the
// idempotent, fixed cron prompt is delivered once on detach rather than as a
// duplicate burst. (Watch events, which carry distinct payloads, are NOT
// coalesced — they queue durably and replay in order.)
func TestRunTask_CronFiresCoalesceDuringDefer(t *testing.T) {
	origPoll := cronDeferPollInterval
	cronDeferPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { cronDeferPollInterval = origPoll })

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repo := setupTaskRepo(t)
	if err := task.AddTask(task.Task{
		ID:            "cccc1586",
		CronExpr:      "* * * * *",
		Enabled:       true,
		TargetSession: "captain",
		ProjectPath:   repo,
		Program:       "claude",
		Prompt:        "run nightly",
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	var attached atomic.Bool
	attached.Store(true)
	var delivered atomic.Int32
	firstAttempt := make(chan struct{})
	var once sync.Once
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		if req.DeferWhileAttached && attached.Load() {
			// The first held attempt proves the parked fire now owns the lock.
			once.Do(func() { close(firstAttempt) })
			return StatusDeferredAttached, nil
		}
		delivered.Add(1)
		return "sent", nil
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	// Fire #1 parks in the defer wait, holding the per-task flock.
	firstDone := make(chan error, 1)
	go func() { firstDone <- RunTask("cccc1586") }()
	select {
	case <-firstAttempt:
	case <-time.After(5 * time.Second):
		t.Fatal("the first cron fire never reached its (deferred) delivery attempt")
	}

	// Subsequent cron ticks during the attach are coalesced away by LOCK_NB.
	const overlaps = 5
	for i := 0; i < overlaps; i++ {
		err := RunTask("cccc1586")
		if err == nil || !strings.Contains(err.Error(), "another run is already active") {
			t.Fatalf("overlapping fire %d must be rejected while one is parked, got err=%v", i, err)
		}
	}
	if got := delivered.Load(); got != 0 {
		t.Fatalf("nothing may deliver while the target is attached, got %d", got)
	}

	// Detach: the single parked fire delivers exactly once — the coalesce.
	attached.Store(false)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("parked cron fire: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the parked cron fire did not deliver after detach")
	}
	if got := delivered.Load(); got != 1 {
		t.Fatalf("frequent cron + long attach must coalesce to exactly ONE delivery on detach, got %d", got)
	}
}

// TestDeliverCronTaskPrompt_NeverPastesWhileAttached pins the core #1586
// invariant on the catch-up valve: a cron target that stays attached — however
// long, well past any prior bound — is NEVER pasted into while attached (that
// would be the exact in-progress-input collision this path prevents). The held
// fire keeps deferring and delivers ONLY once the target detaches. Every attempt
// while attached carries the deferral ON (never a forced defer-off delivery).
func TestDeliverCronTaskPrompt_NeverPastesWhileAttached(t *testing.T) {
	origPoll := cronDeferPollInterval
	cronDeferPollInterval = 2 * time.Millisecond
	t.Cleanup(func() { cronDeferPollInterval = origPoll })

	var attached atomic.Bool
	attached.Store(true)
	var attempts atomic.Int32
	var pasted atomic.Int32
	var forcedWhileAttached atomic.Bool
	firstAttempt := make(chan struct{})
	var once sync.Once
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (string, error) {
		attempts.Add(1)
		once.Do(func() { close(firstAttempt) })
		if attached.Load() {
			// A forced (defer-off) attempt while still attached would paste into
			// the user's pane — the bug. Flag it; the deferral must always be on
			// while attached.
			if !req.DeferWhileAttached {
				forcedWhileAttached.Store(true)
			}
			return StatusDeferredAttached, nil
		}
		pasted.Add(1)
		return "sent", nil
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	tsk := &task.Task{ID: "aa158602", TargetSession: "captain", ProjectPath: t.TempDir(), Prompt: "cron-event"}

	done := make(chan struct{})
	var status string
	var err error
	go func() {
		status, err = deliverCronTaskPrompt(tsk, tsk.Prompt)
		close(done)
	}()

	<-firstAttempt
	// Let it poll MANY times while attached — well past any old bound — and prove
	// nothing is ever pasted and no forced defer-off attempt slips through.
	waitUntil(t, 5*time.Second, "the held fire to poll well past any prior bound", func() bool {
		return attempts.Load() >= 50
	})
	if got := pasted.Load(); got != 0 {
		t.Fatalf("a cron prompt must NEVER be pasted while the target is attached, pasted=%d", got)
	}
	if forcedWhileAttached.Load() {
		t.Fatal("the held fire must never send a forced (defer-off) attempt while attached")
	}
	select {
	case <-done:
		t.Fatalf("delivery resolved (%q) while still attached — it must keep deferring until detach", status)
	default:
	}

	// Detach → it delivers exactly once, now that the pane is unattended.
	attached.Store(false)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery did not land after detach")
	}
	if err != nil {
		t.Fatalf("post-detach delivery: %v", err)
	}
	if status != "sent" {
		t.Fatalf("post-detach status = %q, want \"sent\"", status)
	}
	if got := pasted.Load(); got != 1 {
		t.Fatalf("exactly one delivery on detach, got %d", got)
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
