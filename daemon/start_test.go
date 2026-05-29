package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// startTestBackend is a minimal Backend that records whether trust-prompt
// handling and prompt-send ran, so we can assert startAndSendPrompt's
// behaviour independent of any real tmux/PTY.
type startTestBackend struct {
	trustChecks int
	sentPrompt  string
	promptSent  bool
}

func (b *startTestBackend) Start(instance *session.Instance, _ bool) error {
	instance.SetStartedForTest(true)
	return nil
}

func (b *startTestBackend) Kill(instance *session.Instance) error {
	instance.SetStartedForTest(false)
	return nil
}

func (b *startTestBackend) Preview(*session.Instance) (string, error) {
	return "ready\n❯", nil
}

func (b *startTestBackend) PreviewFullHistory(*session.Instance) (string, error) {
	return "ready\n❯", nil
}

func (b *startTestBackend) Attach(*session.Instance) (chan struct{}, error) {
	ch := make(chan struct{})
	close(ch)
	return ch, nil
}

func (b *startTestBackend) HasUpdated(*session.Instance) (bool, bool)  { return false, false }
func (b *startTestBackend) SendPrompt(*session.Instance, string) error { return nil }
func (b *startTestBackend) SendPromptCommand(_ *session.Instance, prompt string) error {
	b.promptSent = true
	b.sentPrompt = prompt
	return nil
}
func (b *startTestBackend) SendKeys(*session.Instance, string) error         { return nil }
func (b *startTestBackend) SetPreviewSize(*session.Instance, int, int) error { return nil }
func (b *startTestBackend) IsAlive(*session.Instance) bool                   { return true }
func (b *startTestBackend) CheckAndHandleTrustPrompt(*session.Instance) bool {
	b.trustChecks++
	return false
}
func (b *startTestBackend) TapEnter(*session.Instance) {}
func (b *startTestBackend) Type() string               { return "local" }

func newStartTestInstance(t *testing.T, backend session.Backend) *session.Instance {
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

// TestStartAndSendPrompt_EmptyPromptStillHandlesTrust is the regression test
// for #698: a session created without an initial prompt must still wait for
// readiness and dismiss trust prompts, even though no prompt is sent.
func TestStartAndSendPrompt_EmptyPromptStillHandlesTrust(t *testing.T) {
	backend := &startTestBackend{}
	if err := startAndSendPrompt(newStartTestInstance(t, backend), ""); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.trustChecks == 0 {
		t.Fatalf("trust prompt handling must run for empty-prompt sessions (#698)")
	}
	if backend.promptSent {
		t.Fatalf("no prompt should be sent for empty prompt, got %q", backend.sentPrompt)
	}
}

// TestStartAndSendPrompt_NonEmptyPromptSends confirms the prompt is still sent
// (after trust handling) when one is provided.
func TestStartAndSendPrompt_NonEmptyPromptSends(t *testing.T) {
	backend := &startTestBackend{}
	if err := startAndSendPrompt(newStartTestInstance(t, backend), "do work"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if backend.trustChecks == 0 {
		t.Fatalf("trust prompt handling must run before sending the prompt")
	}
	if backend.sentPrompt != "do work" {
		t.Fatalf("expected prompt to be sent, got %q", backend.sentPrompt)
	}
}
