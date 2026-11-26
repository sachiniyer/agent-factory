package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The #7843 transition tests: a delivery parked at a usage limit is logged once
// on entry, silent through every re-check the drainer's ~10s cadence makes,
// logged once on exit, and announced again if a later delivery re-parks.

// isolateParkedDeliveryMarks empties the process-global mark table for the
// test's lifetime, so episodes a neighbor test announced cannot suppress —
// or seed — the transitions asserted here.
func isolateParkedDeliveryMarks(t *testing.T) {
	t.Helper()
	taskParkedDeliveries.mu.Lock()
	saved := taskParkedDeliveries.marks
	taskParkedDeliveries.marks = make(map[string]parkedDeliveryMark)
	taskParkedDeliveries.mu.Unlock()
	t.Cleanup(func() {
		taskParkedDeliveries.mu.Lock()
		taskParkedDeliveries.marks = saved
		taskParkedDeliveries.mu.Unlock()
	})
}

// stubParkedDelivery installs a deliverPromptForTask that reports
// TaskStatusLimitParked while *parked holds and "sent" once it clears — the two
// verdicts the drainer's re-check loop actually alternates between.
func stubParkedDelivery(t *testing.T, parked *bool) {
	t.Helper()
	orig := deliverPromptForTask
	deliverPromptForTask = func(DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		if *parked {
			return taskPromptDeliveryResult{status: TaskStatusLimitParked}, nil
		}
		return taskPromptDeliveryResult{status: "sent", deliveryStatus: session.PromptDelivered}, nil
	}
	t.Cleanup(func() { deliverPromptForTask = orig })
}

func countLogLines(logText, needle string) int {
	n := 0
	for _, line := range strings.Split(logText, "\n") {
		if strings.Contains(line, needle) {
			n++
		}
	}
	return n
}

// TestParkedDeliveryLoggedPerTransition is the headline property: N
// consecutive parked re-checks produce exactly one park line, a delivery ends
// the episode once, and a re-park is a new transition that announces again.
func TestParkedDeliveryLoggedPerTransition(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	parked := true
	stubParkedDelivery(t, &parked)

	tsk := &task.Task{ID: "park7843", TargetSession: "limited", Enabled: true}
	const checks = 20
	for i := 0; i < checks; i++ {
		status, err := deliverTaskPrompt(tsk, "prompt", true)
		if err != nil || status != TaskStatusLimitParked {
			t.Fatalf("check %d: status=%q err=%v, want parked", i, status, err)
		}
	}
	if got := countLogLines(infoLog.String(), "at a usage limit; prompt not sent"); got != 1 {
		t.Fatalf("%d parked re-checks wrote %d park lines, want 1:\n%s", checks, got, infoLog.String())
	}

	// The delivery landing is the exit transition: one "ended" line, then the
	// ordinary delivered line — and every re-check before it stayed silent.
	parked = false
	if _, err := deliverTaskPrompt(tsk, "prompt", true); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got := countLogLines(infoLog.String(), "ended"); got != 1 {
		t.Fatalf("park exit wrote %d ended lines, want 1:\n%s", got, infoLog.String())
	}

	// Re-parking after a delivery is a fresh transition and must announce.
	parked = true
	if _, err := deliverTaskPrompt(tsk, "prompt", true); err != nil {
		t.Fatalf("re-park: %v", err)
	}
	if got := countLogLines(infoLog.String(), "at a usage limit; prompt not sent"); got != 2 {
		t.Fatalf("re-park after delivery wrote %d park lines total, want 2:\n%s", got, infoLog.String())
	}
}

// TestParkedDeliveryLogReminder: while an episode holds, at most one heartbeat
// lands per parkedDeliveryReminder — the frozen clock makes the cadence exact.
func TestParkedDeliveryLogReminder(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	advance := withFrozenClock(t)
	parked := true
	stubParkedDelivery(t, &parked)

	tsk := &task.Task{ID: "parkrem", TargetSession: "limited", Enabled: true}
	_, _ = deliverTaskPrompt(tsk, "prompt", true) // announces
	for i := 0; i < 5; i++ {
		advance(10 * time.Second) // the drainer's cadence
		_, _ = deliverTaskPrompt(tsk, "prompt", true)
	}
	if got := countLogLines(infoLog.String(), "still parked"); got != 0 {
		t.Fatalf("re-checks inside the reminder window wrote %d heartbeats, want 0:\n%s", got, infoLog.String())
	}
	advance(parkedDeliveryReminder + time.Second)
	_, _ = deliverTaskPrompt(tsk, "prompt", true)
	if got := countLogLines(infoLog.String(), "still parked"); got != 1 {
		t.Fatalf("the window lapse wrote %d heartbeats, want 1:\n%s", got, infoLog.String())
	}
	if !strings.Contains(infoLog.String(), "re-checks") {
		t.Fatalf("heartbeat must name the suppressed re-check count:\n%s", infoLog.String())
	}
}

