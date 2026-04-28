package task

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

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
func StartAndSendPrompt(instance *session.Instance, prompt string) error {
	if err := instance.Start(true); err != nil {
		return err
	}

	// Remote sessions handle readiness and prompts on the remote host.
	if instance.IsRemote() {
		return nil
	}

	if err := WaitForReady(instance); err != nil {
		return err
	}

	for instance.CheckAndHandleTrustPrompt() {
		time.Sleep(1 * time.Second)
		if err := WaitForReady(instance); err != nil {
			return err
		}
	}

	if prompt != "" {
		if err := instance.SendPromptCommand(prompt); err != nil {
			return err
		}
	}

	return nil
}
