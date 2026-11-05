package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// limitBannerBackend is a FakeBackend whose pane capture returns a fixed content
// string every idle tick (updated=false, hasPrompt=false) — the shape of a
// claude/codex session stalled at a usage-limit wall (#1146). IsAlive inherits
// FakeBackend's true, so the daemon's idle branch runs the limit detector over
// the content rather than probing the session Lost.
type limitBannerBackend struct {
	*session.FakeBackend
	content string
}

func (b limitBannerBackend) HasUpdated(*session.Instance) (bool, bool, string) {
	return false, false, b.content
}

// persistedLiveness reads a session's persisted liveness from the on-disk store,
// so a limit transition (which composes to a Ready Status) can be asserted on the
// axis the daemon actually writes.
func persistedLiveness(t *testing.T, repoID, title string) session.Liveness {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	for _, d := range data {
		if d.Title == title {
			return d.Liveness
		}
	}
	t.Fatalf("instance %q not found in persisted store", title)
	return session.LivenessUnset
}

// persistedLimitReset reads a session's persisted usage-limit reset time from the
// on-disk store, so the reset-time-persist path (#1146/#1204) can be asserted on
// disk rather than only in memory.
func persistedLimitReset(t *testing.T, repoID, title string) time.Time {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	for _, d := range data {
		if d.Title == title {
			return d.LimitResetAt
		}
	}
	t.Fatalf("instance %q not found in persisted store", title)
	return time.Time{}
}

// persistedLimitEvidence reads a session's persisted reset phrase from the
// on-disk store (#6765) — the value whose survival the post-merge persist-gap
// fix makes durable.
func persistedLimitEvidence(t *testing.T, repoID, title string) string {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	for _, d := range data {
		if d.Title == title {
			return d.LimitResetEvidence
		}
	}
	t.Fatalf("instance %q not found in persisted store", title)
	return ""
}

const claudeLimitBanner = "Claude usage limit reached. Your limit will reset at 2pm (America/New_York)"

// TestPersistPollChange_LaterResetTime_PersistsIndependentOfLiveness is the
// #1204 reset-time-persist regression: a row can enter LiveLimitReached on one
// tick with no parsed reset time and only parse it on a LATER tick. That later
// tick leaves the liveness unchanged, so a liveness-only persist gate would
// silently drop the reset time — the [limit] resets <t> badge would never show
// it and PR3's auto-resume scheduler would have nothing to schedule against after
// a daemon restart. persistPollChange must flush on a reset-time change even when
// the liveness is unchanged. Uses fixed clock values, no wall-clock.
func TestPersistPollChange_LaterResetTime_PersistsIndependentOfLiveness(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)

	// Tick 1: the banner matched but carried no parseable reset time yet, so the
	// row enters LimitReached with a zero reset time. This is a real liveness
	// transition and persists.
	before1 := inst.GetLiveness()
	beforeReset1, _ := inst.LimitResetAt()
	inst.SetLimitReached(time.Time{})
	manager.persistPollChange(repoID, inst, before1, beforeReset1, false)

	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveLimitReached {
		t.Fatalf("after tick 1 persisted liveness = %v, want LiveLimitReached", got)
	}
	if got := persistedLimitReset(t, repoID, "limited"); !got.IsZero() {
		t.Fatalf("after tick 1 persisted reset = %v, want zero (none parsed yet)", got)
	}

	// Tick 2: a later poll finally parses the reset time. The liveness is UNCHANGED
	// (already LimitReached), so a liveness-only gate would drop this write — the
	// regression. persistPollChange must still flush the reset time to disk.
	resetAt := time.Date(2026, 7, 6, 15, 0, 0, 0, time.UTC)
	before2 := inst.GetLiveness()
	beforeReset2, _ := inst.LimitResetAt()
	inst.SetLimitReached(resetAt)
	manager.persistPollChange(repoID, inst, before2, beforeReset2, false)

	got := persistedLimitReset(t, repoID, "limited")
	if !got.Equal(resetAt) {
		t.Fatalf("persisted reset time = %v, want %v (a later-parsed reset time must persist even when liveness is unchanged, #1204)", got, resetAt)
	}
}

