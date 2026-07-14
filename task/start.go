package task

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

const maxTrustPromptAttempts = 20

var trustPromptRetryDelay = time.Second

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
	if ctx == nil {
		ctx = context.Background()
	}
	if err := instance.Start(true); err != nil {
		return err
	}

	// Readiness polling, trust-prompt dismissal and prompt delivery below all
	// drive the agent's PTY locally; a backend without interactive input (remote
	// hook) handles readiness and prompts on its own host, so skip them.
	if !instance.Capabilities().InteractiveInput {
		return nil
	}

	if err := WaitForReady(ctx, instance); err != nil {
		return err
	}

	for attempts := 0; instance.CheckAndHandleTrustPrompt(); attempts++ {
		if attempts+1 >= maxTrustPromptAttempts {
			return fmt.Errorf("trust prompt did not dismiss after %d attempts", maxTrustPromptAttempts)
		}
		// Honor cancellation while backing off between trust-prompt retries so an
		// abandoned create doesn't sit here sleeping and re-waiting.
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(trustPromptRetryDelay):
		}
		if err := WaitForReady(ctx, instance); err != nil {
			return err
		}
	}

	if prompt != "" {
		if err := instance.AgentServer().SendPrompt(prompt); err != nil {
			return err
		}
	}

	return nil
}
