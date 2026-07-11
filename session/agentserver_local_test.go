package session

import (
	"strings"
	"testing"
)

// probeBackend records which Backend methods the local agent-server calls and
// returns recognizable values, so the tests below pin that localAgentServer
// routes each AgentServer method to the right underlying Backend call (#1592
// Phase 2 PR4). It embeds FakeBackend for the methods it does not care about.
type probeBackend struct {
	*FakeBackend

	trustChecked   bool
	tapped         bool
	provisionFirst *bool
	launchFirst    *bool
	sentPrompt     string
	killed         bool
}

func (b *probeBackend) CheckAndHandleTrustPrompt(*Instance) bool {
	b.trustChecked = true
	return false
}

// HasUpdated returns a distinct triple so Snapshot's field mapping is verifiable.
func (b *probeBackend) HasUpdated(*Instance) (bool, bool, string) {
	return true, true, "captured-pane"
}

func (b *probeBackend) TapEnter(*Instance) { b.tapped = true }

func (b *probeBackend) IsAlive(*Instance) bool { return false }

func (b *probeBackend) Preview(*Instance) (string, error) { return "short", nil }

func (b *probeBackend) PreviewFullHistory(*Instance) (string, error) { return "full", nil }

func (b *probeBackend) SendPromptCommand(_ *Instance, prompt string) error {
	b.sentPrompt = prompt
	return nil
}

func (b *probeBackend) Provision(_ *Instance, firstTimeSetup bool) error {
	b.provisionFirst = &firstTimeSetup
	return nil
}

func (b *probeBackend) Launch(_ *Instance, firstTimeSetup bool) error {
	b.launchFirst = &firstTimeSetup
	return nil
}

func (b *probeBackend) Kill(*Instance) error {
	b.killed = true
	return nil
}

func newProbeInstance(t *testing.T) (*Instance, *probeBackend) {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "probe", Path: t.TempDir(), Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	b := &probeBackend{FakeBackend: NewFakeBackend()}
	inst.SetBackend(b)
	return inst, b
}

func TestLocalAgentServerSnapshot(t *testing.T) {
	inst, b := newProbeInstance(t)
	obs, err := inst.AgentServer().Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if !b.trustChecked {
		t.Error("Snapshot must dismiss a pending trust prompt (CheckAndHandleTrustPrompt) first")
	}
	if !obs.Updated || !obs.HasPrompt || obs.Content != "captured-pane" {
		t.Errorf("Observation mismapped: got %+v", obs)
	}
}

func TestLocalAgentServerTapEnter(t *testing.T) {
	inst, b := newProbeInstance(t)
	inst.AgentServer().TapEnter()
	if !b.tapped {
		t.Error("TapEnter must route to Backend.TapEnter")
	}
}

func TestLocalAgentServerAlive(t *testing.T) {
	inst, _ := newProbeInstance(t)
	if inst.AgentServer().Alive() {
		t.Error("Alive must route to Backend.IsAlive (probe returns false)")
	}
}

func TestLocalAgentServerPreview(t *testing.T) {
	inst, _ := newProbeInstance(t)
	as := inst.AgentServer()
	if got, _ := as.Preview(false); got != "short" {
		t.Errorf("Preview(false) = %q, want the visible-pane capture", got)
	}
	if got, _ := as.Preview(true); got != "full" {
		t.Errorf("Preview(true) = %q, want the full scrollback", got)
	}
}

func TestLocalAgentServerSendPrompt(t *testing.T) {
	inst, b := newProbeInstance(t)
	if err := inst.AgentServer().SendPrompt("hello"); err != nil {
		t.Fatalf("SendPrompt: %v", err)
	}
	if b.sentPrompt != "hello" {
		t.Errorf("SendPrompt must route to the reliable command path; got %q", b.sentPrompt)
	}
}

func TestLocalAgentServerLifecycle(t *testing.T) {
	inst, b := newProbeInstance(t)
	as := inst.AgentServer()
	if err := as.Provision(true); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if b.provisionFirst == nil || *b.provisionFirst != true {
		t.Error("Provision must forward firstTimeSetup to Backend.Provision")
	}
	if err := as.Launch(false); err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if b.launchFirst == nil || *b.launchFirst != false {
		t.Error("Launch must forward firstTimeSetup to Backend.Launch")
	}
	if err := as.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !b.killed {
		t.Error("Kill must route to Backend.Kill")
	}
}

func TestLocalAgentServerExpose(t *testing.T) {
	inst, _ := newProbeInstance(t)
	ep, err := inst.AgentServer().Expose()
	if err != nil {
		t.Fatalf("Expose: %v", err)
	}
	if !ep.Local || ep.URL != "" {
		t.Errorf("local endpoint = %+v, want {Local:true, URL:\"\"}", ep)
	}
}

// TestLocalAgentServerDataPlaneNoLocalPTY pins that the wired data plane
// (#1592 PR5) degrades gracefully when the instance has no live tmux pane to
// stream — a probe instance is never started — rather than panicking: every
// data-plane method returns a "no local PTY" error. The ring/fan-out behaviour
// with a live channel is covered by TestPTYBroker* in ptybroker_test.go.
func TestLocalAgentServerDataPlaneNoLocalPTY(t *testing.T) {
	inst, _ := newProbeInstance(t)
	as := inst.AgentServer()
	if _, err := as.Subscribe(0); err == nil || !strings.Contains(err.Error(), "no local PTY") {
		t.Errorf("Subscribe err = %v, want a no-local-PTY error", err)
	}
	if err := as.Input([]byte("x")); err == nil || !strings.Contains(err.Error(), "no local PTY") {
		t.Errorf("Input err = %v, want a no-local-PTY error", err)
	}
	if err := as.Resize(24, 80); err == nil || !strings.Contains(err.Error(), "no local PTY") {
		t.Errorf("Resize err = %v, want a no-local-PTY error", err)
	}
}

// TestLocalAgentServerCached pins the PR5 caching invariant: AgentServer returns
// the SAME instance each call so the data-plane ring buffer and subscribers
// persist (a fresh server per call would drop them).
func TestLocalAgentServerCached(t *testing.T) {
	inst, _ := newProbeInstance(t)
	if inst.AgentServer() != inst.AgentServer() {
		t.Error("AgentServer() must return the cached per-instance singleton")
	}
}
