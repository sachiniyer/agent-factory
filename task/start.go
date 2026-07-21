package task

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// MaxTrustPromptAttempts and trustPromptRetryDelay are THE trust-prompt
// dismissal budget: ~20s, enough for a sequence of dialogs (folder trust, then
// MCP trust) plus the readiness re-wait each one costs.
//
// The budget is exported and the loop below is shared because copying the pair
// is how it broke: the daemon's config agent carried its own 5 × 500ms
// constants under a comment claiming they mirrored these, giving a config-agent
// spawn an eighth of a session's budget and failing it with "trust prompt did
// not dismiss" where a session would have succeeded (#2097). A second copy of
// the numbers cannot drift if there is no second copy — every caller goes
// through DismissTrustPrompt.
const MaxTrustPromptAttempts = 20

var trustPromptRetryDelay = time.Second

// ErrAgentReadiness marks failures that happen before the incoming agent has
// reached a usable composer. ErrPromptDelivery marks the narrower failure after
// readiness was established. Callers that have already crossed an irreversible
// runtime-swap boundary need this distinction: the former must become an inert
// startup-unknown record, while the latter can retain a delivery retry against
// the runtime whose readiness was positively established.
var (
	ErrAgentReadiness = errors.New("agent did not become ready")
	ErrPromptDelivery = errors.New("prompt delivery failed after readiness")
)

// SetTrustPromptTimingForTest compresses the trust-prompt dismissal loop's
// timing — both the backoff between attempts and the readiness poll each retry
// drives — so a test outside this package can exercise the full attempt budget
// without sleeping through ~20s of it. Returns a restore func, matching the
// session.SetBackendFactoryForTest seam. Test-only.
func SetTrustPromptTimingForTest(retryDelay time.Duration) func() {
	oldDelay, oldPoll := trustPromptRetryDelay, waitForReadyPollInterval
	poll := retryDelay
	if poll <= 0 {
		// time.NewTicker panics on a non-positive duration, so the poll floor is
		// the smallest tick rather than the caller's zero.
		poll = time.Nanosecond
	}
	trustPromptRetryDelay, waitForReadyPollInterval = retryDelay, poll
	return func() {
		trustPromptRetryDelay, waitForReadyPollInterval = oldDelay, oldPoll
	}
}

// TrustPromptTarget is a ReadinessTarget that can also dismiss its own first-run
// trust dialog — the narrow contract DismissTrustPrompt drives, implemented both
// by a full session.Instance and by the daemon's bare tmux config agent.
type TrustPromptTarget interface {
	ReadinessTarget
	// CheckAndHandleTrustPrompt dismisses a visible trust dialog and reports
	// whether one was there — i.e. whether another may follow it.
	CheckAndHandleTrustPrompt() bool
}

// DismissTrustPrompt clears an agent's first-run trust dialogs, bounded by the
// canonical budget above.
//
// Dialogs arrive one at a time (folder trust, then MCP trust), so each dismissal
// is followed by a fresh readiness wait before the next check; the loop ends
// when the pane shows no dialog. A nil ctx is treated as context.Background().
func DismissTrustPrompt(ctx context.Context, target TrustPromptTarget) error {
	if ctx == nil {
		ctx = context.Background()
	}
	for attempts := 0; target.CheckAndHandleTrustPrompt(); attempts++ {
		if attempts+1 >= MaxTrustPromptAttempts {
			return fmt.Errorf("trust prompt did not dismiss after %d attempts", MaxTrustPromptAttempts)
		}
		// Honor cancellation while backing off between trust-prompt retries so an
		// abandoned create doesn't sit here sleeping and re-waiting.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(trustPromptRetryDelay):
		}
		if err := WaitForReadyOn(ctx, target); err != nil {
			return err
		}
	}
	return nil
}

// instanceTrustTarget adapts a full session.Instance to TrustPromptTarget,
// reusing the readiness adapter so the trust loop polls the same way
// WaitForReady does.
type instanceTrustTarget struct{ instanceReadinessTarget }

func (t instanceTrustTarget) CheckAndHandleTrustPrompt() bool {
	return t.inst.CheckAndHandleTrustPrompt()
}

// StartAndSendPrompt is the canonical way to start an instance, wait for
// readiness, handle trust prompts, and optionally send a prompt.
//
// It always waits for the program to become ready. If prompt is non-empty,
// it sends the prompt via tmux send-keys after readiness.
//
// It does NOT set the instance status to Running — callers must do so when
// appropriate. For TUI async paths, the instanceStartedMsg handler sets
// Running after saving to disk; for synchronous API/runner paths, the caller
// sets Running immediately after this function returns.
//
// ctx bounds the readiness wait: an abandoned or cancelled create tears down the
// pane-poll instead of spinning to the timeout (see WaitForReady). A nil ctx is
// treated as context.Background().
func StartAndSendPrompt(ctx context.Context, instance *session.Instance, prompt string) error {
	_, err := StartAndSendPromptWithConversationCapture(ctx, instance, prompt)
	return err
}

// StartAndSendPromptWithConversationCapture is the daemon create path. It
// separates provisioning from launch so a local backend can snapshot the exact
// command-specific provider store after the final cwd exists in the model but
// before the agent process can create a transcript there.
func StartAndSendPromptWithConversationCapture(
	ctx context.Context,
	instance *session.Instance,
	prompt string,
) (session.ConversationCaptureSnapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	plan, err := instance.PrepareCreateLaunch()
	if err != nil {
		return session.ConversationCaptureSnapshot{}, err
	}
	capture := plan.ConversationCapture()
	if err := instance.LaunchPreparedCreate(plan); err != nil {
		return capture, err
	}
	return capture, WaitForReadyAndSendPrompt(ctx, instance, prompt)
}

// WaitForReadyAndSendPrompt is the post-launch half of StartAndSendPrompt. A
// handoff has already launched its incoming pane, but it owes the same readiness
// and trust-dialog contract as a fresh create before any mission text is typed.
// Keeping that contract here prevents the two launch paths from drifting.
func WaitForReadyAndSendPrompt(ctx context.Context, instance *session.Instance, prompt string) error {
	if ctx == nil {
		ctx = context.Background()
	}

	// Readiness polling, trust-prompt dismissal and prompt delivery below all
	// drive the agent's PTY locally; a backend without interactive input (remote
	// hook) handles readiness and prompts on its own host, so skip them.
	if !instance.Capabilities().InteractiveInput {
		return nil
	}

	if err := WaitForReady(ctx, instance); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentReadiness, err)
	}

	if err := DismissTrustPrompt(ctx, instanceTrustTarget{instanceReadinessTarget{inst: instance}}); err != nil {
		return fmt.Errorf("%w: %w", ErrAgentReadiness, err)
	}

	if prompt != "" {
		if err := instance.AgentServer().SendPrompt(prompt); err != nil {
			return fmt.Errorf("%w: %w", ErrPromptDelivery, err)
		}
	}

	return nil
}
