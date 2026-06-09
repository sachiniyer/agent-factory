package task

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

type startBackend struct {
	trustPrompts int
	trustChecks  int
	sentPrompt   string
}

func (b *startBackend) Start(instance *session.Instance, _ bool) error {
	instance.SetStartedForTest(true)
	return nil
}

func (b *startBackend) Kill(instance *session.Instance) error {
	instance.SetStartedForTest(false)
	return nil
}

func (b *startBackend) Preview(*session.Instance) (string, error) {
	return "ready\n❯", nil
}

func (b *startBackend) PreviewFullHistory(*session.Instance) (string, error) {
	return "ready\n❯", nil
}

func (b *startBackend) Attach(*session.Instance) (chan struct{}, error) {
	ch := make(chan struct{})
	close(ch)
	return ch, nil
}

func (b *startBackend) HasUpdated(*session.Instance) (bool, bool)  { return false, false }
func (b *startBackend) SendPrompt(*session.Instance, string) error { return nil }
func (b *startBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.sentPrompt = prompt
	return nil
}
func (b *startBackend) SendKeys(*session.Instance, string) error         { return nil }
func (b *startBackend) SetPreviewSize(*session.Instance, int, int) error { return nil }
func (b *startBackend) IsAlive(*session.Instance) bool                   { return true }
func (b *startBackend) CheckAndHandleTrustPrompt(*session.Instance) bool {
	b.trustChecks++
	if b.trustPrompts <= 0 {
		return false
	}
	b.trustPrompts--
	return true
}
func (b *startBackend) TapEnter(*session.Instance) {}
func (b *startBackend) Type() string               { return "local" }

func newStartTestInstance(t *testing.T, backend *startBackend) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   "start-test",
		Path:    t.TempDir(),
		Program: "claude",
	})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(backend)
	return inst
}

func TestStartAndSendPrompt_BoundsPersistentTrustPrompt(t *testing.T) {
	oldDelay := trustPromptRetryDelay
	oldPoll := waitForReadyPollInterval
	trustPromptRetryDelay = 0
	waitForReadyPollInterval = time.Nanosecond
	t.Cleanup(func() {
		trustPromptRetryDelay = oldDelay
		waitForReadyPollInterval = oldPoll
	})

	backend := &startBackend{trustPrompts: maxTrustPromptAttempts + 5}
	err := StartAndSendPrompt(newStartTestInstance(t, backend), "do work")
	if err == nil {
		t.Fatalf("expected persistent trust prompt error")
	}
	if !strings.Contains(err.Error(), "trust prompt did not dismiss") {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.trustChecks != maxTrustPromptAttempts {
		t.Fatalf("expected %d trust checks, got %d", maxTrustPromptAttempts, backend.trustChecks)
	}
	if backend.sentPrompt != "" {
		t.Fatalf("prompt should not be sent after trust prompt failure, got %q", backend.sentPrompt)
	}
}

// TestStartAndSendPrompt_EmptyPromptStillHandlesTrust is the regression test
// for #698: a session created without an initial prompt must still wait for
// readiness and dismiss trust prompts, even though no prompt is sent. This
// also covers the daemon's CreateSession path, which delegates here (#782).
func TestStartAndSendPrompt_EmptyPromptStillHandlesTrust(t *testing.T) {
	backend := &startBackend{}
	if err := StartAndSendPrompt(newStartTestInstance(t, backend), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.trustChecks == 0 {
		t.Fatalf("trust prompt handling must run for empty-prompt sessions (#698)")
	}
	if backend.sentPrompt != "" {
		t.Fatalf("no prompt should be sent for empty prompt, got %q", backend.sentPrompt)
	}
}

func TestStartAndSendPrompt_NonEmptyPromptSends(t *testing.T) {
	backend := &startBackend{}
	if err := StartAndSendPrompt(newStartTestInstance(t, backend), "do work"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.trustChecks == 0 {
		t.Fatalf("trust prompt handling must run before sending the prompt")
	}
	if backend.sentPrompt != "do work" {
		t.Fatalf("expected prompt to be sent, got %q", backend.sentPrompt)
	}
}

func TestStartAndSendPrompt_AllowsSequentialTrustPrompts(t *testing.T) {
	oldDelay := trustPromptRetryDelay
	oldPoll := waitForReadyPollInterval
	trustPromptRetryDelay = time.Nanosecond
	waitForReadyPollInterval = time.Nanosecond
	t.Cleanup(func() {
		trustPromptRetryDelay = oldDelay
		waitForReadyPollInterval = oldPoll
	})

	backend := &startBackend{trustPrompts: 3}
	err := StartAndSendPrompt(newStartTestInstance(t, backend), "do work")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.sentPrompt != "do work" {
		t.Fatalf("expected prompt to be sent after trust prompts clear, got %q", backend.sentPrompt)
	}
}
