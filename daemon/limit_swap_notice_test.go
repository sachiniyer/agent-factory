package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// TestResumeLimitedSessions_AutomaticSwapNoticeNotDeliveredRetainsObligation is
// the #6767 regression: an automatic account swap whose notice submission
// returns PromptNotDelivered — WITHOUT a transport error — must not retire the
// pending swap, clear the limit, or log the move as completed. Positive
// non-delivery is the one verdict that authorizes automatic redelivery, so the
// next due pass resends the same notice and only then settles.
func TestResumeLimitedSessions_AutomaticSwapNoticeNotDeliveredRetainsObligation(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	infoLog := captureInfoLog(t)
	backend.sendStatus = session.PromptNotDelivered

	advance(time.Second)
	manager.ResumeLimitedSessions()

	_, respawns, prompts := backend.snapshot()
	if respawns != 1 || len(prompts) != 1 {
		t.Fatalf("account replacement attempted %d respawns, %d prompts; want 1 each", respawns, len(prompts))
	}
	from, to, pending := inst.PendingAccountSwap()
	if !pending || to != "work" {
		t.Fatalf("a notice reported not-delivered retired its obligation: pending=%v from=%q to=%q", pending, from, to)
	}
	if !inst.LimitReached() {
		t.Fatal("a notice reported not-delivered cleared the limit wall")
	}
	data := inst.ToInstanceData()
	if data.PendingAccountSwap == nil || data.PendingAccountSwap.MissionDeliveryStatus != session.PromptNotDelivered {
		t.Fatalf("the send's exact verdict must be durable on the transaction, got %+v", data.PendingAccountSwap)
	}
	if data.PendingAccountSwap.MissionDeliveryAttemptAt.IsZero() {
		t.Fatal("the recorded verdict must bind the send's own attempt instant for re-verification")
	}
	if logged := infoLog.String(); strings.Contains(logged, "auto-resumed") {
		t.Fatalf("the daemon logged a completed move for a notice that never landed: %q", logged)
	}

	// Positive non-delivery belongs to the scheduler: the next due pass resends
	// the notice, and only the delivered verdict retires the obligation.
	backend.sendStatus = ""
	advance(time.Minute)
	manager.ResumeLimitedSessions()
	_, _, prompts = backend.snapshot()
	if len(prompts) != 2 {
		t.Fatalf("the due pass must resend the missed notice, prompts=%v", prompts)
	}
	if _, _, pending = inst.PendingAccountSwap(); pending {
		t.Fatal("the delivered resend retires the pending swap")
	}
	if inst.LimitReached() {
		t.Fatal("the delivered resend lifts the limit wall")
	}
}

// The ambiguous arm of #6767: a notice whose submission returned
// could-not-confirm — the literal-draft and lost-reply shapes — may already
// have landed, so it must never retire the obligation nor earn a scheduled
// resend. The row stays visibly pending for the re-verification watch and the
// operator exits.
func TestResumeLimitedSessions_AutomaticSwapNoticeCouldNotConfirmStaysPending(t *testing.T) {
	advance := withFrozenClock(t)
	base := nowFunc()
	manager, _, inst, backend := newAutoResumeManager(t, "", true, "finish the migration", base.Add(time.Hour))
	configureLimitAccountCandidate(t, manager, "work")
	infoLog := captureInfoLog(t)
	backend.sendStatus = session.PromptCouldNotConfirm

	advance(time.Second)
	manager.ResumeLimitedSessions()

	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("account replacement sent %d prompts, want 1", len(prompts))
	}
	if _, to, pending := inst.PendingAccountSwap(); !pending || to != "work" {
		t.Fatalf("an unconfirmed notice retired its obligation: to=%q pending=%v", to, pending)
	}
	if !inst.LimitReached() {
		t.Fatal("an unconfirmed notice cleared the limit wall")
	}
	data := inst.ToInstanceData()
	if data.PendingAccountSwap.MissionDeliveryStatus != session.PromptCouldNotConfirm {
		t.Fatalf("the ambiguous verdict must be durable on the transaction, got %q",
			data.PendingAccountSwap.MissionDeliveryStatus)
	}
	if logged := infoLog.String(); strings.Contains(logged, "auto-resumed") {
		t.Fatalf("the daemon logged a completed move for an unconfirmed notice: %q", logged)
	}

	// Ambiguity never authorizes automatic replay — no later pass may resend a
	// notice that may already have landed.
	for i := 0; i < 3; i++ {
		advance(time.Hour)
		manager.ResumeLimitedSessions()
	}
	if _, _, prompts := backend.snapshot(); len(prompts) != 1 {
		t.Fatalf("an ambiguous verdict must never earn a scheduled resend: prompts=%v", prompts)
	}
	if _, _, pending := inst.PendingAccountSwap(); !pending {
		t.Fatal("the obligation stays pending for evidence or the operator")
	}
}
