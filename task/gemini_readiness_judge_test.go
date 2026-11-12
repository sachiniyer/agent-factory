package task

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The fallback, for the one frame shape that cannot prove itself: with both
// anchors hidden, the composer is ready once one unbroken composer has been
// seen on two polls at least geminiBareComposerSettle apart. A frame in between
// that is not that composer starts the count again.
func TestReadinessJudge_GeminiBareComposerIsReadyOnlyOnTwoPollsHalfASecondApart(t *testing.T) {
	bare := geminiTrustCapture(t, "gemini_0_51_0_composer_banner_and_footer_hidden.ansi")
	dialog := geminiTrustCapture(t, "gemini_0_51_0_folder_trust.ansi")
	t0 := time.Unix(1_700_000_000, 0)

	type poll struct {
		at      time.Duration
		content string
		ready   bool
	}
	for _, tt := range []struct {
		name  string
		polls []poll
	}{
		{"two sightings at the margin", []poll{
			{0, bare, false},
			{geminiBareComposerSettle - time.Millisecond, bare, false},
			{geminiBareComposerSettle, bare, true},
		}},
		{"a frame in between restarts the count", []poll{
			{0, bare, false},
			{300 * time.Millisecond, "", false},
			{600 * time.Millisecond, bare, false},
			{600*time.Millisecond + geminiBareComposerSettle, bare, true},
		}},
		{"the dialog replacing the first paint is the dialog, not the composer", []poll{
			{0, geminiTrustCapture(t, "gemini_0_51_0_composer_first_paint.ansi"), false},
			{geminiBareComposerSettle, dialog, true},
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			now := t0
			judge := newReadinessJudge(tmux.ProgramGemini, readinessClock{now: func() time.Time { return now }})
			for i, p := range tt.polls {
				now = t0.Add(p.at)
				if got := judge.ready(p.content); got != p.ready {
					t.Fatalf("poll %d at +%s: ready = %t, want %t", i, p.at, got, p.ready)
				}
			}
		})
	}

	// Only Gemini gets the fallback: another agent's readiness is unchanged.
	judge := newReadinessJudge(tmux.ProgramClaude, readinessClock{now: func() time.Time { return t0 }})
	judge.ready(bare)
	judge.now = func() time.Time { return t0.Add(time.Minute) }
	if judge.ready(bare) {
		t.Fatal("a Gemini composer on a pane resolved to another agent must not become ready by waiting")
	}
}

// End to end: waitForReadyOn carries the judge across its polls, so a Gemini
// pane with both anchors hidden becomes ready on the poll that is the second
// sighting at least geminiBareComposerSettle after the first — not before.
func TestWaitForReadyOn_GeminiBareComposerIsReadyOnTheSecondSightingApart(t *testing.T) {
	bare := geminiTrustCapture(t, "gemini_0_51_0_composer_banner_and_footer_hidden.ansi")

	// Each capture sets the virtual clock to its own poll's time, so the judge
	// reads exactly that time: the first look, then two ticks half the margin
	// apart. Setting the clock from the test goroutine instead would race the
	// probe that is still judging the previous capture.
	pollAt := []time.Duration{0, geminiBareComposerSettle / 2, geminiBareComposerSettle}
	t0 := time.Unix(1_700_000_000, 0)
	var mu sync.Mutex
	now := t0
	var captures atomic.Int32
	inst := newPreviewInstanceWithProgram(t, "gemini", func() (string, error) {
		n := int(captures.Add(1))
		mu.Lock()
		now = t0.Add(pollAt[min(n, len(pollAt))-1])
		mu.Unlock()
		return bare, nil
	})

	observed := newObservedReadinessClock()
	clock := observed.clock()
	clock.now = func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}
	waitForCaptures := func(n int32) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for captures.Load() < n {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for capture %d", n)
			}
			time.Sleep(time.Millisecond)
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- waitForReadyOn(context.Background(), instanceReadinessTarget{inst: inst}, clock, false)
	}()
	receiveReadinessEvent(t, observed.timers, "initial readiness timer")
	receiveReadinessEvent(t, observed.tickerStarted, "readiness ticker")
	waitForCaptures(1) // the look before the first tick: first sighting

	observed.ticks <- time.Time{}
	waitForCaptures(2)
	select {
	case err := <-done:
		t.Fatalf("ready after %s, under the %s margin (err %v)", geminiBareComposerSettle/2, geminiBareComposerSettle, err)
	case <-time.After(50 * time.Millisecond):
	}

	observed.ticks <- time.Time{}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("expected ready on the sighting %s after the first, got %v", geminiBareComposerSettle, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waitForReadyOn did not become ready on the second sighting past the margin")
	}
	if got := captures.Load(); got != 3 {
		t.Fatalf("captures = %d, want 3 (the first look and two ticks)", got)
	}
}
