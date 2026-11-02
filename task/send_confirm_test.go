package task

import (
	"context"
	"sync"
	"testing"
	"time"
)

// scriptedPane serves a fixed sequence of captures, repeating the last one.
type scriptedPane struct {
	mu     sync.Mutex
	agent  string
	frames []string
	n      int
}

func (p *scriptedPane) ResolvedAgent() string      { return p.agent }
func (p *scriptedPane) HooksDone() <-chan struct{} { return nil }
func (p *scriptedPane) PreviewContent(context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	f := p.frames[min(p.n, len(p.frames)-1)]
	p.n++
	return f, nil
}

func compressConfirmWindow(t *testing.T) {
	t.Helper()
	b, p := sendConfirmBudget, sendConfirmPoll
	sendConfirmBudget, sendConfirmPoll = 400*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { sendConfirmBudget, sendConfirmPoll = b, p })
}

func confirms(t *testing.T, pane *scriptedPane, age time.Duration) bool {
	t.Helper()
	compressConfirmWindow(t)
	ctx := context.Background()
	c := BeginSendConfirmation(ctx, pane)
	c.BindAttempt(time.Now().Add(-age))
	return c.Confirm(ctx)
}

const (
	idleComposer = "❯ \n"
	stagedChip   = "❯ [Pasted text #1 +40 lines]\n"
)

func turnRow(sec string) string {
	return "✻ Working… (" + sec + " · esc to interrupt)\n❯ \n"
}

// #6474 case 1: a landed prompt is promoted — the agent's turn began after our
// send, shown by its status row ticking.
func TestSendConfirmation_LandedPromptIsPromoted(t *testing.T) {
	pane := &scriptedPane{agent: "claude", frames: []string{
		idleComposer, turnRow("1s"), turnRow("2s"), turnRow("3s"),
	}}
	if !confirms(t, pane, 30*time.Second) {
		t.Fatal("a turn that began after the send must promote sent-unverified")
	}
}

// Case 2: the prompt still sitting in the composer is not received.
func TestSendConfirmation_PromptStillInComposerStaysUnconfirmed(t *testing.T) {
	pane := &scriptedPane{agent: "claude", frames: []string{idleComposer, stagedChip}}
	if confirms(t, pane, 30*time.Second) {
		t.Fatal("a prompt staged in the composer is not evidence of receipt")
	}
}

// Case 3: a composer cleared without a submission — Esc, a user, a dismissed
// picker — is the empty composer the evidence rule excludes.
func TestSendConfirmation_ComposerClearedWithoutSubmitStaysUnconfirmed(t *testing.T) {
	pane := &scriptedPane{agent: "claude", frames: []string{stagedChip, idleComposer}}
	if confirms(t, pane, 30*time.Second) {
		t.Fatal("an emptied composer alone is not evidence the prompt was submitted")
	}
}

// Case 4: an agent that was already busy before our Enter proves nothing.
func TestSendConfirmation_AgentAlreadyBusyIsNotEvidence(t *testing.T) {
	t.Run("busy pane at send time", func(t *testing.T) {
		pane := &scriptedPane{agent: "claude", frames: []string{
			turnRow("40s"), turnRow("41s"), turnRow("42s"),
		}}
		if confirms(t, pane, 30*time.Second) {
			t.Fatal("a turn already running when the send began is not this prompt's")
		}
	})
	t.Run("turn older than the send surfaces later", func(t *testing.T) {
		// Idle pre-send frame, then a ticking row whose own clock says it began
		// long before the send.
		pane := &scriptedPane{agent: "claude", frames: []string{
			idleComposer, turnRow("5m 1s"), turnRow("5m 2s"), turnRow("5m 3s"),
		}}
		if confirms(t, pane, 10*time.Second) {
			t.Fatal("a ticking row older than the send belongs to earlier work")
		}
	})
	t.Run("codex already working", func(t *testing.T) {
		busy := "› \n  esc to interrupt\n"
		pane := &scriptedPane{agent: "codex", frames: []string{busy, busy}}
		if confirms(t, pane, 30*time.Second) {
			t.Fatal("codex chrome present before the send is not this prompt's")
		}
	})
}

// A pane that cannot be read, or an agent with no in-turn signature, never
// promotes.
func TestSendConfirmation_UnsignaturedAgentNeverPromotes(t *testing.T) {
	pane := &scriptedPane{agent: "aider", frames: []string{idleComposer, turnRow("1s"), turnRow("2s")}}
	if confirms(t, pane, 30*time.Second) {
		t.Fatal("an agent without an in-turn signature has no bound evidence")
	}
}