// TestRefreshStatuses_LimitBannerBecomesLimitReached: an idle claude session
// whose pane shows a usage-limit banner is marked LimitReached (not Ready), the
// parsed reset time is stored, and the daemon persists the liveness transition
// (#1146). This is the core PR2 detection-wiring guarantee.
func TestRefreshStatuses_LimitBannerBecomesLimitReached(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	// Start from Running to prove the pass actively transitions it.
	registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "limited")]
	if !inst.LimitReached() {
		t.Fatalf("in-memory liveness = %v, want LimitReached (a limit banner must never resolve to Ready)", inst.GetLiveness())
	}
	if _, ok := inst.LimitResetAt(); !ok {
		t.Fatal("a parseable reset time must be stored for the badge")
	}
	if got := persistedLiveness(t, repoID, "limited"); got != session.LiveLimitReached {
		t.Fatalf("persisted liveness = %v, want LiveLimitReached (the transition must persist)", got)
	}
}

// codexRelativeLimitBanner is the relative-countdown form Codex renders
// (openai/codex#3031). It is the sharpest repeated-read case: every re-parse
// resolves "in N" against a later now, so an unchanged banner moves its reset
// on every poll when the poll rewrites the stored value (#6765).
const codexRelativeLimitBanner = "■ You've hit your usage limit, try again in 4 days 2 hours 46 minutes."

// codexTimeOnlyLimitBanner is Codex's 5h-window form: a clock time with no
// date. Re-reading it after the stated time mints the NEXT day's occurrence —
// a stale banner on screen postpones auto-resume by a day, not by a tick.
const codexTimeOnlyLimitBanner = "■ You've hit your usage limit. Please try again at 6:34 AM."

// TestRefreshStatuses_UnchangedBannerKeepsReset is the #6765 regression:
// several polls over an unchanged banner must not move its stored reset. The
// relative form drifts every poll; the time-only form jumps a full day once
// the stated clock time passes. Re-reading evidence already recorded is not a
// new observation — the first parse of a phrase is the one nearest the wall
// the provider printed.
func TestRefreshStatuses_UnchangedBannerKeepsReset(t *testing.T) {
	base := time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)
	now := base
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	t.Run("relative countdown does not drift across polls", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: codexRelativeLimitBanner}
		inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
		inst.Program = "codex"

		manager.RefreshStatuses()
		first, ok := inst.LimitResetAt()
		if !ok {
			t.Fatal("a relative countdown banner must park with a parsed reset")
		}
		for step := 1; step <= 3; step++ {
			now = base.Add(time.Duration(step) * time.Minute)
			// Defends the red run too: without the fix the poll reads the
			// real clock, and a slept gap still moves the reset.
			time.Sleep(5 * time.Millisecond)
			manager.RefreshStatuses()
			got, ok := inst.LimitResetAt()
			if !ok {
				t.Fatalf("poll %d dropped the stored reset", step+1)
			}
			if !got.Equal(first) {
				t.Fatalf("poll %d moved the reset from %v to %v: re-reading an unchanged banner is not a new observation", step+1, first, got)
			}
		}
	})

	t.Run("time-only reset does not jump to tomorrow past the stated time", func(t *testing.T) {
		manager, repoID, repoPath := newStatusTestManager(t)
		backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: codexTimeOnlyLimitBanner}
		inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
		inst.Program = "codex"

		now = time.Date(2026, 11, 4, 6, 0, 0, 0, time.Local)
		manager.RefreshStatuses()
		first, ok := inst.LimitResetAt()
		want := time.Date(2026, 11, 4, 6, 34, 0, 0, time.Local).UTC()
		if !ok || !first.Equal(want) {
			t.Fatalf("the 6:34 AM banner at 6:00 must park for today 6:34 (%v), got %v ok=%v", want, first, ok)
		}

		// The stated time passes while the banner is still on screen: the pane
		// is unchanged, so the stored reset must not become tomorrow's.
		now = time.Date(2026, 11, 4, 6, 40, 0, 0, time.Local)
		manager.RefreshStatuses()
		got, ok := inst.LimitResetAt()
		if !ok {
			t.Fatal("a still-visible banner must not drop its stored reset")
		}
		if !got.Equal(first) {
			t.Fatalf("polling past 6:34 moved the reset from %v to %v: the unchanged banner is the same evidence", first, got)
		}
	})
}

