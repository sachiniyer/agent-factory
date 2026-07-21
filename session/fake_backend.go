package session

import (
	"sync"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// FakeBackend is a Backend implementation for tests that need to drive the
// creation flow without spawning real tmux sessions or git worktrees.
//
// The only method with nontrivial behavior is Start: it blocks until the
// test calls CompleteStart or FailStart, so tests can observe the app state
// while an instance is mid-creation (e.g. send navigation keys before the
// instance is marked Running). All other methods are safe no-ops so the
// preview/metadata ticks don't crash when they sweep the sidebar.
//
// Exported (in the session package rather than a _test.go file) so that
// app/ e2e tests can reach it via session.NewFakeBackend.
type FakeBackend struct {
	mu sync.Mutex

	// startCalled is closed the first time Start is invoked. Tests use
	// WaitForStart to synchronise with the creation goroutine.
	startCalled chan struct{}
	// startBlock is closed by CompleteStart/FailStart to let Start return.
	startBlock chan struct{}
	// startErr is the error returned from Start; nil means success.
	startErr error
	// startCount tracks how many times Start was invoked.
	startCount int
}

// NewFakeBackend returns a FakeBackend with its Start call pre-armed to
// block. Tests must arrange for CompleteStart/FailStart to be invoked,
// otherwise the creation goroutine will hang forever.
func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		startCalled: make(chan struct{}),
		startBlock:  make(chan struct{}),
	}
}

// StartCalled returns a channel that is closed when Start is first invoked.
func (b *FakeBackend) StartCalled() <-chan struct{} {
	return b.startCalled
}

// CompleteStart releases a blocked Start with no error.
func (b *FakeBackend) CompleteStart() {
	b.mu.Lock()
	b.startErr = nil
	b.mu.Unlock()
	close(b.startBlock)
}

// FailStart releases a blocked Start with the given error.
func (b *FakeBackend) FailStart(err error) {
	b.mu.Lock()
	b.startErr = err
	b.mu.Unlock()
	close(b.startBlock)
}

// -- Backend interface implementation --

func (b *FakeBackend) Start(instance *Instance, firstTimeSetup bool) error {
	if err := b.Provision(instance, firstTimeSetup); err != nil {
		return err
	}
	return b.Launch(instance, firstTimeSetup)
}

// Provision is a no-op for the fake backend: it holds no workspace. The blocking
// start semantics (startCalled/startBlock, startErr, SetStartedForTest) live in
// Launch so Start = Provision then Launch matches the real backends (#1592 Phase
// 2 PR4).
func (b *FakeBackend) Provision(*Instance, bool) error { return nil }

func (b *FakeBackend) Launch(instance *Instance, _ bool) error {
	b.mu.Lock()
	b.startCount++
	first := b.startCount == 1
	b.mu.Unlock()
	if first {
		close(b.startCalled)
	}
	<-b.startBlock

	b.mu.Lock()
	err := b.startErr
	b.mu.Unlock()
	if err != nil {
		return err
	}
	instance.SetStartedForTest(true)
	return nil
}

func (b *FakeBackend) Kill(instance *Instance) error {
	instance.SetStartedForTest(false)
	return nil
}

// CloseAttachOnly is a no-op for the fake backend: it holds no real PTY to
// release. Tests that need to distinguish a non-destructive close from a Kill
// embed FakeBackend and override this (and Kill) to record the call.
func (b *FakeBackend) CloseAttachOnly(*Instance) error { return nil }

func (b *FakeBackend) Preview(*Instance) (string, error)            { return "", nil }
func (b *FakeBackend) PreviewFullHistory(*Instance) (string, error) { return "", nil }
func (b *FakeBackend) HasUpdated(*Instance) (bool, bool, string)    { return false, false, "" }
func (b *FakeBackend) SendPromptCommand(*Instance, string) error    { return nil }
func (b *FakeBackend) IsAlive(*Instance) (bool, error)              { return true, nil }
func (b *FakeBackend) CheckAndHandleTrustPrompt(*Instance) bool     { return false }
func (b *FakeBackend) AgentModelChange(*Instance) *AgentModelChange { return nil }
func (b *FakeBackend) Recover(*Instance) error                      { return nil }
func (b *FakeBackend) Respawn(*Instance) error                      { return nil }
func (b *FakeBackend) PrepareAgentSwap(_ *Instance, target string) (AgentSwapPlan, error) {
	plan := AgentSwapPlan{target: target, program: target}
	if target == tmux.ProgramCodex {
		plan.conversationCapture = BeginConversationCapture()
	}
	return plan, nil
}
func (b *FakeBackend) SwapAgent(*Instance, AgentSwapPlan) error { return nil }
func (b *FakeBackend) Type() string                             { return "local" }

// Capabilities reports local full parity by default, mirroring LocalBackend so
// the fake stands in for a local session (#1592 Phase 1). Test doubles that
// impersonate a remote backend override this to return a WorkspaceRemote
// descriptor.
func (b *FakeBackend) Capabilities() Capabilities {
	return Capabilities{
		Workspace:        WorkspaceLocalWorktree,
		Archive:          true,
		Recover:          true,
		TabManagement:    true,
		TerminalTab:      true,
		InteractiveInput: true,
		Handoff:          true,
	}
}
