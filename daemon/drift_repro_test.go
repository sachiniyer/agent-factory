package daemon

import (
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestRefreshStatuses_SendPathParkedRow_TimeOnlyPhraseCanDriftResetByOneDay is
// the regression for the send-path evidence bug (#6765 post-merge): a send-path
// park that stores its reset WITHOUT the producing phrase leaves the evidence
// slot empty, so the daemon's first poll over the still-displayed banner fails
// the same-phrase dedupe and re-parses against a later clock. For a time-only
// phrase ("try again at 6:34 AM") whose first adopting poll lands AFTER the
// stated clock time, the re-parse resolves to the NEXT day — moving the stored
// reset by ~24h and postponing auto-resume.
//
// With the fix, the send-path park (limitParkError → LimitReachedError.Evidence
// → finishCreateStart → SetLimitReachedAtEvidence) records the phrase alongside
// its reset, so the daemon's first poll recognizes the re-read as the same
// observation and keeps the first parse's reset regardless of when the poll
// happens.
func TestRefreshStatuses_SendPathParkedRow_TimeOnlyPhraseCanDriftResetByOneDay(t *testing.T) {
	now := time.Date(2026, 11, 4, 6, 0, 0, 0, time.Local)
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: codexTimeOnlyLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Program = "codex"

	// The FIXED send-path park shape: a resolved reset WITH its producing
	// phrase. The real send path (limitParkError) now carries the evidence
	// through LimitReachedError.Evidence into SetLimitReachedAtEvidence.
	todayReset := time.Date(2026, 11, 4, 6, 34, 0, 0, time.Local).UTC()
	inst.SetLimitReachedAtEvidence(todayReset, "try again at 6:34 AM", session.LimitBanner{}, false)
	manager.persistInstance(repoID, inst)

	beforeReset, beforeOK := inst.LimitResetAt()
	if !beforeOK || !beforeReset.Equal(todayReset) {
		t.Fatalf("before poll: LimitResetAt=%v ok=%v, want %v", beforeReset, beforeOK, todayReset)
	}
	if got := inst.LimitResetEvidence(); got != "try again at 6:34 AM" {
		t.Fatalf("before poll: LimitResetEvidence=%q, want %q", got, "try again at 6:34 AM")
	}

	// Advance the clock PAST the stated reset time. A time-only phrase
	// re-parsed after 6:34 AM resolves to the NEXT day's 6:34 AM via
	// nextTimeOfDay — this is the drift window.
	now = time.Date(2026, 11, 4, 6, 40, 0, 0, time.Local)
	manager.RefreshStatuses()

	if !inst.LimitReached() {
		t.Fatal("a still-visible banner must keep the session parked")
	}
	gotReset, ok := inst.LimitResetAt()
	if !ok {
		t.Fatal("a still-visible banner must not drop its stored reset")
	}
	if !gotReset.Equal(todayReset) {
		t.Fatalf("after 6:40 poll: LimitResetAt=%v, want %v (the first parse's reset must not drift to tomorrow when the evidence is stored at park time)", gotReset, todayReset)
	}
	if got := inst.LimitResetEvidence(); got != "try again at 6:34 AM" {
		t.Fatalf("after 6:40 poll: LimitResetEvidence=%q, want %q", got, "try again at 6:34 AM")
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.Equal(todayReset) {
		t.Fatalf("persisted reset = %v, want %v (the non-drifted reset must be durable)", got, todayReset)
	}
	if got := persistedLimitEvidence(t, repoID, "limited"); got != "try again at 6:34 AM" {
		t.Fatalf("persisted evidence = %q, want %q", got, "try again at 6:34 AM")
	}

	// A subsequent poll keeps the same values — the evidence dedupe holds.
	now = time.Date(2026, 11, 4, 6, 41, 0, 0, time.Local)
	manager.RefreshStatuses()
	gotReset2, ok2 := inst.LimitResetAt()
	if !ok2 || !gotReset2.Equal(todayReset) {
		t.Fatalf("after 6:41 poll: LimitResetAt=%v ok=%v, want %v (a subsequent poll must keep the non-drifted reset)", gotReset2, ok2, todayReset)
	}
	if got := inst.LimitResetEvidence(); got != "try again at 6:34 AM" {
		t.Fatalf("after 6:41 poll: LimitResetEvidence=%q, want %q", got, "try again at 6:34 AM")
	}
}

// TestRefreshStatuses_SendPathParkedRow_NoEvidenceStillDrifts documents the
// pre-fix shape: a park that stores a reset WITHOUT its phrase (the old
// SetLimitReached path, or any caller that resolves a reset without parsing a
// banner) still drifts when the first poll is after the stated time. This is
// the behavior the fix moves OFF the send path: callers that DO parse a banner
// now use SetLimitReachedAtEvidence, so the drift no longer reaches them. A
// caller that genuinely has no phrase (a no-reset banner, or a display-only
// mirror) still passes "" and the poll adopts the phrase on the first
// re-read — which is correct for those paths, since they have no first parse to
// keep.
func TestRefreshStatuses_SendPathParkedRow_NoEvidenceStillDrifts(t *testing.T) {
	now := time.Date(2026, 11, 4, 6, 0, 0, 0, time.Local)
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: codexTimeOnlyLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Program = "codex"

	// The pre-fix send-path park shape: a resolved reset with NO phrase.
	todayReset := time.Date(2026, 11, 4, 6, 34, 0, 0, time.Local).UTC()
	inst.SetLimitReached(todayReset)
	manager.persistInstance(repoID, inst)

	// Advance past the stated time and poll: the empty evidence slot lets the
	// poll re-parse against the later clock, moving the reset to tomorrow.
	now = time.Date(2026, 11, 4, 6, 40, 0, 0, time.Local)
	manager.RefreshStatuses()

	gotReset, ok := inst.LimitResetAt()
	if !ok {
		t.Fatal("a still-visible banner must not drop its stored reset")
	}
	tomorrowReset := time.Date(2026, 11, 5, 6, 34, 0, 0, time.Local).UTC()
	if !gotReset.Equal(tomorrowReset) {
		t.Fatalf("after 6:40 poll with no stored evidence: LimitResetAt=%v, want %v (a park without evidence drifts to the next day — this is the behavior the fix moves off the send path)", gotReset, tomorrowReset)
	}
	if got := inst.LimitResetEvidence(); got != "try again at 6:34 AM" {
		t.Fatalf("after 6:40 poll: LimitResetEvidence=%q, want the adopted phrase", got)
	}
}
