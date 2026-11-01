package daemon

import (
	"testing"
	"time"
)

// TestResumeLimitedSessions_DefersWhileAttached pins #5412: the scheduled
// auto-resume pastes the stored prompt and presses Enter, so while a TUI holds
// the attach pause lease it must wait — like every DeliverPrompt path — instead
// of landing in the pane the user is typing into. The deferral must also not burn
// an attempt, so the resume fires on the first tick after the lease is released.
func TestResumeLimitedSessions_DefersWhileAttached(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, repoID, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	advance(time.Hour + limitResumeGrace + time.Second)

	manager.PauseStatusPollFor(repoID, inst.Title, inst.ID, "")
	if !manager.isPollPaused(repoID, inst.Title, inst.ID) {
		t.Fatal("precondition: the attach pause lease must be held")
	}

	manager.ResumeLimitedSessions()
	if _, respawns, prompts := backend.snapshot(); len(prompts) != 0 || respawns != 0 {
		t.Fatalf("auto-resume delivered while the attach pause lease was held: prompts=%v respawns=%d", prompts, respawns)
	}
	if !inst.LimitReached() {
		t.Fatal("a deferred resume must leave the session parked at the limit")
	}

	// Detach: the very next tick resumes (the deferral recorded no attempt/backoff).
	manager.ResumeStatusPollFor(repoID, inst.Title, inst.ID, "")
	manager.ResumeLimitedSessions()
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 || prompts[0] != "finish the migration" {
		t.Fatalf("after detach the resume must fire on the next tick, prompts=%v", prompts)
	}
}