// TestParkedDeliveryRetargetAnnounces: a task retargeted while parked ends the
// old episode by name, and the new target's park is its own transition.
func TestParkedDeliveryRetargetAnnounces(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	parked := true
	stubParkedDelivery(t, &parked)

	tsk := &task.Task{ID: "parkrt", TargetSession: "old-target", Enabled: true}
	_, _ = deliverTaskPrompt(tsk, "prompt", true)

	tsk.TargetSession = "new-target"
	_, _ = deliverTaskPrompt(tsk, "prompt", true)

	text := infoLog.String()
	if !strings.Contains(text, `parked delivery to target session "old-target" ended: the task was retargeted to "new-target"`) {
		t.Fatalf("retarget must end the old park episode by name:\n%s", text)
	}
	if !strings.Contains(text, `parked delivery to target session "new-target" at a usage limit`) {
		t.Fatalf("the new target's park must announce itself:\n%s", text)
	}
}

// TestParkedDeliveryLogNamesResetTime: when the parked row carries a recorded
// reset time, the one park line names it (#7843's "if the limit reset time is
// known, name it").
func TestParkedDeliveryLogNamesResetTime(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	resetAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	inst := registerStarted(t, manager, repoID, repoPath, "limited", session.NewFakeBackend(), true, session.Running)
	inst.SetLimitReached(resetAt)
	if err := persistInstanceData(repoID, inst.ToInstanceData()); err != nil {
		t.Fatalf("persist parked row: %v", err)
	}

	parked := true
	stubParkedDelivery(t, &parked)
	tsk := &task.Task{ID: "parkreset", TargetSession: "limited", ProjectPath: repoPath, Enabled: true}
	if _, err := deliverTaskPrompt(tsk, "prompt", true); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	want := "the recorded limit resets at " + resetAt.Format(time.RFC3339)
	if !strings.Contains(infoLog.String(), want) {
		t.Fatalf("park line must name the reset time %q:\n%s", want, infoLog.String())
	}
}

// TestParkedDeliverySweepEndsOrphanedMarks: the armed-set reload retires marks
// whose task can no longer reach the parked target — removed, disabled, or
// retargeted — so a re-park under the same ID is never silently swallowed.
func TestParkedDeliverySweepEndsOrphanedMarks(t *testing.T) {
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	now := nowFunc()
	for _, id := range []string{"gone", "off", "moved", "same"} {
		taskParkedDeliveries.marks[id] = parkedDeliveryMark{target: "limited", lastNote: now}
	}

	taskParkedDeliveries.sweep([]task.Task{
		{ID: "off", Enabled: false, TargetSession: "limited"},
		{ID: "moved", Enabled: true, TargetSession: "elsewhere"},
		{ID: "same", Enabled: true, TargetSession: "limited"},
	})

	text := infoLog.String()
	for _, want := range []string{
		`task gone: parked delivery to target session "limited" ended: the task was removed`,
		`task off: parked delivery to target session "limited" ended: the task was disabled`,
		`task moved: parked delivery to target session "limited" ended: the task was retargeted to "elsewhere"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("sweep must log %q:\n%s", want, text)
		}
	}
	taskParkedDeliveries.mu.Lock()
	_, kept := taskParkedDeliveries.marks["same"]
	left := len(taskParkedDeliveries.marks)
	taskParkedDeliveries.mu.Unlock()
	if !kept || left != 1 {
		t.Fatalf("sweep kept=%v left=%d, want the unchanged task's mark alone", kept, left)
	}
}

// TestParkedDeliveryTrackerExitIsIdempotent: exits with no announced park are
// silent — the drainer's drop and expire paths call noteExit on every head
// they consume, and only parked ones may log.
func TestParkedDeliveryTrackerExitIsIdempotent(t *testing.T) {
	isolateParkedDeliveryMarks(t)
	infoLog := captureInfo(t)
	taskParkedDeliveries.noteExit("never-parked", "the queued event expired before delivery")
	if got := countLogLines(infoLog.String(), "never-parked"); got != 0 {
		t.Fatalf("noteExit on an unmarked task logged %d lines, want 0:\n%s", got, infoLog.String())
	}
}
