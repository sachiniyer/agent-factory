package api

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// virtualClock is a deterministic clock for the watch loop: sleep() advances a
// monotonic virtual now, so timeout behavior is exact and tests never touch the
// wall clock.
type virtualClock struct{ now time.Time }

func newVirtualClock() *virtualClock          { return &virtualClock{now: time.Unix(0, 0)} }
func (c *virtualClock) Now() time.Time        { return c.now }
func (c *virtualClock) Sleep(d time.Duration) { c.now = c.now.Add(d) }

// seqGetter returns each supplied snapshot in turn, repeating the last one
// forever once the sequence is exhausted (so a pending-forever session drives
// the timeout path). A nil entry means "return errTitleNotFound".
func seqGetter(seq []*session.InstanceData) func(string) (*session.InstanceData, error) {
	i := 0
	return func(string) (*session.InstanceData, error) {
		var d *session.InstanceData
		if i < len(seq) {
			d = seq[i]
			i++
		} else if len(seq) > 0 {
			d = seq[len(seq)-1]
		}
		if d == nil {
			return nil, fmt.Errorf("instance %q %w", "x", errTitleNotFound)
		}
		return d, nil
	}
}

func running() *session.InstanceData {
	return &session.InstanceData{Title: "x", Liveness: session.LiveRunning, Status: session.Running}
}
func ready() *session.InstanceData {
	return &session.InstanceData{Title: "x", Liveness: session.LiveReady, Status: session.Ready}
}

func TestClassifyWatch(t *testing.T) {
	cases := []struct {
		name    string
		data    *session.InstanceData
		want    watchOutcome
		wantSub string // substring the reason must contain (terminal cases)
	}{
		{"ready", ready(), watchReady, "ready for review"},
		{"running", running(), watchPending, ""},
		{"limit-reached", &session.InstanceData{Liveness: session.LiveLimitReached}, watchPending, ""},
		{"lost", &session.InstanceData{Liveness: session.LiveLost}, watchTerminal, "lost"},
		{"dead", &session.InstanceData{Liveness: session.LiveDead}, watchTerminal, "dead"},
		{"archived", &session.InstanceData{Liveness: session.LiveArchived}, watchTerminal, "archived"},
		// An in-flight op overlays the liveness: still settling, keep polling even
		// though the underlying liveness (Ready) would otherwise read as done.
		{"archiving-op", &session.InstanceData{Liveness: session.LiveReady, InFlightOp: session.OpArchiving}, watchPending, ""},
		{"creating-op", &session.InstanceData{Liveness: session.LivenessUnset, InFlightOp: session.OpCreating}, watchPending, ""},
		// Pre-#1195 record: no liveness axis, classify from the legacy Status.
		{"legacy-ready", &session.InstanceData{Liveness: session.LivenessUnset, Status: session.Ready}, watchReady, "ready for review"},
		{"legacy-running", &session.InstanceData{Liveness: session.LivenessUnset, Status: session.Running}, watchPending, ""},
		{"legacy-lost", &session.InstanceData{Liveness: session.LivenessUnset, Status: session.Lost}, watchTerminal, "lost"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, reason := classifyWatch(tc.data)
			if got != tc.want {
				t.Fatalf("classifyWatch = %v, want %v", got, tc.want)
			}
			if tc.wantSub != "" && !strings.Contains(reason, tc.wantSub) {
				t.Fatalf("reason %q does not contain %q", reason, tc.wantSub)
			}
		})
	}
}

// TestWatchForReady_GreenTransition: a session that works for several polls then
// goes idle exits 0 and returns the final ready snapshot.
func TestWatchForReady_GreenTransition(t *testing.T) {
	clk := newVirtualClock()
	seq := []*session.InstanceData{running(), running(), running(), ready()}
	got, err := watchForReady(watchDeps{
		get:      seqGetter(seq),
		interval: 2 * time.Second,
		timeout:  30 * time.Minute,
		now:      clk.Now,
		sleep:    clk.Sleep,
	}, "x")
	if err != nil {
		t.Fatalf("expected nil error on green transition, got: %v", err)
	}
	if got == nil || got.Liveness != session.LiveReady {
		t.Fatalf("expected final ready snapshot, got %+v", got)
	}
}

// TestWatchForReady_Terminal: lost/dead/archived each stop the watch immediately
// with a non-nil error naming the reason.
func TestWatchForReady_Terminal(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lv      session.Liveness
		wantSub string
	}{
		{"lost", session.LiveLost, "lost"},
		{"dead", session.LiveDead, "dead"},
		{"archived", session.LiveArchived, "archived"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clk := newVirtualClock()
			seq := []*session.InstanceData{running(), {Title: "x", Liveness: tc.lv}}
			_, err := watchForReady(watchDeps{
				get:      seqGetter(seq),
				interval: 2 * time.Second,
				timeout:  30 * time.Minute,
				now:      clk.Now,
				sleep:    clk.Sleep,
			}, "x")
			if err == nil {
				t.Fatalf("expected non-nil error for terminal %s", tc.name)
			}
			if !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err.Error(), tc.wantSub)
			}
		})
	}
}

// TestWatchForReady_Timeout: a session that never leaves Running exits non-zero
// once the window elapses, and does so in exactly timeout/interval polls (the
// virtual clock makes this exact).
func TestWatchForReady_Timeout(t *testing.T) {
	clk := newVirtualClock()
	polls := 0
	get := func(string) (*session.InstanceData, error) {
		polls++
		return running(), nil
	}
	_, err := watchForReady(watchDeps{
		get:      get,
		interval: 2 * time.Second,
		timeout:  10 * time.Second,
		now:      clk.Now,
		sleep:    clk.Sleep,
	}, "x")
	if err == nil {
		t.Fatalf("expected timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), "working") {
		t.Fatalf("expected a timeout-while-working error, got: %v", err)
	}
	// Polls at t=0,2,4,6,8,10s: the sixth poll sees elapsed==timeout and exits.
	if polls != 6 {
		t.Fatalf("expected 6 polls for a 10s/2s window, got %d", polls)
	}
}

// TestWatchForReady_UnknownTitle: a title that never resolves surfaces the plain
// not-found sentinel (never the "disappeared" variant).
func TestWatchForReady_UnknownTitle(t *testing.T) {
	clk := newVirtualClock()
	_, err := watchForReady(watchDeps{
		get:      seqGetter([]*session.InstanceData{nil}),
		interval: 2 * time.Second,
		timeout:  30 * time.Minute,
		now:      clk.Now,
		sleep:    clk.Sleep,
	}, "x")
	if err == nil {
		t.Fatalf("expected not-found error for unknown title")
	}
	if !errors.Is(err, errTitleNotFound) {
		t.Fatalf("expected errTitleNotFound sentinel, got: %v", err)
	}
	if strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("unknown title should not report 'disappeared', got: %v", err)
	}
}

// TestWatchForReady_DisappearsMidWatch: a session that existed and then vanishes
// (killed) reports a distinct "disappeared" error, not a bare not-found.
func TestWatchForReady_DisappearsMidWatch(t *testing.T) {
	clk := newVirtualClock()
	_, err := watchForReady(watchDeps{
		get:      seqGetter([]*session.InstanceData{running(), running(), nil}),
		interval: 2 * time.Second,
		timeout:  30 * time.Minute,
		now:      clk.Now,
		sleep:    clk.Sleep,
	}, "x")
	if err == nil {
		t.Fatalf("expected error when session disappears mid-watch")
	}
	if !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("expected a 'disappeared' error, got: %v", err)
	}
}