// TestRefreshStatuses_SendPathParkedRowAdoptsPhraseDurably is the post-merge
// Codex finding on #6765's fix (PR #6788): a send-path park stores a reset with
// NO phrase, so the first poll that re-reads the banner adopts the phrase while
// changing neither liveness nor reset time. A persist gate watching only those
// two drops the adoption — and after a restart the blank evidence slot lets the
// same banner re-parse against a later clock, moving the reset to tomorrow.
func TestRefreshStatuses_SendPathParkedRowAdoptsPhraseDurably(t *testing.T) {
	now := time.Date(2026, 11, 4, 6, 0, 0, 0, time.Local)
	prevNow := nowFunc
	nowFunc = func() time.Time { return now }
	t.Cleanup(func() { nowFunc = prevNow })

	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: codexTimeOnlyLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "limited", backend, true, session.Running)
	inst.Program = "codex"

	// The send-path park shape: a resolved reset with no producing phrase,
	// already durable on disk.
	inst.SetLimitReached(time.Date(2026, 11, 4, 6, 34, 0, 0, time.Local).UTC())
	manager.persistInstance(repoID, inst)
	if got := persistedLimitEvidence(t, repoID, "limited"); got != "" {
		t.Fatalf("seed row must carry no evidence, got %q", got)
	}

	// The poll re-reads the same banner: the reset parses to the SAME instant
	// (6:00 is before 6:34, so the next occurrence is still today's), so
	// liveness and LimitResetAt are both unchanged — only the phrase is new.
	manager.RefreshStatuses()
	if !inst.LimitReached() {
		t.Fatal("a still-visible banner must keep the session parked")
	}
	if got := inst.LimitResetEvidence(); got != "try again at 6:34 AM" {
		t.Fatalf("in-memory evidence = %q, want the banner's phrase adopted", got)
	}
	if got := persistedLimitEvidence(t, repoID, "limited"); got != "try again at 6:34 AM" {
		t.Fatalf("persisted evidence = %q, want the adopted phrase durable — a restart must not re-parse the same banner against a later clock", got)
	}
}

// TestRefreshStatuses_LimitClearsWhenBannerGone: a session that recovers on its
// own — the banner scrolls away and the pane goes idle-clean — leaves the limit
// state and resolves back to Ready.
func TestRefreshStatuses_LimitClearsWhenBannerGone(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: claudeLimitBanner}
	inst := registerStarted(t, manager, repoID, repoPath, "recovering", backend, true, session.Running)

	manager.RefreshStatuses()
	if !inst.LimitReached() {
		t.Fatalf("expected LimitReached after the banner tick, got %v", inst.GetLiveness())
	}

	// The banner is gone; the pane is idle-clean now.
	backend.content = "$ "
	manager.RefreshStatuses()
	if inst.LimitReached() {
		t.Fatal("a session whose banner cleared must leave the limit state")
	}
	if got := inst.GetStatus(); got != session.Ready {
		t.Fatalf("recovered session status = %v, want Ready", got)
	}
}

// TestRefreshStatuses_GeminiBannerNotLimited: gemini is API-key-metered, so even
// a rate-limit-looking pane never resolves to LimitReached in v1 — it settles
// Ready like any idle session.
func TestRefreshStatuses_GeminiBannerNotLimited(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := limitBannerBackend{FakeBackend: session.NewFakeBackend(), content: "429 RESOURCE_EXHAUSTED"}
	inst := registerStarted(t, manager, repoID, repoPath, "gemini-sess", backend, true, session.Running)
	// Point the resolved agent at gemini (registerStarted defaults Program to
	// claude); the instance has no tmux, so ResolvedAgent falls back to Program.
	inst.Program = "gemini"

	manager.RefreshStatuses()

	if inst.LimitReached() {
		t.Fatal("gemini must never resolve to LimitReached in v1")
	}
	if got := inst.GetStatus(); got != session.Ready {
		t.Fatalf("gemini idle status = %v, want Ready", got)
	}
}