// A clockless agent's watch is armed by the pre-send frame, so that frame must
// sit at the send's attempt instant: codex promotes when it does, and not when
// a delayed capture leaves a gap an earlier pending turn could have started in.
func TestSendConfirmation_ClocklessSeedMustBeAtTheAttemptInstant(t *testing.T) {
	busy := "› \n  esc to interrupt\n"
	frames := []string{"› \n", busy, busy}
	if !confirms(t, &scriptedPane{agent: "codex", frames: frames}, 0) {
		t.Fatal("codex chrome appearing after an at-boundary idle frame must promote")
	}
	compressConfirmWindow(t)
	c := BeginSendConfirmation(context.Background(), &scriptedPane{agent: "codex", frames: frames})
	c.BindAttempt(time.Now().Add(30 * time.Second)) // the attempt lands long after the idle frame
	if c.Confirm(context.Background()) {
		t.Fatal("an idle frame far older than the attempt cannot bound a clockless turn's start")
	}
}

// #6590: the daemon keeps re-verifying a sent-unverified task send past the
// bounded Confirm window, fed by the status poll's pane captures. A pane
// already mid-turn at the send therefore still yields a bound watch — the
// bounded Confirm stays ineligible (inside its window the running turn cannot
// be attributed to this send), but the watch outlives it: the pre-existing
// turn's own clock is older than the send and can never fire it, while a NEW
// turn — the queued prompt the agent takes once the current turn ends —
// satisfies the same evidence rule.
func TestSendConfirmation_BusySeedKeepsReverifyWatch(t *testing.T) {
	sentAt := time.Now()
	pane := &scriptedPane{agent: "claude", frames: []string{turnRow("40s")}}
	c := BeginSendConfirmation(context.Background(), pane)
	c.BindAttempt(sentAt)
	w := c.ReverifyWatch()
	if w == nil {
		t.Fatal("a busy pre-send pane must still yield the bound chrome watch for re-verification")
	}
	if c.Confirm(context.Background()) {
		t.Fatal("the bounded Confirm must stay ineligible on a busy pre-send pane")
	}
	// The pre-existing turn keeps ticking: its own clock outdates the send, so
	// it can never fire the watch.
	for i, tick := range []string{"41s", "42s", "43s"} {
		if w.Observe(turnRow(tick), sentAt.Add(time.Duration(i+1)*time.Second)) {
			t.Fatal("a turn older than the send must never fire its re-verification watch")
		}
	}
	// The turn ends and a new one begins — the queued draft's work. Its own
	// clock fits inside the send's age, so the same evidence rule fires.
	if w.Observe(idleComposer, sentAt.Add(4*time.Second)) {
		t.Fatal("an idle frame is not evidence")
	}
	w.Observe(turnRow("1s"), sentAt.Add(5*time.Second))
	if !w.Observe(turnRow("2s"), sentAt.Add(6*time.Second)) {
		t.Fatal("a turn beginning after the send must fire the re-verification watch")
	}
}

// A delivery whose prompt the agent never took is never promoted: the draft
// stays staged, no turn begins, and the watch outlives its window without
// firing.
func TestSendConfirmation_NeverTakenNeverFires(t *testing.T) {
	sentAt := time.Now()
	pane := &scriptedPane{agent: "claude", frames: []string{idleComposer}}
	c := BeginSendConfirmation(context.Background(), pane)
	c.BindAttempt(sentAt)
	w := c.ReverifyWatch()
	if w == nil {
		t.Fatal("a signatured agent's send must yield the bound watch")
	}
	for i := 0; i < 10; i++ {
		if w.Observe(stagedChip, sentAt.Add(time.Duration(i+1)*time.Second)) {
			t.Fatal("a staged draft that never submits must never promote")
		}
	}
}

// An agent with no in-turn signature has nothing the feed could prove — no
// watch exists to feed.
func TestSendConfirmation_UnsignaturedAgentHasNoReverifyWatch(t *testing.T) {
	c := BeginSendConfirmation(context.Background(), &scriptedPane{agent: "aider", frames: []string{idleComposer}})
	if c.ReverifyWatch() != nil {
		t.Fatal("an unsignatured agent must not yield a re-verification watch")
	}
}
