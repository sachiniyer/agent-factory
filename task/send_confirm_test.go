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
	c := BeginSendConfirmation(ctx, pane, time.Now().Add(-age))
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
		busy := "• Working (esc to interrupt)\n› \n"
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
