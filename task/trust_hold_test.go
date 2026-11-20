package task

import (
	"context"
	"strings"
	"testing"
	"time"
)

// heldTrustTarget is a pane whose trust gate holds a dialog on every check.
// held is what its gate reports once the hold has settled; empty means the
// target reports none.
type heldTrustTarget struct {
	held   string
	checks int
}

func (t *heldTrustTarget) ResolvedAgent() string                          { return "claude" }
func (t *heldTrustTarget) PreviewContent(context.Context) (string, error) { return "❯", nil }
func (t *heldTrustTarget) HooksDone() <-chan struct{}                     { return nil }
func (t *heldTrustTarget) CheckAndHandleTrustPrompt() bool                { t.checks++; return true }
func (t *heldTrustTarget) TrustPromptHold() string                        { return t.held }

// plainTrustTarget is the same pane behind a target that cannot report a hold.
type plainTrustTarget struct{ inner *heldTrustTarget }

func (t plainTrustTarget) ResolvedAgent() string { return t.inner.ResolvedAgent() }
func (t plainTrustTarget) PreviewContent(ctx context.Context) (string, error) {
	return t.inner.PreviewContent(ctx)
}
func (t plainTrustTarget) HooksDone() <-chan struct{} { return nil }
func (t plainTrustTarget) CheckAndHandleTrustPrompt() bool {
	return t.inner.CheckAndHandleTrustPrompt()
}

func compressTrustTiming(t *testing.T) {
	t.Helper()
	oldDelay, oldPoll := trustPromptRetryDelay, waitForReadyPollInterval
	trustPromptRetryDelay, waitForReadyPollInterval = 0, time.Nanosecond
	t.Cleanup(func() { trustPromptRetryDelay, waitForReadyPollInterval = oldDelay, oldPoll })
}

// #7635: a create that spends its trust-prompt budget on a dialog the gate
// holds says what af was waiting on. The budget and the failure are master's;
// only the error's text changes. The pane is torn down on this error, so it
// says to retry wider and never to answer a dialog that is gone.
func TestIssue7635SpentTrustBudgetNamesTheHeldDialog(t *testing.T) {
	compressTrustTiming(t)
	target := &heldTrustTarget{held: "a folder-trust dialog it can't read (pane 60×12)"}

	err := DismissTrustPrompt(context.Background(), target)
	if err == nil {
		t.Fatal("a dialog held on every check must still fail the dismissal")
	}
	if target.checks != MaxTrustPromptAttempts {
		t.Fatalf("checks = %d, want the unchanged budget of %d", target.checks, MaxTrustPromptAttempts)
	}
	const want = "trust prompt did not dismiss after 20 attempts: af was waiting on a folder-trust dialog it can't read (pane 60×12) — widen the pane or window and retry"
	if err.Error() != want {
		t.Fatalf("error = %q\n want   %q", err, want)
	}
	if strings.Contains(err.Error(), "agent tab") {
		t.Fatalf("a failed create has no pane left to answer in: %q", err)
	}
}

// With no settled hold to report — the gate reports none, or the target cannot
// report one — the error is exactly master's.
func TestIssue7635SpentTrustBudgetWithoutAHoldKeepsItsMessage(t *testing.T) {
	compressTrustTiming(t)
	const want = "trust prompt did not dismiss after 20 attempts"

	if err := DismissTrustPrompt(context.Background(), &heldTrustTarget{}); err == nil || err.Error() != want {
		t.Fatalf("no settled hold: error = %v, want %q", err, want)
	}
	if err := DismissTrustPrompt(context.Background(), plainTrustTarget{inner: &heldTrustTarget{held: "ignored"}}); err == nil || err.Error() != want {
		t.Fatalf("a target that cannot report a hold: error = %v, want %q", err, want)
	}
}
